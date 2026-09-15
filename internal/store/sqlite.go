package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/infercrane/brezel/internal/domain"
	_ "modernc.org/sqlite"
)

const sqliteSchemaVersion = 1

const (
	resourceEnvironment = "environment"
	resourceSandbox     = "sandbox"
	resourceOperation   = "operation"
	resourceCheckpoint  = "checkpoint"
	resourceWorkspace   = "workspace"
	resourceConnector   = "connector"
)

// SQLiteStore is Brezel's embedded durable lifecycle ledger. The WAL keeps
// resource mutations scoped to changed rows while the process lock preserves
// the current single-controller ownership contract.
type SQLiteStore struct {
	mu       sync.Mutex
	path     string
	db       *sql.DB
	lockFile *os.File
	closed   bool
}

// OpenSQLite opens or creates an embedded ledger. If the database has never
// been initialized and legacyJSONPath names an existing FileStore document,
// it is imported in one transaction. The source file is never modified or
// deleted, making rollback explicit and recoverable.
func OpenSQLite(path, legacyJSONPath string) (*SQLiteStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("state database path is required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	if err := requirePrivateDirectory(dir); err != nil {
		return nil, err
	}
	if err := requirePrivateFileIfPresent(path, "state database"); err != nil {
		return nil, err
	}
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if err := requirePrivateFileIfPresent(sidecar, "state database sidecar"); err != nil {
			return nil, err
		}
	}

	lockPath := path + ".lock"
	if err := requirePrivateFileIfPresent(lockPath, "state database lock"); err != nil {
		return nil, err
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open state database lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		return nil, errors.New("state database is already open by another runtime process")
	}
	closeLock := func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}

	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if createErr != nil {
			closeLock()
			return nil, fmt.Errorf("create state database: %w", createErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			closeLock()
			return nil, fmt.Errorf("close new state database: %w", closeErr)
		}
	} else if err != nil {
		closeLock()
		return nil, fmt.Errorf("inspect state database: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		closeLock()
		return nil, fmt.Errorf("open state database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &SQLiteStore{path: path, db: db, lockFile: lockFile}
	if err := store.initialize(legacyJSONPath); err != nil {
		_ = db.Close()
		closeLock()
		return nil, err
	}
	if err := store.chmodFiles(); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLiteStore) initialize(legacyJSONPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
		"PRAGMA wal_autocheckpoint=1000",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure state database: %w", err)
		}
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS metadata (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS resources (
			kind TEXT NOT NULL,
			resource_key BLOB NOT NULL,
			payload BLOB NOT NULL,
			PRIMARY KEY (kind, resource_key)
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS sandbox_events (
			project_id TEXT NOT NULL,
			resource_id TEXT NOT NULL,
			sequence INTEGER NOT NULL,
			event_id TEXT NOT NULL,
			payload BLOB NOT NULL,
			PRIMARY KEY (project_id, resource_id, sequence)
		) WITHOUT ROWID`,
		`CREATE INDEX IF NOT EXISTS sandbox_events_id ON sandbox_events(event_id)`,
		`CREATE TABLE IF NOT EXISTS idempotency (
			idempotency_key BLOB PRIMARY KEY,
			operation_id TEXT NOT NULL,
			digest TEXT NOT NULL DEFAULT ''
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS sandbox_capacity (
			project_id TEXT PRIMARY KEY,
			active_count INTEGER NOT NULL CHECK (active_count >= 0)
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS workspace_capacity (
			project_id TEXT PRIMARY KEY,
			active_count INTEGER NOT NULL CHECK (active_count >= 0)
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS environment_capacity (
			project_id TEXT PRIMARY KEY,
			resource_count INTEGER NOT NULL CHECK (resource_count >= 0)
		) WITHOUT ROWID`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize state database schema: %w", err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin state database initialization: %w", err)
	}
	defer tx.Rollback()
	// Active workspace mounts are a derived index, so recreation is the
	// migration path from the earlier composite primary key. DDL and the later
	// rebuild share this transaction: an interrupted open retains the previous
	// usable index instead of exposing a half-migrated table.
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS active_workspace_mounts;
		CREATE TABLE active_workspace_mounts (
			project_id TEXT NOT NULL,
			workspace_id TEXT NOT NULL,
			sandbox_key BLOB NOT NULL,
			PRIMARY KEY (project_id, workspace_id)
		) WITHOUT ROWID;
		CREATE INDEX active_workspace_mounts_sandbox ON active_workspace_mounts(sandbox_key)`); err != nil {
		return fmt.Errorf("migrate active workspace attachment index: %w", err)
	}
	// Lifecycle heads preserve operation insertion order across ordinary opens.
	// Databases without the derived-index marker (or whose table was lost) are
	// rebuilt once in this same initialization transaction.
	rebuildLifecycleHeads, err := ensureLifecycleOperationHeadsTx(ctx, tx)
	if err != nil {
		return fmt.Errorf("migrate lifecycle operation recovery index: %w", err)
	}
	var versionText string
	err = tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key = 'schema_version'`).Scan(&versionText)
	switch {
	case err == nil:
		version, parseErr := strconv.Atoi(versionText)
		if parseErr != nil || version != sqliteSchemaVersion {
			return fmt.Errorf("unsupported state database schema version %q (runtime supports %d)", versionText, sqliteSchemaVersion)
		}
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("read state database schema version: %w", err)
	default:
		initial := NewState()
		if legacyJSONPath != "" {
			legacy, loadErr := loadLegacyStateIfPresent(legacyJSONPath)
			if loadErr != nil {
				return fmt.Errorf("load legacy state for migration: %w", loadErr)
			}
			if legacy != nil {
				initial = *legacy
			}
		}
		if err := replaceStateTx(ctx, tx, initial); err != nil {
			return fmt.Errorf("initialize state database contents: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO metadata(key, value) VALUES ('schema_version', ?), ('state_schema_version', ?)`, strconv.Itoa(sqliteSchemaVersion), strconv.Itoa(CurrentSchemaVersion)); err != nil {
			return fmt.Errorf("record state database schema version: %w", err)
		}
	}
	// Admission indexes are derived from authoritative sandbox rows. Rebuilding
	// them while opening also backfills databases created by earlier releases
	// without changing the durable public state schema.
	if err := rebuildSandboxAdmissionIndexesTx(ctx, tx); err != nil {
		return fmt.Errorf("validate stored state while rebuilding sandbox admission indexes: %w", err)
	}
	if rebuildLifecycleHeads {
		if err := rebuildLifecycleOperationHeadsTx(ctx, tx); err != nil {
			return fmt.Errorf("validate stored state while rebuilding lifecycle operation recovery index: %w", err)
		}
		if err := recordLifecycleOperationHeadsVersionTx(ctx, tx); err != nil {
			return fmt.Errorf("record lifecycle operation recovery index version: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit state database initialization: %w", err)
	}
	var quickCheck string
	if err := s.db.QueryRowContext(ctx, "PRAGMA quick_check(1)").Scan(&quickCheck); err != nil {
		return fmt.Errorf("check state database integrity: %w", err)
	}
	if quickCheck != "ok" {
		return fmt.Errorf("state database integrity check failed: %s", quickCheck)
	}
	readTx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin stored state validation: %w", err)
	}
	if _, err := loadStateTx(ctx, readTx); err != nil {
		_ = readTx.Rollback()
		return fmt.Errorf("validate stored state: %w", err)
	}
	if err := readTx.Rollback(); err != nil {
		return fmt.Errorf("finish stored state validation: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var closeErr error
	if s.db != nil {
		closeErr = s.db.Close()
	}
	if s.lockFile != nil {
		unlockErr := syscall.Flock(int(s.lockFile.Fd()), syscall.LOCK_UN)
		lockCloseErr := s.lockFile.Close()
		if closeErr == nil {
			closeErr = unlockErr
		}
		if closeErr == nil {
			closeErr = lockCloseErr
		}
	}
	return closeErr
}

func (s *SQLiteStore) Ready() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.db == nil || s.lockFile == nil {
		return errors.New("state store is closed")
	}
	for _, file := range []struct{ path, label string }{
		{s.path, "state database"}, {s.path + "-wal", "state database sidecar"}, {s.path + "-shm", "state database sidecar"},
	} {
		if err := requirePrivateFileIfPresent(file.path, file.label); err != nil {
			return err
		}
	}
	var version string
	if err := s.db.QueryRow(`SELECT value FROM metadata WHERE key = 'schema_version'`).Scan(&version); err != nil {
		return fmt.Errorf("read state database schema version: %w", err)
	}
	if version != strconv.Itoa(sqliteSchemaVersion) {
		return errors.New("state database schema version changed")
	}
	return nil
}

func (s *SQLiteStore) View(fn func(State) error) error {
	if fn == nil {
		return errors.New("state view callback is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("state store is closed")
	}
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state, err := loadStateTx(context.Background(), tx)
	if err != nil {
		return err
	}
	if err := fn(state); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) Update(fn func(*State) error) error {
	if fn == nil {
		return errors.New("state update callback is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("state store is closed")
	}
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state, err := loadStateTx(ctx, tx)
	if err != nil {
		return err
	}
	before, err := encodeState(state)
	if err != nil {
		return err
	}
	if err := fn(&state); err != nil {
		return err
	}
	if err := validateState(state); err != nil {
		return fmt.Errorf("validate updated state: %w", err)
	}
	after, err := encodeState(state)
	if err != nil {
		return err
	}
	if err := syncEncodedState(ctx, tx, before, after); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) GetSandbox(projectID, sandboxID string) (domain.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return domain.Sandbox{}, errors.New("state store is closed")
	}
	var payload []byte
	err := s.db.QueryRow(`SELECT payload FROM resources WHERE kind = ? AND resource_key = ?`, resourceSandbox, []byte(ScopedKey(projectID, sandboxID))).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Sandbox{}, ErrNotFound
	}
	if err != nil {
		return domain.Sandbox{}, err
	}
	var sandbox domain.Sandbox
	if err := json.Unmarshal(payload, &sandbox); err != nil {
		return domain.Sandbox{}, fmt.Errorf("decode sandbox: %w", err)
	}
	return cloneSandbox(sandbox), nil
}

// ListSandboxesForReconcile scans only sandbox resource rows. Periodic
// observation must not decode the operation, idempotency, or event ledgers,
// whose size is unrelated to the amount of recovery work due this tick.
func (s *SQLiteStore) ListSandboxesForReconcile() ([]domain.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("state store is closed")
	}
	rows, err := s.db.Query(`SELECT payload FROM resources WHERE kind = ?`, resourceSandbox)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sandboxes := make([]domain.Sandbox, 0)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var sandbox domain.Sandbox
		if err := json.Unmarshal(payload, &sandbox); err != nil {
			return nil, fmt.Errorf("decode sandbox: %w", err)
		}
		if sandbox.State == domain.SandboxDeleted || sandbox.State == domain.SandboxExpired || sandbox.State == domain.SandboxFailed {
			continue
		}
		sandboxes = append(sandboxes, sandbox)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sandboxes, nil
}

func (s *SQLiteStore) ListWorkspacesForReconcile() ([]domain.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("state store is closed")
	}
	rows, err := s.db.Query(`SELECT payload FROM resources WHERE kind = ?`, resourceWorkspace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	workspaces := make([]domain.Workspace, 0)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var workspace domain.Workspace
		if err := json.Unmarshal(payload, &workspace); err != nil {
			return nil, fmt.Errorf("decode workspace: %w", err)
		}
		if workspace.State == domain.WorkspaceReady || workspace.State == domain.WorkspaceDeleted || workspace.State == domain.WorkspaceFailed {
			continue
		}
		workspaces = append(workspaces, workspace)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return workspaces, nil
}

func (s *SQLiteStore) ListLifecycleOperationsForReconcile() ([]domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("state store is closed")
	}
	rows, err := s.db.Query(`SELECT resources.payload, heads.project_id, heads.owner_kind, heads.resource_id, heads.family, heads.operation_id
		FROM lifecycle_operation_heads AS heads
		JOIN resources ON resources.kind = heads.operation_resource_kind AND resources.resource_key = heads.operation_key
		ORDER BY heads.project_id, heads.owner_kind, heads.resource_id, heads.family`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	operations := make([]domain.Operation, 0)
	for rows.Next() {
		var payload []byte
		var projectID, ownerKind, resourceID, family, operationID string
		if err := rows.Scan(&payload, &projectID, &ownerKind, &resourceID, &family, &operationID); err != nil {
			return nil, err
		}
		var operation domain.Operation
		if err := json.Unmarshal(payload, &operation); err != nil {
			return nil, fmt.Errorf("decode lifecycle operation head: %w", err)
		}
		key, lifecycle := lifecycleHeadForOperation(operation)
		if !lifecycle || key.projectID != projectID || key.ownerKind != ownerKind || key.resourceID != resourceID || key.family != family || operation.ID != operationID {
			return nil, errors.New("lifecycle operation recovery index is inconsistent")
		}
		if operation.State != domain.OperationSucceeded {
			operations = append(operations, operation)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return operations, nil
}

func (s *SQLiteStore) RecordSandboxActivity(projectID, sandboxID string, at time.Time) (domain.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return domain.Sandbox{}, errors.New("state store is closed")
	}
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Sandbox{}, err
	}
	defer tx.Rollback()
	key := []byte(ScopedKey(projectID, sandboxID))
	var payload []byte
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM resources WHERE kind = ? AND resource_key = ?`, resourceSandbox, key).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Sandbox{}, ErrNotFound
		}
		return domain.Sandbox{}, err
	}
	var current domain.Sandbox
	if err := json.Unmarshal(payload, &current); err != nil {
		return domain.Sandbox{}, fmt.Errorf("decode sandbox: %w", err)
	}
	next := advanceSandboxActivity(current, at)
	if next.Revision == current.Revision {
		return cloneSandbox(current), nil
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return domain.Sandbox{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE resources SET payload = ? WHERE kind = ? AND resource_key = ?`, encoded, resourceSandbox, key); err != nil {
		return domain.Sandbox{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Sandbox{}, err
	}
	return cloneSandbox(next), nil
}

func (s *SQLiteStore) AppendSandboxEvent(event domain.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("state store is closed")
	}
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var sandboxPayload []byte
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM resources WHERE kind = ? AND resource_key = ?`, resourceSandbox, []byte(ScopedKey(event.ProjectID, event.ResourceID))).Scan(&sandboxPayload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var sandbox domain.Sandbox
	if err := json.Unmarshal(sandboxPayload, &sandbox); err != nil {
		return fmt.Errorf("decode sandbox: %w", err)
	}
	details, err := cloneEventDetails(event.Details)
	if err != nil {
		return fmt.Errorf("validate event details: %w", err)
	}
	event.Details = details
	event.State = sandbox.State
	var currentSequence sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(sequence) FROM sandbox_events WHERE project_id = ? AND resource_id = ?`, event.ProjectID, event.ResourceID).Scan(&currentSequence); err != nil {
		return err
	}
	event.Sequence = 1
	if currentSequence.Valid {
		event.Sequence = currentSequence.Int64 + 1
	}
	if err := validateSandboxEvent(event); err != nil {
		return err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sandbox_events(project_id, resource_id, sequence, event_id, payload) VALUES (?, ?, ?, ?, ?)`, event.ProjectID, event.ResourceID, event.Sequence, event.ID, payload); err != nil {
		return err
	}
	return tx.Commit()
}

type encodedState struct {
	resources   map[string]map[string][]byte
	events      map[string][]byte
	idempotency map[string]idempotencyRow
}

type idempotencyRow struct {
	operationID string
	digest      string
}

func encodeState(state State) (encodedState, error) {
	encoded := encodedState{
		resources: map[string]map[string][]byte{
			resourceEnvironment: {}, resourceSandbox: {}, resourceOperation: {},
			resourceCheckpoint: {}, resourceWorkspace: {}, resourceConnector: {},
		},
		events:      make(map[string][]byte, len(state.Events)),
		idempotency: make(map[string]idempotencyRow, len(state.Idempotency)),
	}
	encodeMap := func(kind string, values any) error {
		data, err := json.Marshal(values)
		if err != nil {
			return err
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		for key, payload := range raw {
			encoded.resources[kind][key] = append([]byte(nil), payload...)
		}
		return nil
	}
	for _, entry := range []struct {
		kind   string
		values any
	}{
		{resourceEnvironment, state.Environments}, {resourceSandbox, state.Sandboxes},
		{resourceOperation, state.Operations}, {resourceCheckpoint, state.Checkpoints},
		{resourceWorkspace, state.Workspaces}, {resourceConnector, state.Connectors},
	} {
		if err := encodeMap(entry.kind, entry.values); err != nil {
			return encodedState{}, fmt.Errorf("encode %s resources: %w", entry.kind, err)
		}
	}
	for _, event := range state.Events {
		if err := validateSandboxEvent(event); err != nil {
			return encodedState{}, err
		}
		key := eventRowKey(event.ProjectID, event.ResourceID, event.Sequence)
		if _, exists := encoded.events[key]; exists {
			return encodedState{}, errors.New("events contain a duplicate sandbox sequence")
		}
		payload, err := json.Marshal(event)
		if err != nil {
			return encodedState{}, err
		}
		encoded.events[key] = payload
	}
	for key, operationID := range state.Idempotency {
		encoded.idempotency[key] = idempotencyRow{operationID: operationID, digest: state.IdempotencyDigests[key]}
	}
	return encoded, nil
}

func syncEncodedState(ctx context.Context, tx *sql.Tx, before, after encodedState) error {
	type sandboxIndexChange struct {
		key           string
		before, after []byte
	}
	var sandboxIndexChanges []sandboxIndexChange
	type resourceIndexChange struct {
		kind          string
		key           string
		before, after []byte
	}
	var admissionIndexChanges []resourceIndexChange
	var lifecycleOperationChanges []lifecycleProjectionChange
	var lifecycleOwnerChanges []lifecycleProjectionChange
	for kind, afterRows := range after.resources {
		beforeRows := before.resources[kind]
		for key := range beforeRows {
			if _, exists := afterRows[key]; !exists {
				if _, err := tx.ExecContext(ctx, `DELETE FROM resources WHERE kind = ? AND resource_key = ?`, kind, []byte(key)); err != nil {
					return err
				}
				if kind == resourceSandbox {
					sandboxIndexChanges = append(sandboxIndexChanges, sandboxIndexChange{key: key, before: beforeRows[key]})
				} else if kind == resourceWorkspace || kind == resourceEnvironment {
					admissionIndexChanges = append(admissionIndexChanges, resourceIndexChange{kind: kind, key: key, before: beforeRows[key]})
				}
				if kind == resourceOperation {
					lifecycleOperationChanges = append(lifecycleOperationChanges, lifecycleProjectionChange{kind: kind, key: key, before: beforeRows[key]})
				} else if kind == resourceSandbox || kind == resourceWorkspace {
					lifecycleOwnerChanges = append(lifecycleOwnerChanges, lifecycleProjectionChange{kind: kind, key: key, before: beforeRows[key]})
				}
			}
		}
		for key, payload := range afterRows {
			if string(beforeRows[key]) == string(payload) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO resources(kind, resource_key, payload) VALUES (?, ?, ?) ON CONFLICT(kind, resource_key) DO UPDATE SET payload = excluded.payload`, kind, []byte(key), payload); err != nil {
				return err
			}
			if kind == resourceSandbox {
				sandboxIndexChanges = append(sandboxIndexChanges, sandboxIndexChange{key: key, before: beforeRows[key], after: payload})
			} else if kind == resourceWorkspace || kind == resourceEnvironment {
				admissionIndexChanges = append(admissionIndexChanges, resourceIndexChange{kind: kind, key: key, before: beforeRows[key], after: payload})
			}
			if kind == resourceOperation {
				lifecycleOperationChanges = append(lifecycleOperationChanges, lifecycleProjectionChange{kind: kind, key: key, before: beforeRows[key], after: payload})
			} else if kind == resourceSandbox || kind == resourceWorkspace {
				lifecycleOwnerChanges = append(lifecycleOwnerChanges, lifecycleProjectionChange{kind: kind, key: key, before: beforeRows[key], after: payload})
			}
		}
	}
	if err := syncLifecycleOperationHeadsTx(ctx, tx, lifecycleOperationChanges, lifecycleOwnerChanges); err != nil {
		return err
	}
	for _, change := range admissionIndexChanges {
		if err := removeResourceAdmissionIndexTx(ctx, tx, change.kind, change.key, change.before); err != nil {
			return err
		}
	}
	for _, change := range admissionIndexChanges {
		if err := addResourceAdmissionIndexTx(ctx, tx, change.kind, change.key, change.after); err != nil {
			return err
		}
	}
	// Workspace ownership handoffs can legitimately remove one active owner and
	// add another in one state transaction. Remove every changed before-owner
	// first, then insert after-owners, so Go map iteration order cannot create a
	// transient uniqueness failure.
	for _, change := range sandboxIndexChanges {
		if err := removeSandboxAdmissionIndexesTx(ctx, tx, change.key, change.before); err != nil {
			return err
		}
	}
	for _, change := range sandboxIndexChanges {
		if err := addSandboxAdmissionIndexesTx(ctx, tx, change.key, change.after); err != nil {
			return err
		}
	}
	for key := range before.events {
		if _, exists := after.events[key]; !exists {
			projectID, resourceID, sequence, err := parseEventRowKey(key)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM sandbox_events WHERE project_id = ? AND resource_id = ? AND sequence = ?`, projectID, resourceID, sequence); err != nil {
				return err
			}
		}
	}
	for key, payload := range after.events {
		if string(before.events[key]) == string(payload) {
			continue
		}
		var event domain.Event
		if err := json.Unmarshal(payload, &event); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sandbox_events(project_id, resource_id, sequence, event_id, payload) VALUES (?, ?, ?, ?, ?) ON CONFLICT(project_id, resource_id, sequence) DO UPDATE SET event_id = excluded.event_id, payload = excluded.payload`, event.ProjectID, event.ResourceID, event.Sequence, event.ID, payload); err != nil {
			return err
		}
	}
	for key := range before.idempotency {
		if _, exists := after.idempotency[key]; !exists {
			if _, err := tx.ExecContext(ctx, `DELETE FROM idempotency WHERE idempotency_key = ?`, []byte(key)); err != nil {
				return err
			}
		}
	}
	for key, row := range after.idempotency {
		if before.idempotency[key] == row {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency(idempotency_key, operation_id, digest) VALUES (?, ?, ?) ON CONFLICT(idempotency_key) DO UPDATE SET operation_id = excluded.operation_id, digest = excluded.digest`, []byte(key), row.operationID, row.digest); err != nil {
			return err
		}
	}
	return nil
}

func replaceStateTx(ctx context.Context, tx *sql.Tx, state State) error {
	if err := validateState(state); err != nil {
		return err
	}
	encoded, err := encodeState(state)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM resources; DELETE FROM sandbox_events; DELETE FROM idempotency; DELETE FROM sandbox_capacity; DELETE FROM workspace_capacity; DELETE FROM environment_capacity; DELETE FROM active_workspace_mounts; DELETE FROM lifecycle_operation_heads`); err != nil {
		return err
	}
	return syncEncodedState(ctx, tx, encodedState{resources: map[string]map[string][]byte{}, events: map[string][]byte{}, idempotency: map[string]idempotencyRow{}}, encoded)
}

func loadStateTx(ctx context.Context, tx *sql.Tx) (State, error) {
	state := NewState()
	rows, err := tx.QueryContext(ctx, `SELECT kind, resource_key, payload FROM resources`)
	if err != nil {
		return State{}, err
	}
	for rows.Next() {
		var kind string
		var key, payload []byte
		if err := rows.Scan(&kind, &key, &payload); err != nil {
			rows.Close()
			return State{}, err
		}
		if err := decodeResource(&state, kind, string(key), payload); err != nil {
			rows.Close()
			return State{}, err
		}
	}
	if err := rows.Close(); err != nil {
		return State{}, err
	}

	eventRows, err := tx.QueryContext(ctx, `SELECT payload FROM sandbox_events ORDER BY project_id, resource_id, sequence`)
	if err != nil {
		return State{}, err
	}
	for eventRows.Next() {
		var payload []byte
		if err := eventRows.Scan(&payload); err != nil {
			eventRows.Close()
			return State{}, err
		}
		var event domain.Event
		if err := json.Unmarshal(payload, &event); err != nil {
			eventRows.Close()
			return State{}, fmt.Errorf("decode event: %w", err)
		}
		state.Events = append(state.Events, event)
	}
	if err := eventRows.Close(); err != nil {
		return State{}, err
	}

	idempotencyRows, err := tx.QueryContext(ctx, `SELECT idempotency_key, operation_id, digest FROM idempotency`)
	if err != nil {
		return State{}, err
	}
	for idempotencyRows.Next() {
		var key []byte
		var operationID, digest string
		if err := idempotencyRows.Scan(&key, &operationID, &digest); err != nil {
			idempotencyRows.Close()
			return State{}, err
		}
		state.Idempotency[string(key)] = operationID
		if digest != "" {
			state.IdempotencyDigests[string(key)] = digest
		}
	}
	if err := idempotencyRows.Close(); err != nil {
		return State{}, err
	}
	if err := validateState(state); err != nil {
		return State{}, fmt.Errorf("validate stored state: %w", err)
	}
	return state, nil
}

func decodeResource(state *State, kind, key string, payload []byte) error {
	switch kind {
	case resourceEnvironment:
		var value domain.Environment
		if err := json.Unmarshal(payload, &value); err != nil {
			return err
		}
		state.Environments[key] = value
	case resourceSandbox:
		var value domain.Sandbox
		if err := json.Unmarshal(payload, &value); err != nil {
			return err
		}
		state.Sandboxes[key] = value
	case resourceOperation:
		var value domain.Operation
		if err := json.Unmarshal(payload, &value); err != nil {
			return err
		}
		state.Operations[key] = value
	case resourceCheckpoint:
		var value domain.Checkpoint
		if err := json.Unmarshal(payload, &value); err != nil {
			return err
		}
		state.Checkpoints[key] = value
	case resourceWorkspace:
		var value domain.Workspace
		if err := json.Unmarshal(payload, &value); err != nil {
			return err
		}
		state.Workspaces[key] = value
	case resourceConnector:
		var value domain.Connector
		if err := json.Unmarshal(payload, &value); err != nil {
			return err
		}
		state.Connectors[key] = value
	default:
		return fmt.Errorf("state database contains unknown resource kind %q", kind)
	}
	return nil
}

func loadLegacyStateIfPresent(path string) (*State, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("legacy state file must be a private regular file")
	}
	if info.Size() > maxStateBytes {
		return nil, errors.New("legacy state file exceeds 128 MiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	state := State{}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode legacy state: %w", err)
	}
	if state.SchemaVersion == 0 {
		state.SchemaVersion = CurrentSchemaVersion
	}
	normalizeState(&state)
	if err := validateState(state); err != nil {
		return nil, err
	}
	return &state, nil
}

func normalizeState(state *State) {
	if state.Environments == nil {
		state.Environments = map[string]domain.Environment{}
	}
	if state.Sandboxes == nil {
		state.Sandboxes = map[string]domain.Sandbox{}
	}
	if state.Operations == nil {
		state.Operations = map[string]domain.Operation{}
	}
	if state.Checkpoints == nil {
		state.Checkpoints = map[string]domain.Checkpoint{}
	}
	if state.Workspaces == nil {
		state.Workspaces = map[string]domain.Workspace{}
	}
	if state.Connectors == nil {
		state.Connectors = map[string]domain.Connector{}
	}
	if state.Events == nil {
		state.Events = []domain.Event{}
	}
	if state.Idempotency == nil {
		state.Idempotency = map[string]string{}
	}
	if state.IdempotencyDigests == nil {
		state.IdempotencyDigests = map[string]string{}
	}
}

func eventRowKey(projectID, resourceID string, sequence int64) string {
	return projectID + "\x00" + resourceID + "\x00" + strconv.FormatInt(sequence, 10)
}

func parseEventRowKey(key string) (string, string, int64, error) {
	parts := strings.Split(key, "\x00")
	if len(parts) != 3 {
		return "", "", 0, errors.New("invalid encoded event identity")
	}
	sequence, err := strconv.ParseInt(parts[2], 10, 64)
	return parts[0], parts[1], sequence, err
}

func requirePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect state directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("state directory must be a real directory")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("state directory permissions must not allow group or other writes")
	}
	return nil
}

func requirePrivateFileIfPresent(path, label string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", label, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must be a private regular file", label)
	}
	return nil
}

func (s *SQLiteStore) chmodFiles() error {
	for _, path := range []string{s.path, s.path + "-wal", s.path + "-shm"} {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("restrict state database permissions: %w", err)
		}
	}
	return nil
}
