package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/infercrane/brezel/internal/domain"
)

const (
	lifecycleOperationHeadsProjectionMetadataKey = "lifecycle_operation_heads_projection_version"
	lifecycleOperationHeadsProjectionVersion     = 1

	lifecycleOwnerSandbox   = "sandbox"
	lifecycleOwnerWorkspace = "workspace"

	lifecycleFamilySandboxCreate    = "create_sandbox"
	lifecycleFamilySandboxResume    = "resume_sandbox"
	lifecycleFamilySandboxPause     = "pause_sandbox"
	lifecycleFamilySandboxAutoPause = "auto_pause_sandbox"
	lifecycleFamilySandboxDelete    = "delete_sandbox"
	lifecycleFamilyWorkspaceCreate  = "create_workspace"
	lifecycleFamilyWorkspaceDelete  = "delete_workspace"
)

func ensureLifecycleOperationHeadsTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var tableExists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'lifecycle_operation_heads'
	)`).Scan(&tableExists); err != nil {
		return false, err
	}
	var versionText string
	markerErr := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key = ?`, lifecycleOperationHeadsProjectionMetadataKey).Scan(&versionText)
	switch {
	case markerErr == nil:
		version, err := strconv.Atoi(versionText)
		if err != nil || version < 1 {
			return false, fmt.Errorf("invalid lifecycle operation recovery index version %q", versionText)
		}
		if version > lifecycleOperationHeadsProjectionVersion {
			return false, fmt.Errorf("unsupported lifecycle operation recovery index version %d (runtime supports %d)", version, lifecycleOperationHeadsProjectionVersion)
		}
	case !errors.Is(markerErr, sql.ErrNoRows):
		return false, markerErr
	}

	rebuild := tableExists == 0 || markerErr != nil || versionText != strconv.Itoa(lifecycleOperationHeadsProjectionVersion)
	if rebuild {
		if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS lifecycle_operation_heads;
			CREATE TABLE lifecycle_operation_heads (
				project_id TEXT NOT NULL,
				owner_kind TEXT NOT NULL CHECK (owner_kind IN ('sandbox', 'workspace')),
				resource_id TEXT NOT NULL,
				family TEXT NOT NULL,
				operation_id TEXT NOT NULL,
				operation_resource_kind TEXT NOT NULL DEFAULT 'operation' CHECK (operation_resource_kind = 'operation'),
				operation_key BLOB NOT NULL,
				PRIMARY KEY (project_id, owner_kind, resource_id, family),
				FOREIGN KEY (operation_resource_kind, operation_key) REFERENCES resources(kind, resource_key) ON DELETE CASCADE
			) WITHOUT ROWID;
			CREATE INDEX lifecycle_operation_heads_operation_key
				ON lifecycle_operation_heads(operation_resource_kind, operation_key)`); err != nil {
			return false, err
		}
		return true, nil
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS lifecycle_operation_heads_operation_key
		ON lifecycle_operation_heads(operation_resource_kind, operation_key)`); err != nil {
		return false, err
	}
	return false, nil
}

func recordLifecycleOperationHeadsVersionTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO metadata(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		lifecycleOperationHeadsProjectionMetadataKey, strconv.Itoa(lifecycleOperationHeadsProjectionVersion))
	return err
}

type lifecycleProjectionChange struct {
	kind          string
	key           string
	before, after []byte
}

type lifecycleHeadKey struct {
	projectID  string
	ownerKind  string
	resourceID string
	family     string
}

func lifecycleHeadForOperation(operation domain.Operation) (lifecycleHeadKey, bool) {
	key := lifecycleHeadKey{projectID: operation.ProjectID, resourceID: operation.ResourceID}
	switch operation.Kind {
	case "create_sandbox":
		key.ownerKind, key.family = lifecycleOwnerSandbox, lifecycleFamilySandboxCreate
	case "resume_sandbox:" + operation.ResourceID:
		key.ownerKind, key.family = lifecycleOwnerSandbox, lifecycleFamilySandboxResume
	case "pause_sandbox:" + operation.ResourceID:
		key.ownerKind, key.family = lifecycleOwnerSandbox, lifecycleFamilySandboxPause
	case "auto_pause_sandbox:" + operation.ResourceID:
		key.ownerKind, key.family = lifecycleOwnerSandbox, lifecycleFamilySandboxAutoPause
	case "delete_sandbox:" + operation.ResourceID:
		key.ownerKind, key.family = lifecycleOwnerSandbox, lifecycleFamilySandboxDelete
	case "create_workspace":
		key.ownerKind, key.family = lifecycleOwnerWorkspace, lifecycleFamilyWorkspaceCreate
	case "delete_workspace:" + operation.ResourceID:
		key.ownerKind, key.family = lifecycleOwnerWorkspace, lifecycleFamilyWorkspaceDelete
	default:
		return lifecycleHeadKey{}, false
	}
	return key, true
}

func lifecycleOperationIsNewer(candidate, current domain.Operation) bool {
	if candidate.UpdatedAt.After(current.UpdatedAt) {
		return true
	}
	if candidate.UpdatedAt.Before(current.UpdatedAt) {
		return false
	}
	if candidate.CreatedAt.After(current.CreatedAt) {
		return true
	}
	if candidate.CreatedAt.Before(current.CreatedAt) {
		return false
	}
	return candidate.ID > current.ID
}

func rebuildLifecycleOperationHeadsTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM lifecycle_operation_heads`); err != nil {
		return err
	}
	heads, err := selectLifecycleOperationHeadsTx(ctx, tx, nil)
	if err != nil {
		return err
	}
	for _, key := range sortedLifecycleHeadKeys(heads) {
		if err := upsertLifecycleOperationHeadTx(ctx, tx, heads[key]); err != nil {
			return err
		}
	}
	return nil
}

func syncLifecycleOperationHeadsTx(ctx context.Context, tx *sql.Tx, operationChanges, ownerChanges []lifecycleProjectionChange) error {
	dirty := make(map[lifecycleHeadKey]struct{})
	inserted := make(map[lifecycleHeadKey]domain.Operation)
	for _, change := range operationChanges {
		before, beforeKey, beforeLifecycle, err := decodeLifecycleOperation(change.before)
		if err != nil {
			return fmt.Errorf("decode previous lifecycle operation projection: %w", err)
		}
		after, afterKey, afterLifecycle, err := decodeLifecycleOperation(change.after)
		if err != nil {
			return fmt.Errorf("decode lifecycle operation projection: %w", err)
		}
		if beforeLifecycle && (!afterLifecycle || beforeKey != afterKey || before.ID != after.ID) {
			current, exists, err := lifecycleOperationIsCurrentHeadTx(ctx, tx, beforeKey, before.ID)
			if err != nil {
				return err
			}
			// Deleting the referenced operation cascades the head row before this
			// projection pass. A missing head therefore also requires fallback
			// promotion; deleting a known non-head does not.
			if current || !exists {
				dirty[beforeKey] = struct{}{}
			}
		}
		// The durable row insertion, not wall-clock time, defines which lifecycle
		// attempt is current. This keeps a valid operation visible after a host
		// clock step. Updates retain their existing head position, so completing or
		// retrying an older non-head operation cannot steal the family head.
		newMembership := afterLifecycle && (!beforeLifecycle || beforeKey != afterKey || before.ID != after.ID)
		if newMembership {
			if current, exists := inserted[afterKey]; !exists || lifecycleOperationIsNewer(after, current) {
				inserted[afterKey] = after
			}
		}
	}
	for _, key := range sortedLifecycleHeadKeys(dirty) {
		if err := refreshLifecycleOperationHeadTx(ctx, tx, key); err != nil {
			return err
		}
	}
	for _, key := range sortedLifecycleHeadKeys(inserted) {
		if err := replaceLifecycleOperationHeadTx(ctx, tx, inserted[key]); err != nil {
			return err
		}
	}
	for _, change := range ownerChanges {
		_, beforeKey, err := lifecycleOwnerEligibility(change.kind, change.before)
		if err != nil {
			return fmt.Errorf("decode previous lifecycle owner projection: %w", err)
		}
		afterEligible, afterKey, err := lifecycleOwnerEligibility(change.kind, change.after)
		if err != nil {
			return fmt.Errorf("decode lifecycle owner projection: %w", err)
		}
		if beforeKey.projectID != "" && (!afterEligible || beforeKey != afterKey) {
			if err := deleteLifecycleOwnerHeadsTx(ctx, tx, beforeKey); err != nil {
				return err
			}
		}
		// Entering the reconciliation set does not resurrect historical work.
		// Lifecycle transitions write their operation in the same transaction,
		// and the candidate pass above installs its head after the authoritative
		// owner row exists. Existing databases are rebuilt once during open.
		// Avoiding an owner-wide scan here keeps admission independent of retained
		// operation history.
	}
	return nil
}

func lifecycleOperationIsCurrentHeadTx(ctx context.Context, tx *sql.Tx, key lifecycleHeadKey, operationID string) (bool, bool, error) {
	var currentID string
	err := tx.QueryRowContext(ctx, `SELECT operation_id FROM lifecycle_operation_heads
		WHERE project_id = ? AND owner_kind = ? AND resource_id = ? AND family = ?`,
		key.projectID, key.ownerKind, key.resourceID, key.family).Scan(&currentID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	return currentID == operationID, true, err
}

func decodeLifecycleOperation(payload []byte) (domain.Operation, lifecycleHeadKey, bool, error) {
	if len(payload) == 0 {
		return domain.Operation{}, lifecycleHeadKey{}, false, nil
	}
	var operation domain.Operation
	if err := json.Unmarshal(payload, &operation); err != nil {
		return domain.Operation{}, lifecycleHeadKey{}, false, err
	}
	key, lifecycle := lifecycleHeadForOperation(operation)
	return operation, key, lifecycle, nil
}

func upsertLifecycleOperationHeadTx(ctx context.Context, tx *sql.Tx, candidate domain.Operation) error {
	key, lifecycle := lifecycleHeadForOperation(candidate)
	if !lifecycle {
		return nil
	}
	eligible, err := lifecycleOwnerIsEligibleTx(ctx, tx, key)
	if err != nil {
		return err
	}
	if !eligible {
		return deleteLifecycleOwnerHeadsTx(ctx, tx, key)
	}
	var currentPayload []byte
	err = tx.QueryRowContext(ctx, `SELECT resources.payload
		FROM lifecycle_operation_heads AS heads
		JOIN resources ON resources.kind = heads.operation_resource_kind AND resources.resource_key = heads.operation_key
		WHERE heads.project_id = ? AND heads.owner_kind = ? AND heads.resource_id = ? AND heads.family = ?`,
		key.projectID, key.ownerKind, key.resourceID, key.family).Scan(&currentPayload)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		var current domain.Operation
		if err := json.Unmarshal(currentPayload, &current); err != nil {
			return fmt.Errorf("decode current lifecycle operation head: %w", err)
		}
		if candidate.ID != current.ID && !lifecycleOperationIsNewer(candidate, current) {
			return nil
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO lifecycle_operation_heads(project_id, owner_kind, resource_id, family, operation_id, operation_key)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, owner_kind, resource_id, family) DO UPDATE SET operation_id = excluded.operation_id, operation_key = excluded.operation_key`,
		key.projectID, key.ownerKind, key.resourceID, key.family, candidate.ID, []byte(ScopedKey(candidate.ProjectID, candidate.ID)))
	return err
}

func replaceLifecycleOperationHeadTx(ctx context.Context, tx *sql.Tx, candidate domain.Operation) error {
	key, lifecycle := lifecycleHeadForOperation(candidate)
	if !lifecycle {
		return nil
	}
	eligible, err := lifecycleOwnerIsEligibleTx(ctx, tx, key)
	if err != nil {
		return err
	}
	if !eligible {
		return deleteLifecycleOwnerHeadsTx(ctx, tx, key)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO lifecycle_operation_heads(project_id, owner_kind, resource_id, family, operation_id, operation_key)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, owner_kind, resource_id, family) DO UPDATE SET operation_id = excluded.operation_id, operation_key = excluded.operation_key`,
		key.projectID, key.ownerKind, key.resourceID, key.family, candidate.ID, []byte(ScopedKey(candidate.ProjectID, candidate.ID)))
	return err
}

func refreshLifecycleOperationHeadTx(ctx context.Context, tx *sql.Tx, key lifecycleHeadKey) error {
	if err := deleteLifecycleOperationHeadTx(ctx, tx, key); err != nil {
		return err
	}
	heads, err := selectLifecycleOperationHeadsTx(ctx, tx, &key)
	if err != nil {
		return err
	}
	if operation, ok := heads[key]; ok {
		return upsertLifecycleOperationHeadTx(ctx, tx, operation)
	}
	return nil
}

func selectLifecycleOperationHeadsTx(ctx context.Context, tx *sql.Tx, only *lifecycleHeadKey) (map[lifecycleHeadKey]domain.Operation, error) {
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM resources WHERE kind = ?`, resourceOperation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	heads := make(map[lifecycleHeadKey]domain.Operation)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var operation domain.Operation
		if err := json.Unmarshal(payload, &operation); err != nil {
			return nil, fmt.Errorf("decode operation while rebuilding lifecycle recovery index: %w", err)
		}
		key, lifecycle := lifecycleHeadForOperation(operation)
		if !lifecycle || (only != nil && key != *only) {
			continue
		}
		if current, exists := heads[key]; !exists || lifecycleOperationIsNewer(operation, current) {
			heads[key] = operation
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return heads, nil
}

func lifecycleOwnerIsEligibleTx(ctx context.Context, tx *sql.Tx, key lifecycleHeadKey) (bool, error) {
	resourceKind := resourceSandbox
	if key.ownerKind == lifecycleOwnerWorkspace {
		resourceKind = resourceWorkspace
	}
	var payload []byte
	err := tx.QueryRowContext(ctx, `SELECT payload FROM resources WHERE kind = ? AND resource_key = ?`, resourceKind, []byte(ScopedKey(key.projectID, key.resourceID))).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	eligible, _, err := lifecycleOwnerEligibility(resourceKind, payload)
	return eligible, err
}

func lifecycleOwnerEligibility(kind string, payload []byte) (bool, lifecycleHeadKey, error) {
	if len(payload) == 0 {
		return false, lifecycleHeadKey{}, nil
	}
	switch kind {
	case resourceSandbox:
		var sandbox domain.Sandbox
		if err := json.Unmarshal(payload, &sandbox); err != nil {
			return false, lifecycleHeadKey{}, err
		}
		terminal := sandbox.State == domain.SandboxDeleted || sandbox.State == domain.SandboxExpired || sandbox.State == domain.SandboxFailed
		return !terminal, lifecycleHeadKey{projectID: sandbox.ProjectID, ownerKind: lifecycleOwnerSandbox, resourceID: sandbox.ID}, nil
	case resourceWorkspace:
		var workspace domain.Workspace
		if err := json.Unmarshal(payload, &workspace); err != nil {
			return false, lifecycleHeadKey{}, err
		}
		settledOrTerminal := workspace.State == domain.WorkspaceReady || workspace.State == domain.WorkspaceDeleted || workspace.State == domain.WorkspaceFailed
		return !settledOrTerminal, lifecycleHeadKey{projectID: workspace.ProjectID, ownerKind: lifecycleOwnerWorkspace, resourceID: workspace.ID}, nil
	default:
		return false, lifecycleHeadKey{}, fmt.Errorf("unsupported lifecycle owner kind %q", kind)
	}
}

func deleteLifecycleOperationHeadTx(ctx context.Context, tx *sql.Tx, key lifecycleHeadKey) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM lifecycle_operation_heads WHERE project_id = ? AND owner_kind = ? AND resource_id = ? AND family = ?`, key.projectID, key.ownerKind, key.resourceID, key.family)
	return err
}

func deleteLifecycleOwnerHeadsTx(ctx context.Context, tx *sql.Tx, owner lifecycleHeadKey) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM lifecycle_operation_heads WHERE project_id = ? AND owner_kind = ? AND resource_id = ?`, owner.projectID, owner.ownerKind, owner.resourceID)
	return err
}

func sortedLifecycleHeadKeys[T any](values map[lifecycleHeadKey]T) []lifecycleHeadKey {
	keys := make([]lifecycleHeadKey, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].projectID != keys[j].projectID {
			return keys[i].projectID < keys[j].projectID
		}
		if keys[i].ownerKind != keys[j].ownerKind {
			return keys[i].ownerKind < keys[j].ownerKind
		}
		if keys[i].resourceID != keys[j].resourceID {
			return keys[i].resourceID < keys[j].resourceID
		}
		return keys[i].family < keys[j].family
	})
	return keys
}
