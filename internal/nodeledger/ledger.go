// Package nodeledger persists the node-local route generation boundary used by
// a relay to reject stale capabilities and stale engine assignments.
package nodeledger

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	CurrentSchemaVersion = 1
	maxLedgerBytes       = 16 << 20
	maxRoutes            = 65_536
	maxEngineIDBytes     = 1_024
)

var (
	ErrNotFound          = errors.New("node route not found")
	ErrConflict          = errors.New("node route already exists")
	ErrStaleGeneration   = errors.New("stale node route generation")
	ErrStateConflict     = errors.New("node route state conflict")
	ErrIllegalTransition = errors.New("illegal node route transition")
	ErrActiveOperations  = errors.New("node route has active operations")
	ErrClosed            = errors.New("node generation ledger is closed")
	ErrCorrupt           = errors.New("node generation ledger is corrupt")
	ErrInvalid           = errors.New("invalid node route")
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// State is the relay-facing lifecycle of one opaque route. Released is a
// tombstone state and never contains an engine identifier.
type State string

const (
	StateAttaching State = "attaching"
	StateReady     State = "ready"
	StateDraining  State = "draining"
	StateStandby   State = "standby"
	StateReleased  State = "released"
)

// Route is safe to expose to callers. Substrate identifiers are deliberately
// absent from this representation.
type Route struct {
	RouteID      string    `json:"route_id"`
	ProjectID    string    `json:"project_id"`
	SandboxID    string    `json:"sandbox_id"`
	Generation   uint64    `json:"generation"`
	State        State     `json:"state"`
	LastActivity time.Time `json:"last_activity"`
}

// Binding is a trusted node-local result. Its engine identifier is private and
// is redacted from JSON and formatted representations. Trusted relay code must
// opt in to reading it through EngineID.
type Binding struct {
	route    Route
	engineID string
}

func (b Binding) Public() Route { return b.route }

func (b Binding) EngineID() string { return b.engineID }

func (b Binding) MarshalJSON() ([]byte, error) { return json.Marshal(b.route) }

func (b Binding) String() string {
	return fmt.Sprintf("node route binding route=%q project=%q sandbox=%q generation=%d state=%q", b.route.RouteID, b.route.ProjectID, b.route.SandboxID, b.route.Generation, b.route.State)
}

func (b Binding) GoString() string { return b.String() }

type diskEntry struct {
	RouteID      string    `json:"route_id"`
	ProjectID    string    `json:"project_id"`
	SandboxID    string    `json:"sandbox_id"`
	EngineID     string    `json:"engine_id,omitempty"`
	Generation   uint64    `json:"generation"`
	State        State     `json:"state"`
	LastActivity time.Time `json:"last_activity"`
}

type diskState struct {
	SchemaVersion  int                  `json:"schema_version"`
	LastGeneration uint64               `json:"last_generation"`
	Routes         map[string]diskEntry `json:"routes"`
}

type diskEnvelope struct {
	State    diskState `json:"state"`
	Checksum string    `json:"checksum"`
}

func newDiskState() diskState {
	return diskState{SchemaVersion: CurrentSchemaVersion, Routes: map[string]diskEntry{}}
}

// Ledger is a single-process, crash-safe node generation ledger. The process
// must retain it for the complete lifetime of the relay.
type Ledger struct {
	mu               sync.RWMutex
	path             string
	state            diskState
	activeOperations map[operationKey]uint64
	lockFile         *os.File
	closed           bool
}

// operationKey binds a live data-path operation to the complete route
// assignment that admitted it. Generation is part of the identity so a lease
// for an old assignment can never protect or release work on a replacement.
type operationKey struct {
	routeID    string
	projectID  string
	sandboxID  string
	generation uint64
}

// OperationLease keeps one admitted data-path operation live while a route is
// draining. It is intentionally process-local: after a process exits, the
// operation it protected cannot still be executing through that process.
//
// Release is idempotent and safe to call concurrently.
type OperationLease struct {
	state *operationLeaseState
}

type operationLeaseState struct {
	ledger     *Ledger
	key        operationKey
	release    sync.Once
	releaseErr error
}

// Release ends an admitted operation. Callers should defer it immediately
// after a successful AcquireOperation.
func (lease *OperationLease) Release() error {
	if lease == nil || lease.state == nil || lease.state.ledger == nil {
		return ErrInvalid
	}
	lease.state.release.Do(func() {
		lease.state.releaseErr = lease.state.ledger.releaseOperation(lease.state.key)
	})
	return lease.state.releaseErr
}

// Open creates or opens a protected ledger and takes a non-blocking exclusive
// process lock. Corrupt or unverifiable state always fails closed.
func Open(path string) (*Ledger, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: ledger path is required", ErrInvalid)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create ledger directory: %w", err)
	}
	if err := validatePrivateDirectory(dir); err != nil {
		return nil, err
	}

	lockPath := path + ".lock"
	if info, err := os.Lstat(lockPath); err == nil {
		if err := validatePrivateFileInfo(info, "ledger lock"); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect ledger lock: %w", err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open ledger lock: %w", err)
	}
	closeLock := true
	defer func() {
		if closeLock {
			_ = lockFile.Close()
		}
	}()
	lockInfo, err := lockFile.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect open ledger lock: %w", err)
	}
	pathLockInfo, err := os.Lstat(lockPath)
	if err != nil || !os.SameFile(lockInfo, pathLockInfo) {
		return nil, errors.New("ledger lock changed while opening")
	}
	if err := validatePrivateFileInfo(lockInfo, "ledger lock"); err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.New("node generation ledger is already open by another process")
	}

	ledger := &Ledger{path: path, state: newDiskState(), activeOperations: make(map[operationKey]uint64), lockFile: lockFile}
	closeLock = false
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := ledger.persist(ledger.state); err != nil {
			_ = ledger.Close()
			return nil, err
		}
		return ledger, nil
	}
	if err != nil {
		_ = ledger.Close()
		return nil, fmt.Errorf("inspect node generation ledger: %w", err)
	}
	if err := validatePrivateFileInfo(info, "node generation ledger"); err != nil {
		_ = ledger.Close()
		return nil, err
	}
	if info.Size() <= 0 || info.Size() > maxLedgerBytes {
		_ = ledger.Close()
		return nil, fmt.Errorf("%w: encoded ledger size is invalid", ErrCorrupt)
	}
	data, err := readBounded(path, maxLedgerBytes)
	if err != nil {
		_ = ledger.Close()
		return nil, err
	}
	state, err := decodeAndValidate(data)
	if err != nil {
		_ = ledger.Close()
		return nil, err
	}
	ledger.state = state
	return ledger, nil
}

func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	if len(l.activeOperations) != 0 {
		return ErrActiveOperations
	}
	l.closed = true
	if l.lockFile == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.lockFile.Fd()), syscall.LOCK_UN)
	closeErr := l.lockFile.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// Ready confirms that the lock and durable path still have their required
// identity, ownership, type, and permissions.
func (l *Ledger) Ready() error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed || l.lockFile == nil {
		return ErrClosed
	}
	if err := validatePrivateDirectory(filepath.Dir(l.path)); err != nil {
		return err
	}
	info, err := os.Lstat(l.path)
	if err != nil {
		return err
	}
	if err := validatePrivateFileInfo(info, "node generation ledger"); err != nil {
		return err
	}
	lockInfo, err := l.lockFile.Stat()
	if err != nil {
		return err
	}
	pathLockInfo, err := os.Lstat(l.path + ".lock")
	if err != nil || !os.SameFile(lockInfo, pathLockInfo) {
		return errors.New("ledger lock identity changed")
	}
	return validatePrivateFileInfo(lockInfo, "ledger lock")
}

// Bind allocates a fresh globally monotonic generation for a new opaque route.
func (l *Ledger) Bind(routeID, projectID, sandboxID, engineID string, at time.Time) (Binding, error) {
	if err := validateRouteInput(routeID, projectID, sandboxID, engineID); err != nil {
		return Binding{}, err
	}
	at, err := normalizeActivity(at)
	if err != nil {
		return Binding{}, err
	}
	var result diskEntry
	err = l.update(func(next *diskState) error {
		if _, exists := next.Routes[routeID]; exists {
			return ErrConflict
		}
		if assignmentConflicts(next.Routes, "", projectID, sandboxID, engineID) {
			return ErrConflict
		}
		generation, err := nextGeneration(next.LastGeneration)
		if err != nil {
			return err
		}
		result = diskEntry{RouteID: routeID, ProjectID: projectID, SandboxID: sandboxID, EngineID: engineID, Generation: generation, State: StateAttaching, LastActivity: at}
		next.LastGeneration = generation
		next.Routes[routeID] = result
		return nil
	})
	if err != nil {
		return Binding{}, err
	}
	return bindingFromDisk(result), nil
}

// Rebind replaces the engine target of a standby route and increments the
// generation. A capability for the previous generation can never address the
// replacement target.
func (l *Ledger) Rebind(routeID string, expectedGeneration uint64, expectedState State, engineID string, at time.Time) (Binding, error) {
	if !safeID.MatchString(routeID) || validateEngineID(engineID) != nil {
		return Binding{}, ErrInvalid
	}
	if expectedState != StateStandby {
		return Binding{}, ErrIllegalTransition
	}
	at, err := normalizeActivity(at)
	if err != nil {
		return Binding{}, err
	}
	var result diskEntry
	err = l.update(func(next *diskState) error {
		current, ok := next.Routes[routeID]
		if !ok {
			return ErrNotFound
		}
		if current.Generation != expectedGeneration {
			return ErrStaleGeneration
		}
		if current.State != expectedState {
			return ErrStateConflict
		}
		if at.Before(current.LastActivity) {
			return fmt.Errorf("%w: activity time moved backwards", ErrInvalid)
		}
		if assignmentConflicts(next.Routes, routeID, current.ProjectID, current.SandboxID, engineID) {
			return ErrConflict
		}
		generation, err := nextGeneration(next.LastGeneration)
		if err != nil {
			return err
		}
		current.EngineID = engineID
		current.Generation = generation
		current.State = StateAttaching
		current.LastActivity = at
		next.LastGeneration = generation
		next.Routes[routeID] = current
		result = current
		return nil
	})
	if err != nil {
		return Binding{}, err
	}
	return bindingFromDisk(result), nil
}

// Transition applies a compare-and-swap lifecycle transition.
func (l *Ledger) Transition(routeID string, expectedGeneration uint64, expectedState, nextState State) (Binding, error) {
	if !safeID.MatchString(routeID) || !validState(expectedState) || !validState(nextState) {
		return Binding{}, ErrInvalid
	}
	var result diskEntry
	err := l.update(func(next *diskState) error {
		current, ok := next.Routes[routeID]
		if !ok {
			return ErrNotFound
		}
		if current.Generation != expectedGeneration {
			return ErrStaleGeneration
		}
		if current.State != expectedState {
			return ErrStateConflict
		}
		if !allowedTransition(expectedState, nextState) {
			return ErrIllegalTransition
		}
		if expectedState == StateDraining && (nextState == StateStandby || nextState == StateReleased) && l.hasActiveOperations(current) {
			return ErrActiveOperations
		}
		current.State = nextState
		if nextState == StateReleased {
			current.EngineID = ""
		}
		next.Routes[routeID] = current
		result = current
		return nil
	})
	if err != nil {
		return Binding{}, err
	}
	return bindingFromDisk(result), nil
}

// AcquireOperation admits one relay data-path operation against the current
// route assignment. Only Ready routes admit new work. A successfully acquired
// operation may finish after the route enters Draining, but it prevents the
// assignment from reaching Standby or Released until Release is called.
func (l *Ledger) AcquireOperation(routeID, projectID, sandboxID string, expectedGeneration uint64) (Binding, *OperationLease, error) {
	if err := validateRouteIdentity(routeID, projectID, sandboxID); err != nil || expectedGeneration == 0 {
		return Binding{}, nil, ErrInvalid
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return Binding{}, nil, ErrClosed
	}
	entry, ok := l.state.Routes[routeID]
	if !ok || entry.ProjectID != projectID || entry.SandboxID != sandboxID {
		return Binding{}, nil, ErrNotFound
	}
	if entry.Generation != expectedGeneration {
		return Binding{}, nil, ErrStaleGeneration
	}
	if entry.State != StateReady {
		return Binding{}, nil, ErrStateConflict
	}
	key := operationKey{routeID: routeID, projectID: projectID, sandboxID: sandboxID, generation: expectedGeneration}
	if l.activeOperations[key] == math.MaxUint64 {
		return Binding{}, nil, ErrActiveOperations
	}
	l.activeOperations[key]++
	lease := &OperationLease{state: &operationLeaseState{ledger: l, key: key}}
	return bindingFromDisk(entry), lease, nil
}

func (l *Ledger) releaseOperation(key operationKey) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	count, ok := l.activeOperations[key]
	if !ok || count == 0 {
		return ErrInvalid
	}
	entry, ok := l.state.Routes[key.routeID]
	if !ok || entry.ProjectID != key.projectID || entry.SandboxID != key.sandboxID || entry.Generation != key.generation {
		return ErrStateConflict
	}
	if entry.State != StateReady && entry.State != StateDraining {
		return ErrStateConflict
	}
	if count == 1 {
		delete(l.activeOperations, key)
	} else {
		l.activeOperations[key] = count - 1
	}
	return nil
}

func (l *Ledger) hasActiveOperations(entry diskEntry) bool {
	key := operationKey{routeID: entry.RouteID, projectID: entry.ProjectID, sandboxID: entry.SandboxID, generation: entry.Generation}
	return l.activeOperations[key] != 0
}

// Touch records qualifying traffic only if both generation and lifecycle state
// still match the caller's authorization decision.
func (l *Ledger) Touch(routeID string, expectedGeneration uint64, expectedState State, at time.Time) (Binding, error) {
	if !safeID.MatchString(routeID) || (expectedState != StateReady && expectedState != StateDraining) {
		return Binding{}, ErrInvalid
	}
	at, err := normalizeActivity(at)
	if err != nil {
		return Binding{}, err
	}
	var result diskEntry
	err = l.update(func(next *diskState) error {
		current, ok := next.Routes[routeID]
		if !ok {
			return ErrNotFound
		}
		if current.Generation != expectedGeneration {
			return ErrStaleGeneration
		}
		if current.State != expectedState {
			return ErrStateConflict
		}
		if at.Before(current.LastActivity) {
			return fmt.Errorf("%w: activity time moved backwards", ErrInvalid)
		}
		current.LastActivity = at
		next.Routes[routeID] = current
		result = current
		return nil
	})
	if err != nil {
		return Binding{}, err
	}
	return bindingFromDisk(result), nil
}

func (l *Ledger) Resolve(routeID string) (Binding, error) {
	if !safeID.MatchString(routeID) {
		return Binding{}, ErrInvalid
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return Binding{}, ErrClosed
	}
	entry, ok := l.state.Routes[routeID]
	if !ok {
		return Binding{}, ErrNotFound
	}
	return bindingFromDisk(entry), nil
}

// ResolveCurrent is the data-path lookup. It returns an engine target only
// when every identity, generation, and state bound into the caller's admitted
// capability still matches. Relay traffic should use this method rather than
// the administrative Resolve lookup.
func (l *Ledger) ResolveCurrent(routeID, projectID, sandboxID string, expectedGeneration uint64, expectedState State) (Binding, error) {
	if err := validateRouteIdentity(routeID, projectID, sandboxID); err != nil || !validState(expectedState) {
		return Binding{}, ErrInvalid
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return Binding{}, ErrClosed
	}
	entry, ok := l.state.Routes[routeID]
	if !ok || entry.ProjectID != projectID || entry.SandboxID != sandboxID {
		return Binding{}, ErrNotFound
	}
	if entry.Generation != expectedGeneration {
		return Binding{}, ErrStaleGeneration
	}
	if entry.State != expectedState {
		return Binding{}, ErrStateConflict
	}
	return bindingFromDisk(entry), nil
}

func (l *Ledger) List() ([]Route, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return nil, ErrClosed
	}
	routes := make([]Route, 0, len(l.state.Routes))
	for _, entry := range l.state.Routes {
		routes = append(routes, publicFromDisk(entry))
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].RouteID < routes[j].RouteID })
	return routes, nil
}

// Remove deletes only a released tombstone. The global generation counter is
// retained so reusing the opaque route can never recreate an old generation.
func (l *Ledger) Remove(routeID string, expectedGeneration uint64, expectedState State) error {
	if !safeID.MatchString(routeID) || expectedState != StateReleased {
		return ErrInvalid
	}
	return l.update(func(next *diskState) error {
		current, ok := next.Routes[routeID]
		if !ok {
			return ErrNotFound
		}
		if current.Generation != expectedGeneration {
			return ErrStaleGeneration
		}
		if current.State != expectedState {
			return ErrStateConflict
		}
		delete(next.Routes, routeID)
		return nil
	})
}

func (l *Ledger) update(fn func(*diskState) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	next := cloneDiskState(l.state)
	if err := fn(&next); err != nil {
		return err
	}
	if err := validateDiskState(next); err != nil {
		return err
	}
	if err := l.persist(next); err != nil {
		return err
	}
	l.state = next
	return nil
}

func (l *Ledger) persist(state diskState) error {
	if err := validateDiskState(state); err != nil {
		return err
	}
	checksum, err := checksumState(state)
	if err != nil {
		return err
	}
	data, err := json.Marshal(diskEnvelope{State: state, Checksum: checksum})
	if err != nil {
		return fmt.Errorf("encode node generation ledger: %w", err)
	}
	if len(data) > maxLedgerBytes {
		return fmt.Errorf("%w: encoded ledger exceeds limit", ErrInvalid)
	}
	dir := filepath.Dir(l.path)
	tmp, err := os.CreateTemp(dir, ".brezel-node-ledger-*")
	if err != nil {
		return fmt.Errorf("create temporary node generation ledger: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("protect temporary node generation ledger: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary node generation ledger: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary node generation ledger: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary node generation ledger: %w", err)
	}
	if err := os.Rename(tmpName, l.path); err != nil {
		return fmt.Errorf("replace node generation ledger: %w", err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open node generation ledger directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync node generation ledger directory: %w", err)
	}
	return nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open node generation ledger: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect open node generation ledger: %w", err)
	}
	pathInfo, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, pathInfo) {
		return nil, errors.New("node generation ledger changed while opening")
	}
	if err := validatePrivateFileInfo(info, "node generation ledger"); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read node generation ledger: %w", err)
	}
	if len(data) == 0 || int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: encoded ledger size is invalid", ErrCorrupt)
	}
	return data, nil
}

func decodeAndValidate(data []byte) (diskState, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var envelope diskEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return diskState{}, fmt.Errorf("%w: decode: %v", ErrCorrupt, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return diskState{}, fmt.Errorf("%w: trailing JSON data", ErrCorrupt)
	}
	want, err := checksumState(envelope.State)
	if err != nil {
		return diskState{}, fmt.Errorf("%w: checksum payload: %v", ErrCorrupt, err)
	}
	wantBytes, wantErr := hex.DecodeString(want)
	gotBytes, gotErr := hex.DecodeString(envelope.Checksum)
	if wantErr != nil || gotErr != nil || len(gotBytes) != sha256.Size || subtle.ConstantTimeCompare(wantBytes, gotBytes) != 1 {
		return diskState{}, fmt.Errorf("%w: checksum mismatch", ErrCorrupt)
	}
	if err := validateDiskState(envelope.State); err != nil {
		return diskState{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return envelope.State, nil
}

func checksumState(state diskState) (string, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func validateDiskState(state diskState) error {
	if state.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("unsupported schema version %d", state.SchemaVersion)
	}
	if state.Routes == nil {
		return errors.New("routes map is missing")
	}
	if len(state.Routes) > maxRoutes {
		return errors.New("route count exceeds limit")
	}
	generations := make(map[uint64]struct{}, len(state.Routes))
	engineIDs := make(map[string]struct{}, len(state.Routes))
	sandboxes := make(map[string]struct{}, len(state.Routes))
	for key, entry := range state.Routes {
		if key != entry.RouteID {
			return errors.New("route key does not match route identity")
		}
		if err := validateRouteIdentity(entry.RouteID, entry.ProjectID, entry.SandboxID); err != nil {
			return err
		}
		if entry.Generation == 0 || entry.Generation > state.LastGeneration {
			return errors.New("route generation is outside the durable generation clock")
		}
		if _, duplicate := generations[entry.Generation]; duplicate {
			return errors.New("route generations must be unique")
		}
		generations[entry.Generation] = struct{}{}
		if !validState(entry.State) {
			return errors.New("route has invalid state")
		}
		if _, offset := entry.LastActivity.Zone(); entry.LastActivity.IsZero() || offset != 0 {
			return errors.New("route has invalid last activity")
		}
		if entry.State == StateReleased {
			if entry.EngineID != "" {
				return errors.New("released route retained an engine identity")
			}
			continue
		}
		if err := validateEngineID(entry.EngineID); err != nil {
			return err
		}
		if _, duplicate := engineIDs[entry.EngineID]; duplicate {
			return errors.New("engine identity is assigned to multiple routes")
		}
		engineIDs[entry.EngineID] = struct{}{}
		sandboxKey := entry.ProjectID + "\x00" + entry.SandboxID
		if _, duplicate := sandboxes[sandboxKey]; duplicate {
			return errors.New("sandbox identity is assigned to multiple routes")
		}
		sandboxes[sandboxKey] = struct{}{}
	}
	return nil
}

func validatePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect ledger directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("ledger directory must be a real directory")
	}
	if info.Mode().Perm() != 0o700 {
		return errors.New("ledger directory must have mode 0700")
	}
	if !ownedByCurrentUser(info) {
		return errors.New("ledger directory must be owned by the current user")
	}
	return nil
}

func validatePrivateFileInfo(info os.FileInfo, label string) error {
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must be a regular file", label)
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("%s must have mode 0600", label)
	}
	if !ownedByCurrentUser(info) {
		return fmt.Errorf("%s must be owned by the current user", label)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
		return fmt.Errorf("%s must not have additional hard links", label)
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return !ok || stat.Uid == uint32(os.Geteuid())
}

func validateRouteInput(routeID, projectID, sandboxID, engineID string) error {
	if err := validateRouteIdentity(routeID, projectID, sandboxID); err != nil {
		return err
	}
	return validateEngineID(engineID)
}

func validateRouteIdentity(routeID, projectID, sandboxID string) error {
	if !safeID.MatchString(routeID) || !safeID.MatchString(projectID) || !safeID.MatchString(sandboxID) {
		return ErrInvalid
	}
	return nil
}

func validateEngineID(engineID string) error {
	if engineID == "" || len(engineID) > maxEngineIDBytes || !utf8.ValidString(engineID) || strings.TrimSpace(engineID) != engineID {
		return ErrInvalid
	}
	for _, character := range engineID {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return ErrInvalid
		}
	}
	return nil
}

func normalizeActivity(at time.Time) (time.Time, error) {
	if at.IsZero() {
		return time.Time{}, fmt.Errorf("%w: activity time is required", ErrInvalid)
	}
	return at.UTC().Round(0), nil
}

func nextGeneration(current uint64) (uint64, error) {
	if current == math.MaxUint64 {
		return 0, errors.New("node route generation space is exhausted")
	}
	return current + 1, nil
}

func validState(state State) bool {
	switch state {
	case StateAttaching, StateReady, StateDraining, StateStandby, StateReleased:
		return true
	default:
		return false
	}
}

func allowedTransition(from, to State) bool {
	switch from {
	case StateAttaching:
		return to == StateReady || to == StateReleased
	case StateReady:
		return to == StateDraining
	case StateDraining:
		return to == StateReady || to == StateStandby || to == StateReleased
	case StateStandby:
		return to == StateReleased
	default:
		return false
	}
}

func assignmentConflicts(routes map[string]diskEntry, exceptRouteID, projectID, sandboxID, engineID string) bool {
	for routeID, entry := range routes {
		if routeID == exceptRouteID || entry.State == StateReleased {
			continue
		}
		if entry.EngineID == engineID || (entry.ProjectID == projectID && entry.SandboxID == sandboxID) {
			return true
		}
	}
	return false
}

func cloneDiskState(state diskState) diskState {
	routes := make(map[string]diskEntry, len(state.Routes))
	for key, entry := range state.Routes {
		routes[key] = entry
	}
	state.Routes = routes
	return state
}

func bindingFromDisk(entry diskEntry) Binding {
	return Binding{route: publicFromDisk(entry), engineID: entry.EngineID}
}

func publicFromDisk(entry diskEntry) Route {
	return Route{RouteID: entry.RouteID, ProjectID: entry.ProjectID, SandboxID: entry.SandboxID, Generation: entry.Generation, State: entry.State, LastActivity: entry.LastActivity}
}
