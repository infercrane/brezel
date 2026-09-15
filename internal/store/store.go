package store

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/infercrane/brezel/internal/domain"
)

var (
	ErrNotFound = errors.New("resource not found")
	ErrConflict = errors.New("resource already exists")
)

const maxStateBytes = 128 << 20

const CurrentSchemaVersion = 1

type State struct {
	SchemaVersion int                           `json:"schema_version"`
	Environments  map[string]domain.Environment `json:"environments"`
	Sandboxes     map[string]domain.Sandbox     `json:"sandboxes"`
	Operations    map[string]domain.Operation   `json:"operations"`
	Checkpoints   map[string]domain.Checkpoint  `json:"checkpoints"`
	Workspaces    map[string]domain.Workspace   `json:"workspaces"`
	Connectors    map[string]domain.Connector   `json:"connectors"`
	Events        []domain.Event                `json:"events"`
	Idempotency   map[string]string             `json:"idempotency"`
	// IdempotencyDigests binds each key to the canonical request payload. Old
	// state files may omit this map and remain readable during upgrade.
	IdempotencyDigests map[string]string `json:"idempotency_digests"`
}

func NewState() State {
	return State{
		SchemaVersion:      CurrentSchemaVersion,
		Environments:       map[string]domain.Environment{},
		Sandboxes:          map[string]domain.Sandbox{},
		Operations:         map[string]domain.Operation{},
		Checkpoints:        map[string]domain.Checkpoint{},
		Workspaces:         map[string]domain.Workspace{},
		Connectors:         map[string]domain.Connector{},
		Events:             []domain.Event{},
		Idempotency:        map[string]string{},
		IdempotencyDigests: map[string]string{},
	}
}

func ScopedKey(projectID, resourceID string) string {
	return projectID + "\x00" + resourceID
}

func IdempotencyKey(projectID, kind, key string) string {
	return projectID + "\x00" + kind + "\x00" + key
}

type Store interface {
	View(func(State) error) error
	Update(func(*State) error) error
}

// MutationScope declares the exact ledger rows a callback may change. Resource
// keys are the same project-scoped keys used by State maps. EventStreams make
// only the current stream tail visible, which is sufficient for appendEvent
// style mutations while keeping work independent of the stream's history.
type MutationScope struct {
	Environments []string
	Sandboxes    []string
	Operations   []string
	Checkpoints  []string
	Workspaces   []string
	Connectors   []string
	Idempotency  []string
	EventStreams []EventStream
}

type EventStream struct {
	ProjectID  string
	ResourceID string
}

// RowScopedUpdater is an optional extension for transactional mutations whose
// read/write set is known in advance. Implementations must reject changes
// outside scope and commit the declared resource, idempotency, and event rows
// atomically.
type RowScopedUpdater interface {
	UpdateRows(MutationScope, func(*State) error) error
}

// UpdateRows uses a row-scoped transaction when the store supports one. The
// FileStore fallback deliberately retains its existing whole-state atomic
// replacement semantics.
func UpdateRows(target Store, scope MutationScope, fn func(*State) error) error {
	if updater, ok := target.(RowScopedUpdater); ok {
		return updater.UpdateRows(scope, fn)
	}
	return target.Update(fn)
}

// SandboxReader is an optional read-optimized extension implemented by stores
// that can return one sandbox without copying their entire control state.
// Callers must still treat the returned value as an isolated snapshot.
type SandboxReader interface {
	GetSandbox(projectID, sandboxID string) (domain.Sandbox, error)
}

// SandboxReconcileReader is an optional read-optimized extension for the
// controller's periodic recovery scan. Implementations return only
// non-terminal sandboxes and must not materialize unrelated operation or event
// history. Each value is an isolated snapshot; lifecycle code must re-read and
// validate the revision before applying an observation.
type SandboxReconcileReader interface {
	ListSandboxesForReconcile() ([]domain.Sandbox, error)
}

// WorkspaceReconcileReader is the workspace counterpart to
// SandboxReconcileReader. Ready and terminal workspaces do not require a
// periodic recovery observation.
type WorkspaceReconcileReader interface {
	ListWorkspacesForReconcile() ([]domain.Workspace, error)
}

// LifecycleOperationReconcileReader returns the small operation subset needed
// to recover interrupted sandbox and workspace lifecycle transitions. A
// materialized implementation may omit a succeeded family head, but it must
// preserve that head as a suppressor so an older incomplete attempt is never
// returned as current recovery work.
type LifecycleOperationReconcileReader interface {
	ListLifecycleOperationsForReconcile() ([]domain.Operation, error)
}

// ListSandboxesForReconcile uses the narrow recovery scan when the store
// supports one. The whole-state fallback keeps third-party Store
// implementations source compatible.
func ListSandboxesForReconcile(target Store) ([]domain.Sandbox, error) {
	if reader, ok := target.(SandboxReconcileReader); ok {
		return reader.ListSandboxesForReconcile()
	}
	var sandboxes []domain.Sandbox
	err := target.View(func(state State) error {
		for _, sandbox := range state.Sandboxes {
			if sandbox.State == domain.SandboxDeleted || sandbox.State == domain.SandboxExpired || sandbox.State == domain.SandboxFailed {
				continue
			}
			sandboxes = append(sandboxes, sandbox)
		}
		return nil
	})
	return sandboxes, err
}

func ListWorkspacesForReconcile(target Store) ([]domain.Workspace, error) {
	if reader, ok := target.(WorkspaceReconcileReader); ok {
		return reader.ListWorkspacesForReconcile()
	}
	var workspaces []domain.Workspace
	err := target.View(func(state State) error {
		for _, workspace := range state.Workspaces {
			if workspace.State == domain.WorkspaceReady || workspace.State == domain.WorkspaceDeleted || workspace.State == domain.WorkspaceFailed {
				continue
			}
			workspaces = append(workspaces, workspace)
		}
		return nil
	})
	return workspaces, err
}

func ListLifecycleOperationsForReconcile(target Store) ([]domain.Operation, error) {
	if reader, ok := target.(LifecycleOperationReconcileReader); ok {
		return reader.ListLifecycleOperationsForReconcile()
	}
	var operations []domain.Operation
	err := target.View(func(state State) error {
		for _, operation := range state.Operations {
			if isReconciledLifecycleOperation(operation) {
				operations = append(operations, operation)
			}
		}
		return nil
	})
	return operations, err
}

func isReconciledLifecycleOperation(operation domain.Operation) bool {
	_, lifecycle := lifecycleHeadForOperation(operation)
	return lifecycle
}

// SandboxEventAppender is an optional write-optimized extension for durable,
// content-free sandbox events. It preserves FileStore's atomic replacement and
// fsync boundary without cloning and validating unrelated control records.
type SandboxEventAppender interface {
	AppendSandboxEvent(event domain.Event) error
}

// SandboxActivityRecorder is an optional write-optimized extension for the
// guest-operation hot path. Implementations must advance activity
// monotonically and durably before returning the updated sandbox.
type SandboxActivityRecorder interface {
	RecordSandboxActivity(projectID, sandboxID string, at time.Time) (domain.Sandbox, error)
}

type FileStore struct {
	mu       sync.RWMutex
	path     string
	state    State
	lockFile *os.File
	closed   bool
}

func OpenFile(path string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("state file path is required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	directoryInfo, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("inspect state directory: %w", err)
	}
	if !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("state directory must be a real directory")
	}
	if directoryInfo.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("state directory permissions must not allow group or other writes")
	}
	lockPath := path + ".lock"
	if info, err := os.Lstat(lockPath); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("state lock must be a private regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect state lock: %w", err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lockFile.Close()
		return nil, errors.New("state file is already open by another runtime process")
	}
	s := &FileStore{path: path, state: NewState(), lockFile: lockFile}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := s.persist(s.state); err != nil {
			s.Close()
			return nil, err
		}
		return s, nil
	}
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("inspect state: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		s.Close()
		return nil, errors.New("state file must be a private regular file")
	}
	if info.Size() > maxStateBytes {
		s.Close()
		return nil, errors.New("state file exceeds 128 MiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("read state: %w", err)
	}
	// Decode into a zero value so an omitted schema_version is distinguishable
	// from the current version during legacy migration.
	s.state = State{}
	if err := json.Unmarshal(data, &s.state); err != nil {
		s.Close()
		return nil, fmt.Errorf("decode state: %w", err)
	}
	legacy := s.state.SchemaVersion == 0
	s.ensureMaps()
	if legacy {
		s.state.SchemaVersion = CurrentSchemaVersion
	}
	if err := validateState(s.state); err != nil {
		s.Close()
		return nil, fmt.Errorf("validate state: %w", err)
	}
	if legacy {
		if err := s.persist(s.state); err != nil {
			s.Close()
			return nil, fmt.Errorf("migrate legacy state: %w", err)
		}
	}
	return s, nil
}

// Close releases the exclusive process lock. A running runtime must keep the
// store open for its whole lifetime.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.lockFile == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(s.lockFile.Fd()), syscall.LOCK_UN)
	closeErr := s.lockFile.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// Ready proves that the process still owns the store and that the durable file
// retains its required type and permissions. OpenFile performs an actual
// atomic write, so a successful open plus this check is a meaningful readiness
// signal rather than a constant response.
func (s *FileStore) Ready() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.lockFile == nil {
		return errors.New("state store is closed")
	}
	info, err := os.Lstat(s.path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("state file is not a private regular file")
	}
	return nil
}

func (s *FileStore) View(fn func(State) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return errors.New("state store is closed")
	}
	clone, err := cloneState(s.state)
	if err != nil {
		return err
	}
	return fn(clone)
}

// GetSandbox returns a deep copy of one sandbox. Keeping this operation keyed
// avoids an O(total state) clone on guest-command authorization paths while
// retaining the no-alias guarantee provided by View.
func (s *FileStore) GetSandbox(projectID, sandboxID string) (domain.Sandbox, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return domain.Sandbox{}, errors.New("state store is closed")
	}
	sandbox, ok := s.state.Sandboxes[ScopedKey(projectID, sandboxID)]
	if !ok {
		return domain.Sandbox{}, ErrNotFound
	}
	return cloneSandbox(sandbox), nil
}

// ListSandboxesForReconcile copies only live sandbox records. In particular it
// avoids cloning the event ledger on every controller tick.
func (s *FileStore) ListSandboxesForReconcile() ([]domain.Sandbox, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.New("state store is closed")
	}
	sandboxes := make([]domain.Sandbox, 0)
	for _, sandbox := range s.state.Sandboxes {
		if sandbox.State == domain.SandboxDeleted || sandbox.State == domain.SandboxExpired || sandbox.State == domain.SandboxFailed {
			continue
		}
		sandboxes = append(sandboxes, cloneSandbox(sandbox))
	}
	return sandboxes, nil
}

func (s *FileStore) ListWorkspacesForReconcile() ([]domain.Workspace, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.New("state store is closed")
	}
	workspaces := make([]domain.Workspace, 0)
	for _, workspace := range s.state.Workspaces {
		if workspace.State == domain.WorkspaceReady || workspace.State == domain.WorkspaceDeleted || workspace.State == domain.WorkspaceFailed {
			continue
		}
		workspaces = append(workspaces, workspace)
	}
	return workspaces, nil
}

func (s *FileStore) ListLifecycleOperationsForReconcile() ([]domain.Operation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.New("state store is closed")
	}
	operations := make([]domain.Operation, 0)
	for _, operation := range s.state.Operations {
		if isReconciledLifecycleOperation(operation) {
			operations = append(operations, operation)
		}
	}
	return operations, nil
}

// AppendSandboxEvent durably appends one event for an existing sandbox. The
// current sandbox state and next per-sandbox sequence are assigned while the
// store is locked, so callers cannot persist a stale state or race ordering.
// The complete state file is still atomically replaced and fsynced before the
// in-memory event becomes visible.
func (s *FileStore) AppendSandboxEvent(event domain.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("state store is closed")
	}
	current, ok := s.state.Sandboxes[ScopedKey(event.ProjectID, event.ResourceID)]
	if !ok {
		return ErrNotFound
	}
	details, err := cloneEventDetails(event.Details)
	if err != nil {
		return fmt.Errorf("validate event details: %w", err)
	}
	event.Details = details
	event.State = current.State
	event.Sequence = nextSandboxEventSequence(s.state.Events, event.ProjectID, event.ResourceID)
	if err := validateSandboxEvent(event); err != nil {
		return err
	}

	next := s.state
	next.Events = make([]domain.Event, len(s.state.Events)+1)
	copy(next.Events, s.state.Events)
	next.Events[len(s.state.Events)] = event
	if err := s.persist(next); err != nil {
		return err
	}
	s.state = next
	return nil
}

// RecordSandboxActivity avoids cloning the complete in-memory state while
// preserving FileStore's atomic replacement boundary. SQLiteStore implements
// the same contract as a single-row transaction.
func (s *FileStore) RecordSandboxActivity(projectID, sandboxID string, at time.Time) (domain.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return domain.Sandbox{}, errors.New("state store is closed")
	}
	key := ScopedKey(projectID, sandboxID)
	current, ok := s.state.Sandboxes[key]
	if !ok {
		return domain.Sandbox{}, ErrNotFound
	}
	nextSandbox := advanceSandboxActivity(current, at)
	if nextSandbox.Revision == current.Revision {
		return cloneSandbox(current), nil
	}
	next := s.state
	next.Sandboxes = maps.Clone(s.state.Sandboxes)
	next.Sandboxes[key] = nextSandbox
	if err := s.persist(next); err != nil {
		return domain.Sandbox{}, err
	}
	s.state = next
	return cloneSandbox(nextSandbox), nil
}

func (s *FileStore) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("state store is closed")
	}
	next, err := cloneState(s.state)
	if err != nil {
		return err
	}
	if err := fn(&next); err != nil {
		return err
	}
	if err := validateState(next); err != nil {
		return fmt.Errorf("validate updated state: %w", err)
	}
	if err := s.persist(next); err != nil {
		return err
	}
	s.state = next
	return nil
}

func (s *FileStore) persist(next State) error {
	data, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if len(data) > maxStateBytes {
		return errors.New("encoded state exceeds 128 MiB")
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".brezel-state-*")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open state directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}

func (s *FileStore) ensureMaps() {
	if s.state.Environments == nil {
		s.state.Environments = map[string]domain.Environment{}
	}
	if s.state.Sandboxes == nil {
		s.state.Sandboxes = map[string]domain.Sandbox{}
	}
	if s.state.Operations == nil {
		s.state.Operations = map[string]domain.Operation{}
	}
	if s.state.Checkpoints == nil {
		s.state.Checkpoints = map[string]domain.Checkpoint{}
	}
	if s.state.Workspaces == nil {
		s.state.Workspaces = map[string]domain.Workspace{}
	}
	if s.state.Connectors == nil {
		s.state.Connectors = map[string]domain.Connector{}
	}
	if s.state.Events == nil {
		s.state.Events = []domain.Event{}
	}
	if s.state.Idempotency == nil {
		s.state.Idempotency = map[string]string{}
	}
	if s.state.IdempotencyDigests == nil {
		s.state.IdempotencyDigests = map[string]string{}
	}
}

func cloneState(in State) (State, error) {
	out := State{
		SchemaVersion:      in.SchemaVersion,
		Environments:       maps.Clone(in.Environments),
		Sandboxes:          make(map[string]domain.Sandbox, len(in.Sandboxes)),
		Operations:         make(map[string]domain.Operation, len(in.Operations)),
		Checkpoints:        maps.Clone(in.Checkpoints),
		Workspaces:         make(map[string]domain.Workspace, len(in.Workspaces)),
		Connectors:         make(map[string]domain.Connector, len(in.Connectors)),
		Events:             make([]domain.Event, len(in.Events)),
		Idempotency:        maps.Clone(in.Idempotency),
		IdempotencyDigests: maps.Clone(in.IdempotencyDigests),
	}
	for key, sandbox := range in.Sandboxes {
		out.Sandboxes[key] = cloneSandbox(sandbox)
	}
	for key, operation := range in.Operations {
		operation.Failure = cloneFailure(operation.Failure)
		out.Operations[key] = operation
	}
	for key, workspace := range in.Workspaces {
		workspace.Failure = cloneFailure(workspace.Failure)
		out.Workspaces[key] = workspace
	}
	for key, connector := range in.Connectors {
		connector.AllowedMethods = slices.Clone(connector.AllowedMethods)
		connector.AllowedPaths = slices.Clone(connector.AllowedPaths)
		out.Connectors[key] = connector
	}
	for index, event := range in.Events {
		details, err := cloneEventDetails(event.Details)
		if err != nil {
			return State{}, err
		}
		event.Details = details
		out.Events[index] = event
	}
	return out, nil
}

func cloneSandbox(in domain.Sandbox) domain.Sandbox {
	in.Network.AllowOut = slices.Clone(in.Network.AllowOut)
	in.Network.DenyOut = slices.Clone(in.Network.DenyOut)
	in.ConnectorRevisions = slices.Clone(in.ConnectorRevisions)
	in.WorkspaceMounts = slices.Clone(in.WorkspaceMounts)
	in.Failure = cloneFailure(in.Failure)
	return in
}

func cloneFailure(in *domain.Failure) *domain.Failure {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

// Event details are an intentionally small, JSON-shaped extension point. Keep
// their JSON validation and normalization while copying the typed control
// records directly; this avoids encoding and decoding the entire state on
// every read and before every durable mutation.
func cloneEventDetails(in map[string]any) (map[string]any, error) {
	if in == nil {
		return nil, nil
	}
	data, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func nextSandboxEventSequence(events []domain.Event, projectID, resourceID string) int64 {
	var sequence int64 = 1
	for _, event := range events {
		if event.ProjectID == projectID && event.ResourceID == resourceID && event.Sequence >= sequence {
			sequence = event.Sequence + 1
		}
	}
	return sequence
}

func advanceSandboxActivity(current domain.Sandbox, at time.Time) domain.Sandbox {
	if current.State != domain.SandboxRunning && current.State != domain.SandboxStandby {
		return current
	}
	if at.Before(current.LastActiveAt) {
		at = current.LastActiveAt
	}
	current.UpdatedAt = at
	current.LastActiveAt = at
	current.Revision++
	if current.Lifecycle.StandbyAfterSeconds > 0 {
		seconds := current.Lifecycle.StandbyAfterSeconds + current.Lifecycle.StandbyGraceSeconds
		current.StandbyEligibleAt = at.Add(time.Duration(seconds) * time.Second)
	} else {
		current.StandbyEligibleAt = time.Time{}
	}
	return current
}

func validateSandboxEvent(event domain.Event) error {
	if err := domain.ValidateProjectID(event.ProjectID); err != nil {
		return errors.New("event has invalid project identity")
	}
	if event.ID == "" || strings.ContainsAny(event.ID, "\x00\r\n") {
		return errors.New("event has invalid identity")
	}
	if event.ResourceID == "" || strings.ContainsAny(event.ResourceID, "\x00\r\n") {
		return errors.New("event has invalid resource identity")
	}
	if event.OperationID != "" && strings.ContainsAny(event.OperationID, "\x00\r\n") {
		return errors.New("event has invalid operation identity")
	}
	if strings.TrimSpace(event.Type) == "" || strings.ContainsAny(event.Type, "\r\n") {
		return errors.New("event has invalid type")
	}
	if event.Sequence < 1 {
		return errors.New("event has invalid sequence")
	}
	if event.At.IsZero() {
		return errors.New("event has invalid timestamp")
	}
	return nil
}

func validateState(state State) error {
	if state.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("unsupported schema version %d (runtime supports %d)", state.SchemaVersion, CurrentSchemaVersion)
	}
	for key, value := range state.Environments {
		if err := requireScopedIdentity(key, value.ProjectID, value.RevisionID, "environment"); err != nil {
			return err
		}
	}
	for key, value := range state.Sandboxes {
		if err := requireScopedIdentity(key, value.ProjectID, value.ID, "sandbox"); err != nil {
			return err
		}
		hasNodeIdentity := value.NodeID != "" || value.NodeRouteID != "" || value.NodeGeneration != 0
		if hasNodeIdentity && (value.NodeID == "" || value.NodeRouteID == "" || value.NodeGeneration == 0) {
			return errors.New("sandbox contains an incomplete node assignment")
		}
		if strings.ContainsAny(value.NodeID+value.NodeRouteID, "\x00\r\n") {
			return errors.New("sandbox contains an invalid node assignment")
		}
	}
	for key, value := range state.Operations {
		if err := requireScopedIdentity(key, value.ProjectID, value.ID, "operation"); err != nil {
			return err
		}
	}
	for key, value := range state.Checkpoints {
		if err := requireScopedIdentity(key, value.ProjectID, value.ID, "checkpoint"); err != nil {
			return err
		}
	}
	for key, value := range state.Workspaces {
		if err := requireScopedIdentity(key, value.ProjectID, value.ID, "workspace"); err != nil {
			return err
		}
	}
	for key, value := range state.Connectors {
		if err := requireScopedIdentity(key, value.ProjectID, value.RevisionID, "connector"); err != nil {
			return err
		}
	}
	for key, operationID := range state.Idempotency {
		parts := strings.Split(key, "\x00")
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return errors.New("idempotency index contains an invalid key")
		}
		operation, ok := state.Operations[ScopedKey(parts[0], operationID)]
		if !ok || operation.ProjectID != parts[0] || operation.Kind != parts[1] || operation.IdempotencyKey != parts[2] {
			return errors.New("idempotency index refers to an inconsistent operation")
		}
	}
	for key, digest := range state.IdempotencyDigests {
		if _, ok := state.Idempotency[key]; !ok {
			return errors.New("idempotency digest has no matching operation index")
		}
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != 32 {
			return errors.New("idempotency digest is not a SHA-256 value")
		}
	}
	return nil
}

func requireScopedIdentity(key, projectID, resourceID, kind string) error {
	if err := domain.ValidateProjectID(projectID); err != nil {
		return fmt.Errorf("%s has invalid project identity", kind)
	}
	if resourceID == "" || strings.ContainsAny(resourceID, "\x00\r\n") {
		return fmt.Errorf("%s has invalid resource identity", kind)
	}
	if key != ScopedKey(projectID, resourceID) {
		return fmt.Errorf("%s key does not match its project and resource identity", kind)
	}
	return nil
}
