package store

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/infercrane/brezel/internal/domain"
)

func TestSQLiteStoreReadSandboxAdmissionReturnsIndexedDependenciesAndCounts(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := makeBenchmarkState(3)
	other := state.Sandboxes[ScopedKey("project-bench", "sbx-000000")]
	other.ID = "sbx-other"
	other.ProjectID = "project-other"
	other.WorkspaceMounts = nil
	state.Sandboxes[ScopedKey(other.ProjectID, other.ID)] = other
	terminal := other
	terminal.ID = "sbx-terminal"
	terminal.State = domain.SandboxFailed
	state.Sandboxes[ScopedKey(terminal.ProjectID, terminal.ID)] = terminal
	unattached := state.Workspaces[ScopedKey("project-bench", "ws-000000")]
	unattached.ID = "ws-unattached"
	unattached.BackendName = "workspace-unattached"
	state.Workspaces[ScopedKey(unattached.ProjectID, unattached.ID)] = unattached
	if err := s.Update(func(next *State) error {
		*next = state
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ReadSandboxAdmission(SandboxAdmissionQuery{
		ProjectID: "project-bench", IdempotencyKind: "create_sandbox", IdempotencyKey: "request-admission-0001",
		CheckpointID: "chk-000001", ConnectorRevisions: []string{"con-000001"}, WorkspaceIDs: []string{"ws-000001", "ws-unattached"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ActiveForProject != 3 || got.ActiveTotal != 4 {
		t.Fatalf("capacity = project %d total %d, want 3 and 4", got.ActiveForProject, got.ActiveTotal)
	}
	if !got.CheckpointFound || got.Checkpoint.ID != "chk-000001" {
		t.Fatalf("checkpoint = %#v, found=%t", got.Checkpoint, got.CheckpointFound)
	}
	if !got.EnvironmentFound || got.Environment.RevisionID != "env-000001" {
		t.Fatalf("environment = %#v, found=%t", got.Environment, got.EnvironmentFound)
	}
	if got.Connectors["con-000001"].RevisionID != "con-000001" || got.Workspaces["ws-000001"].ID != "ws-000001" {
		t.Fatalf("dependencies = connectors %#v workspaces %#v", got.Connectors, got.Workspaces)
	}
	if !got.AttachedWorkspaceIDs["ws-000001"] || got.AttachedWorkspaceIDs["ws-unattached"] {
		t.Fatalf("workspace attachments = %#v", got.AttachedWorkspaceIDs)
	}
}

func TestSQLiteStoreReadSandboxAdmissionPreservesReplayFirstBehavior(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := makeBenchmarkState(1)
	op := domain.Operation{
		ID: "op-replay", ProjectID: "project-bench", Kind: "create_sandbox", ResourceID: "sbx-000000",
		State: domain.OperationSucceeded, IdempotencyKey: "request-replay-0001",
		CreatedAt: state.Sandboxes[ScopedKey("project-bench", "sbx-000000")].CreatedAt,
		UpdatedAt: state.Sandboxes[ScopedKey("project-bench", "sbx-000000")].UpdatedAt,
	}
	indexKey := IdempotencyKey(op.ProjectID, op.Kind, op.IdempotencyKey)
	state.Operations[ScopedKey(op.ProjectID, op.ID)] = op
	state.Idempotency[indexKey] = op.ID
	state.IdempotencyDigests[indexKey] = strings.Repeat("a", 64)
	if err := s.Update(func(next *State) error { *next = state; return nil }); err != nil {
		t.Fatal(err)
	}

	got, err := s.ReadSandboxAdmission(SandboxAdmissionQuery{
		ProjectID: op.ProjectID, IdempotencyKind: op.Kind, IdempotencyKey: op.IdempotencyKey,
		EnvironmentRevision: "missing", ConnectorRevisions: []string{"missing"}, WorkspaceIDs: []string{"missing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Idempotency.Found || got.Idempotency.Operation.ID != op.ID || got.Idempotency.Digest != strings.Repeat("a", 64) {
		t.Fatalf("idempotency replay = %#v", got.Idempotency)
	}
	if got.ActiveTotal != 0 || got.EnvironmentFound || len(got.Connectors) != 0 || len(got.Workspaces) != 0 {
		t.Fatalf("replay unnecessarily loaded admission dependencies: %#v", got)
	}
}

func TestSQLiteStoreLookupIdempotencyRejectsOperationOutsideNamespace(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := makeBenchmarkState(1)
	now := state.Sandboxes[ScopedKey("project-bench", "sbx-000000")].CreatedAt
	op := domain.Operation{
		ID: "op-wrong-kind", ProjectID: "project-bench", Kind: "create_workspace", ResourceID: "ws-000000",
		State: domain.OperationSucceeded, IdempotencyKey: "request-namespace-0001", CreatedAt: now, UpdatedAt: now,
	}
	// Insert a semantically corrupt cross-kind claim directly. The normal store
	// mutation API rejects this shape before commit.
	opPayload := []byte(fmt.Sprintf(`{"id":%q,"project_id":%q,"kind":%q,"resource_id":%q,"state":%q,"idempotency_key":%q,"created_at":%q,"updated_at":%q}`,
		op.ID, op.ProjectID, op.Kind, op.ResourceID, op.State, op.IdempotencyKey, op.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), op.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")))
	if _, err := s.db.Exec(`INSERT INTO resources(kind, resource_key, payload) VALUES (?, ?, ?)`, resourceOperation, []byte(ScopedKey(op.ProjectID, op.ID)), opPayload); err != nil {
		t.Fatal(err)
	}
	requestedKind := "create_sandbox"
	indexKey := IdempotencyKey(op.ProjectID, requestedKind, op.IdempotencyKey)
	if _, err := s.db.Exec(`INSERT INTO idempotency(idempotency_key, operation_id, digest) VALUES (?, ?, '')`, []byte(indexKey), op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupIdempotency(op.ProjectID, requestedKind, op.IdempotencyKey); err == nil || !strings.Contains(err.Error(), "inconsistent operation") {
		t.Fatalf("cross-kind idempotency lookup error = %v", err)
	}
}

func TestSQLiteStoreWorkspaceAdmissionAndExactReadsUseDerivedIndexes(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := makeBenchmarkState(3)
	terminalKey := ScopedKey("project-bench", "ws-000002")
	terminal := state.Workspaces[terminalKey]
	terminal.State = domain.WorkspaceDeleted
	state.Workspaces[terminalKey] = terminal
	if err := s.Update(func(next *State) error { *next = state; return nil }); err != nil {
		t.Fatal(err)
	}

	admission, err := s.ReadWorkspaceAdmission(WorkspaceAdmissionQuery{
		ProjectID: "project-bench", IdempotencyKind: "create_workspace", IdempotencyKey: "workspace-admission-0001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if admission.ActiveForProject != 2 || admission.Idempotency.Found {
		t.Fatalf("workspace admission = %#v, want two active and no replay", admission)
	}
	workspace, err := s.GetWorkspace("project-bench", "ws-000001")
	if err != nil || workspace.ID != "ws-000001" {
		t.Fatalf("workspace = %#v, %v", workspace, err)
	}
	attached, err := s.WorkspaceAttached("project-bench", "ws-000001")
	if err != nil || !attached {
		t.Fatalf("workspace attached = %t, %v", attached, err)
	}
}

func TestSQLiteStoreWorkspaceAdmissionPreservesReplayFirstBehavior(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := makeBenchmarkState(1)
	now := state.Workspaces[ScopedKey("project-bench", "ws-000000")].CreatedAt
	op := domain.Operation{
		ID: "op-workspace-replay", ProjectID: "project-bench", Kind: "create_workspace", ResourceID: "ws-000000",
		State: domain.OperationSucceeded, IdempotencyKey: "workspace-replay-0001", CreatedAt: now, UpdatedAt: now,
	}
	indexKey := IdempotencyKey(op.ProjectID, op.Kind, op.IdempotencyKey)
	state.Operations[ScopedKey(op.ProjectID, op.ID)] = op
	state.Idempotency[indexKey] = op.ID
	state.IdempotencyDigests[indexKey] = strings.Repeat("c", 64)
	if err := s.Update(func(next *State) error { *next = state; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE workspace_capacity SET active_count = 999 WHERE project_id = ?`, op.ProjectID); err != nil {
		t.Fatal(err)
	}

	got, err := s.ReadWorkspaceAdmission(WorkspaceAdmissionQuery{
		ProjectID: op.ProjectID, IdempotencyKind: op.Kind, IdempotencyKey: op.IdempotencyKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Idempotency.Found || got.Idempotency.Operation.ID != op.ID || got.ActiveForProject != 0 {
		t.Fatalf("workspace replay = %#v", got)
	}
}

func TestSQLiteStoreEnvironmentAdmissionReusesExactRevisionAndCountsProject(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := makeBenchmarkState(3)
	other := state.Environments[ScopedKey("project-bench", "env-000000")]
	other.ProjectID = "project-other"
	other.RevisionID = "env-other"
	state.Environments[ScopedKey(other.ProjectID, other.RevisionID)] = other
	if err := s.Update(func(next *State) error { *next = state; return nil }); err != nil {
		t.Fatal(err)
	}

	existing, err := s.ReadEnvironmentAdmission(EnvironmentAdmissionQuery{
		ProjectID: "project-bench", IdempotencyKind: "create_environment", IdempotencyKey: "environment-existing-0001", RevisionID: "env-000001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !existing.EnvironmentFound || existing.Environment.RevisionID != "env-000001" || existing.CountForProject != 0 {
		t.Fatalf("existing environment admission = %#v", existing)
	}
	missing, err := s.ReadEnvironmentAdmission(EnvironmentAdmissionQuery{
		ProjectID: "project-bench", IdempotencyKind: "create_environment", IdempotencyKey: "environment-missing-0001", RevisionID: "env-missing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if missing.EnvironmentFound || missing.CountForProject != 3 {
		t.Fatalf("missing environment admission = %#v", missing)
	}
	got, err := s.GetEnvironment("project-bench", "env-000002")
	if err != nil || got.RevisionID != "env-000002" {
		t.Fatalf("environment = %#v, %v", got, err)
	}
}

func TestSQLiteStoreWorkspaceCapacityRemainsConsistentDuringConcurrentTransitions(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const resources = 64
	state := makeBenchmarkState(resources)
	if err := s.Update(func(next *State) error { *next = state; return nil }); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, resources*2)
	for index := 0; index < resources; index++ {
		index := index
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, readErr := s.ReadWorkspaceAdmission(WorkspaceAdmissionQuery{
				ProjectID: "project-bench", IdempotencyKind: "create_workspace", IdempotencyKey: fmt.Sprintf("workspace-concurrent-%04d", index),
			})
			errs <- readErr
		}()
		go func() {
			defer wg.Done()
			if index%2 != 0 {
				errs <- nil
				return
			}
			key := ScopedKey("project-bench", fmt.Sprintf("ws-%06d", index))
			errs <- s.UpdateRows(MutationScope{Workspaces: []string{key}}, func(scoped *State) error {
				workspace := scoped.Workspaces[key]
				workspace.State = domain.WorkspaceFailed
				scoped.Workspaces[key] = workspace
				return nil
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ReadWorkspaceAdmission(WorkspaceAdmissionQuery{
		ProjectID: "project-bench", IdempotencyKind: "create_workspace", IdempotencyKey: "workspace-concurrent-final-0001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ActiveForProject != resources/2 {
		t.Fatalf("active workspaces = %d, want %d", got.ActiveForProject, resources/2)
	}
}

func TestSQLiteStoreAdmissionReadDoesNotDecodeUnrelatedLedgerRows(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := makeBenchmarkState(1)
	if err := s.Update(func(next *State) error { *next = state; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO resources(kind, resource_key, payload) VALUES (?, ?, ?)`, resourceOperation, []byte(ScopedKey("project-unrelated", "broken")), []byte(`{"id":`)); err != nil {
		t.Fatal(err)
	}

	got, err := s.ReadSandboxAdmission(SandboxAdmissionQuery{
		ProjectID: "project-bench", IdempotencyKind: "create_sandbox", IdempotencyKey: "request-unrelated-0001", EnvironmentRevision: "env-000000",
	})
	if err != nil {
		t.Fatalf("admission decoded unrelated ledger history: %v", err)
	}
	if !got.EnvironmentFound || got.ActiveForProject != 1 || got.ActiveTotal != 1 {
		t.Fatalf("admission = %#v", got)
	}
}

func TestSQLiteStoreAdmissionIndexesRemainConsistentDuringConcurrentTransitions(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const resources = 64
	state := makeBenchmarkState(resources)
	if err := s.Update(func(next *State) error { *next = state; return nil }); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, resources*2)
	for index := 0; index < resources; index++ {
		index := index
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, readErr := s.ReadSandboxAdmission(SandboxAdmissionQuery{
				ProjectID: "project-bench", IdempotencyKind: "create_sandbox", IdempotencyKey: fmt.Sprintf("concurrent-read-%04d", index), EnvironmentRevision: "env-000000",
			})
			errs <- readErr
		}()
		go func() {
			defer wg.Done()
			if index%2 != 0 {
				errs <- nil
				return
			}
			key := ScopedKey("project-bench", fmt.Sprintf("sbx-%06d", index))
			errs <- s.UpdateRows(MutationScope{Sandboxes: []string{key}}, func(scoped *State) error {
				sandbox := scoped.Sandboxes[key]
				sandbox.State = domain.SandboxFailed
				sandbox.Revision++
				scoped.Sandboxes[key] = sandbox
				return nil
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ReadSandboxAdmission(SandboxAdmissionQuery{
		ProjectID: "project-bench", IdempotencyKind: "create_sandbox", IdempotencyKey: "concurrent-final-0001", EnvironmentRevision: "env-000001", WorkspaceIDs: []string{"ws-000000", "ws-000001"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ActiveForProject != resources/2 || got.ActiveTotal != resources/2 {
		t.Fatalf("capacity = project %d total %d, want %d", got.ActiveForProject, got.ActiveTotal, resources/2)
	}
	if got.AttachedWorkspaceIDs["ws-000000"] || !got.AttachedWorkspaceIDs["ws-000001"] {
		t.Fatalf("active workspace attachment index = %#v", got.AttachedWorkspaceIDs)
	}
}

func TestSQLiteStoreRebuildsAdmissionIndexesWhenReopened(t *testing.T) {
	path := privateTestPath(t, "state.db")
	s, err := OpenSQLite(path, "")
	if err != nil {
		t.Fatal(err)
	}
	state := makeBenchmarkState(4)
	if err := s.Update(func(next *State) error { *next = state; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM sandbox_capacity; DELETE FROM workspace_capacity; DELETE FROM environment_capacity; DELETE FROM active_workspace_mounts`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLite(path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.ReadSandboxAdmission(SandboxAdmissionQuery{
		ProjectID: "project-bench", IdempotencyKind: "create_sandbox", IdempotencyKey: "reopen-admission-0001", EnvironmentRevision: "env-000000", WorkspaceIDs: []string{"ws-000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ActiveForProject != 4 || got.ActiveTotal != 4 || !got.AttachedWorkspaceIDs["ws-000000"] {
		t.Fatalf("rebuilt admission indexes = %#v", got)
	}
	workspaceAdmission, err := reopened.ReadWorkspaceAdmission(WorkspaceAdmissionQuery{
		ProjectID: "project-bench", IdempotencyKind: "create_workspace", IdempotencyKey: "reopen-workspace-0001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if workspaceAdmission.ActiveForProject != 4 {
		t.Fatalf("rebuilt workspace capacity = %#v", workspaceAdmission)
	}
	environmentAdmission, err := reopened.ReadEnvironmentAdmission(EnvironmentAdmissionQuery{
		ProjectID: "project-bench", IdempotencyKind: "create_environment", IdempotencyKey: "reopen-environment-0001", RevisionID: "missing-environment",
	})
	if err != nil {
		t.Fatal(err)
	}
	if environmentAdmission.CountForProject != 4 {
		t.Fatalf("rebuilt environment capacity = %#v", environmentAdmission)
	}
}

func TestSQLiteStoreMigratesLegacyWorkspaceAttachmentIndexToUniqueWorkspaceOwnership(t *testing.T) {
	path := privateTestPath(t, "state.db")
	s, err := OpenSQLite(path, "")
	if err != nil {
		t.Fatal(err)
	}
	state := makeBenchmarkState(2)
	if err := s.Update(func(next *State) error { *next = state; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE active_workspace_mounts;
		CREATE TABLE active_workspace_mounts (
			project_id TEXT NOT NULL,
			workspace_id TEXT NOT NULL,
			sandbox_key BLOB NOT NULL,
			PRIMARY KEY (project_id, workspace_id, sandbox_key)
		) WITHOUT ROWID;
		INSERT INTO active_workspace_mounts(project_id, workspace_id, sandbox_key) VALUES
			('project-bench', 'stale-duplicate', x'01'),
			('project-bench', 'stale-duplicate', x'02');`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLite(path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.ReadSandboxAdmission(SandboxAdmissionQuery{
		ProjectID: "project-bench", IdempotencyKind: "create_sandbox", IdempotencyKey: "migration-admission-0001", EnvironmentRevision: "env-000000", WorkspaceIDs: []string{"ws-000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ActiveTotal != 2 || !got.AttachedWorkspaceIDs["ws-000000"] {
		t.Fatalf("rebuilt unique workspace ownership index = %#v", got)
	}
	var stale int
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM active_workspace_mounts WHERE workspace_id = 'stale-duplicate'`).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Fatalf("stale legacy attachment rows survived rebuild: %d", stale)
	}
}

func TestSQLiteStoreRejectsTwoActiveOwnersForOneWorkspace(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := makeBenchmarkState(2)
	secondKey := ScopedKey("project-bench", "sbx-000001")
	second := state.Sandboxes[secondKey]
	second.WorkspaceMounts = []domain.WorkspaceMount{{WorkspaceID: "ws-000000", Path: "/other"}}
	state.Sandboxes[secondKey] = second
	if err := s.Update(func(next *State) error { *next = state; return nil }); err == nil {
		t.Fatal("two active sandboxes acquired the same workspace")
	}
	got, err := s.ReadSandboxAdmission(SandboxAdmissionQuery{
		ProjectID: "project-bench", IdempotencyKind: "create_sandbox", IdempotencyKey: "unique-rollback-0001", EnvironmentRevision: "env-000000", WorkspaceIDs: []string{"ws-000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ActiveTotal != 0 || got.AttachedWorkspaceIDs["ws-000000"] {
		t.Fatalf("failed duplicate ownership transaction did not roll back: %#v", got)
	}
}

func TestSQLiteStoreAtomicallyHandsWorkspaceBetweenActiveOwners(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state := makeBenchmarkState(2)
	firstKey := ScopedKey("project-bench", "sbx-000000")
	secondKey := ScopedKey("project-bench", "sbx-000001")
	second := state.Sandboxes[secondKey]
	second.State = domain.SandboxFailed
	second.WorkspaceMounts = nil
	state.Sandboxes[secondKey] = second
	if err := s.Update(func(next *State) error { *next = state; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(next *State) error {
		first := next.Sandboxes[firstKey]
		first.State = domain.SandboxFailed
		first.Revision++
		next.Sandboxes[firstKey] = first
		second := next.Sandboxes[secondKey]
		second.State = domain.SandboxRunning
		second.WorkspaceMounts = []domain.WorkspaceMount{{WorkspaceID: "ws-000000", Path: "/workspace"}}
		second.Revision++
		next.Sandboxes[secondKey] = second
		return nil
	}); err != nil {
		t.Fatalf("atomic workspace ownership handoff: %v", err)
	}
	got, err := s.ReadSandboxAdmission(SandboxAdmissionQuery{
		ProjectID: "project-bench", IdempotencyKind: "create_sandbox", IdempotencyKey: "handoff-read-0001", EnvironmentRevision: "env-000000", WorkspaceIDs: []string{"ws-000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ActiveTotal != 1 || got.ActiveForProject != 1 || !got.AttachedWorkspaceIDs["ws-000000"] {
		t.Fatalf("workspace ownership after handoff = %#v", got)
	}
	var owner []byte
	if err := s.db.QueryRow(`SELECT sandbox_key FROM active_workspace_mounts WHERE project_id = ? AND workspace_id = ?`, "project-bench", "ws-000000").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if string(owner) != secondKey {
		t.Fatalf("workspace owner = %q want %q", owner, secondKey)
	}
}

func BenchmarkSQLiteStoreSandboxAdmissionWithUnrelatedHistory(b *testing.B) {
	for _, resources := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("resources=%d", resources), func(b *testing.B) {
			s := benchmarkSQLiteStore(b, resources)
			defer s.Close()
			query := SandboxAdmissionQuery{
				ProjectID: "project-bench", IdempotencyKind: "create_sandbox", IdempotencyKey: "benchmark-admission-0001",
				EnvironmentRevision: fmt.Sprintf("env-%06d", resources/2),
				ConnectorRevisions:  []string{fmt.Sprintf("con-%06d", resources/2)},
				WorkspaceIDs:        []string{fmt.Sprintf("ws-%06d", resources/2)},
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := s.ReadSandboxAdmission(query); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSQLiteStoreIdempotencyLookupWithUnrelatedHistory(b *testing.B) {
	for _, resources := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("resources=%d", resources), func(b *testing.B) {
			s := benchmarkSQLiteStore(b, resources)
			defer s.Close()
			state := makeBenchmarkState(1)
			now := state.Sandboxes[ScopedKey("project-bench", "sbx-000000")].CreatedAt
			op := domain.Operation{
				ID: "op-lookup", ProjectID: "project-bench", Kind: "create_workspace", ResourceID: "ws-000000",
				State: domain.OperationSucceeded, IdempotencyKey: "benchmark-lookup-0001", CreatedAt: now, UpdatedAt: now,
			}
			indexKey := IdempotencyKey(op.ProjectID, op.Kind, op.IdempotencyKey)
			if err := s.UpdateRows(MutationScope{
				Operations: []string{ScopedKey(op.ProjectID, op.ID)}, Idempotency: []string{indexKey},
			}, func(scoped *State) error {
				scoped.Operations[ScopedKey(op.ProjectID, op.ID)] = op
				scoped.Idempotency[indexKey] = op.ID
				scoped.IdempotencyDigests[indexKey] = strings.Repeat("b", 64)
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				lookup, err := s.LookupIdempotency(op.ProjectID, op.Kind, op.IdempotencyKey)
				if err != nil || !lookup.Found {
					b.Fatalf("lookup = %#v, %v", lookup, err)
				}
			}
		})
	}
}

func BenchmarkSQLiteStoreWorkspaceAdmissionWithUnrelatedHistory(b *testing.B) {
	for _, resources := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("resources=%d", resources), func(b *testing.B) {
			s := benchmarkSQLiteStore(b, resources)
			defer s.Close()
			query := WorkspaceAdmissionQuery{
				ProjectID: "project-bench", IdempotencyKind: "create_workspace", IdempotencyKey: "benchmark-workspace-admission-0001",
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := s.ReadWorkspaceAdmission(query); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSQLiteStoreWorkspaceExactReadsWithUnrelatedHistory(b *testing.B) {
	for _, resources := range []int{100, 1000, 10000} {
		b.Run(fmt.Sprintf("resources=%d", resources), func(b *testing.B) {
			s := benchmarkSQLiteStore(b, resources)
			defer s.Close()
			workspaceID := fmt.Sprintf("ws-%06d", resources/2)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := s.GetWorkspace("project-bench", workspaceID); err != nil {
					b.Fatal(err)
				}
				if _, err := s.WorkspaceAttached("project-bench", workspaceID); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
