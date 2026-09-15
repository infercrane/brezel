package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/infercrane/brezel/internal/domain"
)

const totalSandboxCapacityKey = ""

// LookupIdempotency reads one idempotency claim and its referenced operation
// through primary-key lookups. It deliberately does not decode unrelated
// operations or resources.
func (s *SQLiteStore) LookupIdempotency(projectID, kind, key string) (IdempotencyLookup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return IdempotencyLookup{}, errors.New("state store is closed")
	}
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return IdempotencyLookup{}, err
	}
	defer tx.Rollback()
	lookup, err := loadIdempotencyLookupTx(ctx, tx, projectID, kind, key)
	if err != nil {
		return IdempotencyLookup{}, err
	}
	if err := tx.Commit(); err != nil {
		return IdempotencyLookup{}, err
	}
	return lookup, nil
}

// ReadSandboxAdmission returns one consistent create/restore admission view.
// Capacity counters and active workspace attachments are maintained in the
// same transaction as sandbox lifecycle rows, while every dependency lookup
// uses the resources primary key.
func (s *SQLiteStore) ReadSandboxAdmission(query SandboxAdmissionQuery) (SandboxAdmissionSnapshot, error) {
	if err := validateSandboxAdmissionQuery(query); err != nil {
		return SandboxAdmissionSnapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return SandboxAdmissionSnapshot{}, errors.New("state store is closed")
	}
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return SandboxAdmissionSnapshot{}, err
	}
	defer tx.Rollback()

	out := SandboxAdmissionSnapshot{
		Connectors:           make(map[string]domain.Connector, len(query.ConnectorRevisions)),
		Workspaces:           make(map[string]domain.Workspace, len(query.WorkspaceIDs)),
		AttachedWorkspaceIDs: make(map[string]bool, len(query.WorkspaceIDs)),
	}
	out.Idempotency, err = loadIdempotencyLookupTx(ctx, tx, query.ProjectID, query.IdempotencyKind, query.IdempotencyKey)
	if err != nil {
		return SandboxAdmissionSnapshot{}, err
	}
	// An idempotent replay does not consume capacity or require dependencies to
	// still be independently discoverable. This preserves the service's prior
	// replay-first behavior.
	if out.Idempotency.Found {
		if err := tx.Commit(); err != nil {
			return SandboxAdmissionSnapshot{}, err
		}
		return out, nil
	}

	if out.ActiveTotal, err = readSandboxCapacityTx(ctx, tx, totalSandboxCapacityKey); err != nil {
		return SandboxAdmissionSnapshot{}, err
	}
	if out.ActiveForProject, err = readSandboxCapacityTx(ctx, tx, query.ProjectID); err != nil {
		return SandboxAdmissionSnapshot{}, err
	}

	environmentRevision := query.EnvironmentRevision
	if query.CheckpointID != "" {
		out.CheckpointFound, err = loadAdmissionResourceTx(ctx, tx, resourceCheckpoint, query.ProjectID, query.CheckpointID, &out.Checkpoint)
		if err != nil {
			return SandboxAdmissionSnapshot{}, err
		}
		if out.CheckpointFound {
			environmentRevision = out.Checkpoint.EnvironmentRevision
		}
	}
	if environmentRevision != "" {
		out.EnvironmentFound, err = loadAdmissionResourceTx(ctx, tx, resourceEnvironment, query.ProjectID, environmentRevision, &out.Environment)
		if err != nil {
			return SandboxAdmissionSnapshot{}, err
		}
	}
	for _, revision := range query.ConnectorRevisions {
		var connector domain.Connector
		found, loadErr := loadAdmissionResourceTx(ctx, tx, resourceConnector, query.ProjectID, revision, &connector)
		if loadErr != nil {
			return SandboxAdmissionSnapshot{}, loadErr
		}
		if found {
			out.Connectors[revision] = connector
		}
	}
	for _, workspaceID := range query.WorkspaceIDs {
		var workspace domain.Workspace
		found, loadErr := loadAdmissionResourceTx(ctx, tx, resourceWorkspace, query.ProjectID, workspaceID, &workspace)
		if loadErr != nil {
			return SandboxAdmissionSnapshot{}, loadErr
		}
		if found {
			out.Workspaces[workspaceID] = workspace
		}
		var attached int
		loadErr = tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM active_workspace_mounts WHERE project_id = ? AND workspace_id = ?
		)`, query.ProjectID, workspaceID).Scan(&attached)
		if loadErr != nil {
			return SandboxAdmissionSnapshot{}, loadErr
		}
		out.AttachedWorkspaceIDs[workspaceID] = attached != 0
	}
	if err := tx.Commit(); err != nil {
		return SandboxAdmissionSnapshot{}, err
	}
	return out, nil
}

func (s *SQLiteStore) ReadWorkspaceAdmission(query WorkspaceAdmissionQuery) (WorkspaceAdmissionSnapshot, error) {
	if err := validateAdmissionIdentity(query.ProjectID, query.IdempotencyKind, query.IdempotencyKey); err != nil {
		return WorkspaceAdmissionSnapshot{}, fmt.Errorf("workspace admission query: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return WorkspaceAdmissionSnapshot{}, errors.New("state store is closed")
	}
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return WorkspaceAdmissionSnapshot{}, err
	}
	defer tx.Rollback()
	out := WorkspaceAdmissionSnapshot{}
	out.Idempotency, err = loadIdempotencyLookupTx(ctx, tx, query.ProjectID, query.IdempotencyKind, query.IdempotencyKey)
	if err != nil {
		return WorkspaceAdmissionSnapshot{}, err
	}
	if !out.Idempotency.Found {
		out.ActiveForProject, err = readCapacityTx(ctx, tx, "workspace_capacity", "active_count", query.ProjectID)
		if err != nil {
			return WorkspaceAdmissionSnapshot{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return WorkspaceAdmissionSnapshot{}, err
	}
	return out, nil
}

func (s *SQLiteStore) ReadEnvironmentAdmission(query EnvironmentAdmissionQuery) (EnvironmentAdmissionSnapshot, error) {
	if err := validateAdmissionIdentity(query.ProjectID, query.IdempotencyKind, query.IdempotencyKey); err != nil {
		return EnvironmentAdmissionSnapshot{}, fmt.Errorf("environment admission query: %w", err)
	}
	if query.RevisionID == "" || strings.ContainsAny(query.RevisionID, "\x00\r\n") {
		return EnvironmentAdmissionSnapshot{}, errors.New("environment admission query has invalid revision identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return EnvironmentAdmissionSnapshot{}, errors.New("state store is closed")
	}
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return EnvironmentAdmissionSnapshot{}, err
	}
	defer tx.Rollback()
	out := EnvironmentAdmissionSnapshot{}
	out.Idempotency, err = loadIdempotencyLookupTx(ctx, tx, query.ProjectID, query.IdempotencyKind, query.IdempotencyKey)
	if err != nil {
		return EnvironmentAdmissionSnapshot{}, err
	}
	revisionID := query.RevisionID
	if out.Idempotency.Found {
		revisionID = out.Idempotency.Operation.ResourceID
	}
	out.EnvironmentFound, err = loadAdmissionResourceTx(ctx, tx, resourceEnvironment, query.ProjectID, revisionID, &out.Environment)
	if err != nil {
		return EnvironmentAdmissionSnapshot{}, err
	}
	if out.Idempotency.Found && !out.EnvironmentFound {
		return EnvironmentAdmissionSnapshot{}, errors.New("environment idempotency index refers to a missing resource")
	}
	if !out.Idempotency.Found && !out.EnvironmentFound {
		out.CountForProject, err = readCapacityTx(ctx, tx, "environment_capacity", "resource_count", query.ProjectID)
		if err != nil {
			return EnvironmentAdmissionSnapshot{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return EnvironmentAdmissionSnapshot{}, err
	}
	return out, nil
}

func validateAdmissionIdentity(projectID, kind, key string) error {
	if err := domain.ValidateProjectID(projectID); err != nil {
		return errors.New("invalid project identity")
	}
	if err := validateIdempotencyIndexKey(IdempotencyKey(projectID, kind, key)); err != nil {
		return fmt.Errorf("invalid idempotency identity: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetWorkspace(projectID, workspaceID string) (domain.Workspace, error) {
	var workspace domain.Workspace
	found, err := s.getExactAdmissionResource(resourceWorkspace, projectID, workspaceID, &workspace)
	if err != nil {
		return domain.Workspace{}, err
	}
	if !found {
		return domain.Workspace{}, ErrNotFound
	}
	return workspace, nil
}

func (s *SQLiteStore) GetEnvironment(projectID, revisionID string) (domain.Environment, error) {
	var environment domain.Environment
	found, err := s.getExactAdmissionResource(resourceEnvironment, projectID, revisionID, &environment)
	if err != nil {
		return domain.Environment{}, err
	}
	if !found {
		return domain.Environment{}, ErrNotFound
	}
	return environment, nil
}

func (s *SQLiteStore) getExactAdmissionResource(kind, projectID, resourceID string, destination any) (bool, error) {
	if err := domain.ValidateProjectID(projectID); err != nil || resourceID == "" || strings.ContainsAny(resourceID, "\x00\r\n") {
		return false, errors.New("invalid exact resource identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, errors.New("state store is closed")
	}
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	found, err := loadAdmissionResourceTx(ctx, tx, kind, projectID, resourceID, destination)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return found, nil
}

func (s *SQLiteStore) WorkspaceAttached(projectID, workspaceID string) (bool, error) {
	if err := domain.ValidateProjectID(projectID); err != nil || workspaceID == "" || strings.ContainsAny(workspaceID, "\x00\r\n") {
		return false, errors.New("invalid workspace attachment identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, errors.New("state store is closed")
	}
	var attached int
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM active_workspace_mounts WHERE project_id = ? AND workspace_id = ?)`, projectID, workspaceID).Scan(&attached)
	return attached != 0, err
}

func validateSandboxAdmissionQuery(query SandboxAdmissionQuery) error {
	if err := domain.ValidateProjectID(query.ProjectID); err != nil {
		return errors.New("sandbox admission query has invalid project identity")
	}
	if err := validateIdempotencyIndexKey(IdempotencyKey(query.ProjectID, query.IdempotencyKind, query.IdempotencyKey)); err != nil {
		return fmt.Errorf("sandbox admission query has invalid idempotency identity: %w", err)
	}
	for _, value := range append(append(append([]string{}, query.ConnectorRevisions...), query.WorkspaceIDs...), query.EnvironmentRevision, query.CheckpointID) {
		if strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("sandbox admission query has invalid resource identity")
		}
	}
	return nil
}

func loadIdempotencyLookupTx(ctx context.Context, tx *sql.Tx, projectID, kind, key string) (IdempotencyLookup, error) {
	indexKey := IdempotencyKey(projectID, kind, key)
	if err := validateIdempotencyIndexKey(indexKey); err != nil {
		return IdempotencyLookup{}, fmt.Errorf("invalid idempotency lookup: %w", err)
	}
	var operationID, digest string
	err := tx.QueryRowContext(ctx, `SELECT operation_id, digest FROM idempotency WHERE idempotency_key = ?`, []byte(indexKey)).Scan(&operationID, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return IdempotencyLookup{}, nil
	}
	if err != nil {
		return IdempotencyLookup{}, err
	}
	var operation domain.Operation
	found, err := loadAdmissionResourceTx(ctx, tx, resourceOperation, projectID, operationID, &operation)
	if err != nil {
		return IdempotencyLookup{}, err
	}
	if !found || operation.ProjectID != projectID || operation.ID != operationID || operation.Kind != kind || operation.IdempotencyKey != key {
		return IdempotencyLookup{}, errors.New("idempotency index refers to an inconsistent operation")
	}
	validation := NewState()
	validation.Operations[ScopedKey(projectID, operationID)] = operation
	validation.Idempotency[indexKey] = operationID
	if digest != "" {
		validation.IdempotencyDigests[indexKey] = digest
	}
	if err := validateState(validation); err != nil {
		return IdempotencyLookup{}, err
	}
	return IdempotencyLookup{Operation: operation, Digest: digest, Found: true}, nil
}

func loadAdmissionResourceTx(ctx context.Context, tx *sql.Tx, kind, projectID, resourceID string, destination any) (bool, error) {
	var payload []byte
	err := tx.QueryRowContext(ctx, `SELECT payload FROM resources WHERE kind = ? AND resource_key = ?`, kind, []byte(ScopedKey(projectID, resourceID))).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	state := NewState()
	resourceKey := ScopedKey(projectID, resourceID)
	if err := decodeResource(&state, kind, resourceKey, payload); err != nil {
		return false, fmt.Errorf("decode %s resource: %w", kind, err)
	}
	if err := validateState(state); err != nil {
		return false, fmt.Errorf("validate %s resource: %w", kind, err)
	}
	switch value := destination.(type) {
	case *domain.Environment:
		*value = state.Environments[resourceKey]
	case *domain.Operation:
		*value = state.Operations[resourceKey]
	case *domain.Checkpoint:
		*value = state.Checkpoints[resourceKey]
	case *domain.Workspace:
		*value = state.Workspaces[resourceKey]
	case *domain.Connector:
		*value = state.Connectors[resourceKey]
	default:
		return false, errors.New("unsupported sandbox admission resource destination")
	}
	return true, nil
}

func readSandboxCapacityTx(ctx context.Context, tx *sql.Tx, projectID string) (int, error) {
	return readCapacityTx(ctx, tx, "sandbox_capacity", "active_count", projectID)
}

func readCapacityTx(ctx context.Context, tx *sql.Tx, table, column, projectID string) (int, error) {
	var query string
	switch table + "." + column {
	case "sandbox_capacity.active_count":
		query = `SELECT active_count FROM sandbox_capacity WHERE project_id = ?`
	case "workspace_capacity.active_count":
		query = `SELECT active_count FROM workspace_capacity WHERE project_id = ?`
	case "environment_capacity.resource_count":
		query = `SELECT resource_count FROM environment_capacity WHERE project_id = ?`
	default:
		return 0, errors.New("unsupported capacity index")
	}
	var count int
	err := tx.QueryRowContext(ctx, query, projectID).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if count < 0 {
		return 0, errors.New("sandbox capacity index contains a negative count")
	}
	return count, nil
}

func removeResourceAdmissionIndexTx(ctx context.Context, tx *sql.Tx, kind, key string, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	switch kind {
	case resourceWorkspace:
		var workspace domain.Workspace
		if err := json.Unmarshal(payload, &workspace); err != nil {
			return fmt.Errorf("decode workspace capacity source: %w", err)
		}
		if key != ScopedKey(workspace.ProjectID, workspace.ID) {
			return errors.New("workspace capacity source has inconsistent identity")
		}
		if workspaceCountsTowardCapacity(workspace.State) {
			return changeNamedCapacityTx(ctx, tx, "workspace_capacity", "active_count", workspace.ProjectID, -1)
		}
	case resourceEnvironment:
		var environment domain.Environment
		if err := json.Unmarshal(payload, &environment); err != nil {
			return fmt.Errorf("decode environment capacity source: %w", err)
		}
		if key != ScopedKey(environment.ProjectID, environment.RevisionID) {
			return errors.New("environment capacity source has inconsistent identity")
		}
		return changeNamedCapacityTx(ctx, tx, "environment_capacity", "resource_count", environment.ProjectID, -1)
	}
	return nil
}

func addResourceAdmissionIndexTx(ctx context.Context, tx *sql.Tx, kind, key string, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	switch kind {
	case resourceWorkspace:
		var workspace domain.Workspace
		if err := json.Unmarshal(payload, &workspace); err != nil {
			return fmt.Errorf("decode workspace capacity destination: %w", err)
		}
		if key != ScopedKey(workspace.ProjectID, workspace.ID) {
			return errors.New("workspace capacity destination has inconsistent identity")
		}
		if workspaceCountsTowardCapacity(workspace.State) {
			return changeNamedCapacityTx(ctx, tx, "workspace_capacity", "active_count", workspace.ProjectID, 1)
		}
	case resourceEnvironment:
		var environment domain.Environment
		if err := json.Unmarshal(payload, &environment); err != nil {
			return fmt.Errorf("decode environment capacity destination: %w", err)
		}
		if key != ScopedKey(environment.ProjectID, environment.RevisionID) {
			return errors.New("environment capacity destination has inconsistent identity")
		}
		return changeNamedCapacityTx(ctx, tx, "environment_capacity", "resource_count", environment.ProjectID, 1)
	}
	return nil
}

func workspaceCountsTowardCapacity(state domain.WorkspaceState) bool {
	return state != domain.WorkspaceDeleted && state != domain.WorkspaceFailed
}

func changeNamedCapacityTx(ctx context.Context, tx *sql.Tx, table, column, projectID string, delta int) error {
	var insert, update, selectCount string
	switch table + "." + column {
	case "workspace_capacity.active_count":
		insert = `INSERT INTO workspace_capacity(project_id, active_count) VALUES (?, 0) ON CONFLICT(project_id) DO NOTHING`
		update = `UPDATE workspace_capacity SET active_count = active_count + ? WHERE project_id = ?`
		selectCount = `SELECT active_count FROM workspace_capacity WHERE project_id = ?`
	case "environment_capacity.resource_count":
		insert = `INSERT INTO environment_capacity(project_id, resource_count) VALUES (?, 0) ON CONFLICT(project_id) DO NOTHING`
		update = `UPDATE environment_capacity SET resource_count = resource_count + ? WHERE project_id = ?`
		selectCount = `SELECT resource_count FROM environment_capacity WHERE project_id = ?`
	default:
		return errors.New("unsupported capacity index")
	}
	if _, err := tx.ExecContext(ctx, insert, projectID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, update, delta, projectID); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, selectCount, projectID).Scan(&count); err != nil {
		return err
	}
	if count < 0 {
		return errors.New("resource capacity index would become negative")
	}
	return nil
}

func removeSandboxAdmissionIndexesTx(ctx context.Context, tx *sql.Tx, key string, payload []byte) error {
	if len(payload) > 0 {
		var sandbox domain.Sandbox
		if err := json.Unmarshal(payload, &sandbox); err != nil {
			return fmt.Errorf("decode sandbox capacity source: %w", err)
		}
		if key != ScopedKey(sandbox.ProjectID, sandbox.ID) {
			return errors.New("sandbox admission index source has inconsistent identity")
		}
		if sandboxCountsTowardCapacity(sandbox.State) {
			if err := changeSandboxCapacityTx(ctx, tx, totalSandboxCapacityKey, -1); err != nil {
				return err
			}
			if err := changeSandboxCapacityTx(ctx, tx, sandbox.ProjectID, -1); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM active_workspace_mounts WHERE sandbox_key = ?`, []byte(key)); err != nil {
		return err
	}
	return nil
}

func addSandboxAdmissionIndexesTx(ctx context.Context, tx *sql.Tx, key string, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	var sandbox domain.Sandbox
	if err := json.Unmarshal(payload, &sandbox); err != nil {
		return fmt.Errorf("decode sandbox capacity destination: %w", err)
	}
	if key != ScopedKey(sandbox.ProjectID, sandbox.ID) {
		return errors.New("sandbox admission index destination has inconsistent identity")
	}
	if sandboxCountsTowardCapacity(sandbox.State) {
		if err := changeSandboxCapacityTx(ctx, tx, totalSandboxCapacityKey, 1); err != nil {
			return err
		}
		if err := changeSandboxCapacityTx(ctx, tx, sandbox.ProjectID, 1); err != nil {
			return err
		}
		for _, mount := range sandbox.WorkspaceMounts {
			if _, err := tx.ExecContext(ctx, `INSERT INTO active_workspace_mounts(project_id, workspace_id, sandbox_key) VALUES (?, ?, ?)`, sandbox.ProjectID, mount.WorkspaceID, []byte(key)); err != nil {
				return err
			}
		}
	}
	return nil
}

func changeSandboxCapacityTx(ctx context.Context, tx *sql.Tx, projectID string, delta int) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO sandbox_capacity(project_id, active_count) VALUES (?, 0)
		ON CONFLICT(project_id) DO NOTHING`, projectID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sandbox_capacity SET active_count = active_count + ? WHERE project_id = ?`, delta, projectID); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT active_count FROM sandbox_capacity WHERE project_id = ?`, projectID).Scan(&count); err != nil {
		return err
	}
	if count < 0 {
		return errors.New("sandbox capacity index would become negative")
	}
	return nil
}

func rebuildSandboxAdmissionIndexesTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM sandbox_capacity; DELETE FROM workspace_capacity; DELETE FROM environment_capacity; DELETE FROM active_workspace_mounts`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sandbox_capacity(project_id, active_count) VALUES (?, 0)`, totalSandboxCapacityKey); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT resource_key, payload FROM resources WHERE kind = ?`, resourceSandbox)
	if err != nil {
		return err
	}
	type row struct {
		key     string
		payload []byte
	}
	var sandboxes []row
	for rows.Next() {
		var key, payload []byte
		if err := rows.Scan(&key, &payload); err != nil {
			rows.Close()
			return err
		}
		sandboxes = append(sandboxes, row{key: string(key), payload: append([]byte(nil), payload...)})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, sandbox := range sandboxes {
		if err := addSandboxAdmissionIndexesTx(ctx, tx, sandbox.key, sandbox.payload); err != nil {
			return err
		}
	}
	for _, kind := range []string{resourceWorkspace, resourceEnvironment} {
		rows, err := tx.QueryContext(ctx, `SELECT resource_key, payload FROM resources WHERE kind = ?`, kind)
		if err != nil {
			return err
		}
		var resources []row
		for rows.Next() {
			var key, payload []byte
			if err := rows.Scan(&key, &payload); err != nil {
				rows.Close()
				return err
			}
			resources = append(resources, row{key: string(key), payload: append([]byte(nil), payload...)})
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, resource := range resources {
			if err := addResourceAdmissionIndexTx(ctx, tx, kind, resource.key, resource.payload); err != nil {
				return err
			}
		}
	}
	return nil
}

func sandboxCountsTowardCapacity(state domain.SandboxState) bool {
	return state != domain.SandboxDeleted && state != domain.SandboxExpired && state != domain.SandboxFailed
}
