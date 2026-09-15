package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/brezel/internal/domain"
)

func TestSQLiteStoreUpdateReopenAndRollback(t *testing.T) {
	path := privateTestPath(t, "state.db")
	s, err := OpenSQLite(path, "")
	if err != nil {
		t.Fatal(err)
	}
	sandbox := sqliteTestSandbox()
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("rollback")
	if err := s.Update(func(state *State) error {
		current := state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)]
		current.BackendID = "must-not-persist"
		state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)] = current
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("rollback error=%v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLite(path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.GetSandbox(sandbox.ProjectID, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, sandbox) {
		t.Fatalf("reopened sandbox mismatch\ngot:  %#v\nwant: %#v", got, sandbox)
	}
}

func TestSQLiteStoreMigratesLegacyFileWithoutModifyingIt(t *testing.T) {
	directory := privateTestDirectory(t)
	legacyPath := filepath.Join(directory, "state.json")
	legacy, err := OpenFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	sandbox := sqliteTestSandbox()
	if err := legacy.Update(func(state *State) error {
		state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}

	databasePath := filepath.Join(directory, "state.db")
	s, err := OpenSQLite(databasePath, legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSandbox(sandbox.ProjectID, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, sandbox) {
		t.Fatalf("migrated sandbox mismatch: %#v", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("legacy source changed during migration")
	}

	// Once initialized, the database remains authoritative even if the legacy
	// source later changes.
	var legacyState State
	if err := json.Unmarshal(after, &legacyState); err != nil {
		t.Fatal(err)
	}
	delete(legacyState.Sandboxes, ScopedKey(sandbox.ProjectID, sandbox.ID))
	changed, err := json.Marshal(legacyState)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(databasePath, legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.GetSandbox(sandbox.ProjectID, sandbox.ID); err != nil {
		t.Fatalf("initialized database was reimported: %v", err)
	}
}

func TestSQLiteStoreRejectsConcurrentOpenSymlinkAndUnsafePermissions(t *testing.T) {
	directory := privateTestDirectory(t)
	path := filepath.Join(directory, "state.db")
	first, err := OpenSQLite(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSQLite(path, ""); err == nil {
		t.Fatal("second controller opened the same database")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSQLite(path, ""); err == nil {
		t.Fatal("group-readable database was accepted")
	}

	target := filepath.Join(directory, "target.db")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(directory, "symlink.db")
	if err := os.Symlink(target, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSQLite(symlink, ""); err == nil {
		t.Fatal("symlink database was accepted")
	}
}

func TestSQLiteStoreActivityAndEventsAreKeyedDurableOperations(t *testing.T) {
	path := privateTestPath(t, "state.db")
	s, err := OpenSQLite(path, "")
	if err != nil {
		t.Fatal(err)
	}
	sandbox := sqliteTestSandbox()
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	earlier := sandbox.LastActiveAt.Add(-time.Minute)
	got, err := s.RecordSandboxActivity(sandbox.ProjectID, sandbox.ID, earlier)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastActiveAt.Equal(sandbox.LastActiveAt) || got.Revision != sandbox.Revision+1 {
		t.Fatalf("activity was not monotonic: %#v", got)
	}
	wantEligible := sandbox.LastActiveAt.Add(45 * time.Second)
	if !got.StandbyEligibleAt.Equal(wantEligible) {
		t.Fatalf("standby eligible=%v want=%v", got.StandbyEligibleAt, wantEligible)
	}

	const events = 16
	var wg sync.WaitGroup
	errorsSeen := make(chan error, events)
	for index := 0; index < events; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			errorsSeen <- s.AppendSandboxEvent(domain.Event{
				ID: "evt-" + string(rune('a'+index)), ProjectID: sandbox.ProjectID,
				ResourceID: sandbox.ID, Type: "command.finished",
				At: time.Unix(1_700_000_100+int64(index), 0).UTC(),
			})
		}(index)
	}
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLite(path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.View(func(state State) error {
		if len(state.Events) != events {
			t.Fatalf("event count=%d", len(state.Events))
		}
		for index, event := range state.Events {
			if event.Sequence != int64(index+1) || event.State != sandbox.State {
				t.Fatalf("event[%d]=%#v", index, event)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteStoreRejectsSemanticallyCorruptPayloadOnOpen(t *testing.T) {
	path := privateTestPath(t, "state.db")
	s, err := OpenSQLite(path, "")
	if err != nil {
		t.Fatal(err)
	}
	sandbox := sqliteTestSandbox()
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE resources SET payload = ? WHERE kind = ?`, []byte(`{"id":`), resourceSandbox); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSQLite(path, ""); err == nil || !strings.Contains(err.Error(), "validate stored state") {
		t.Fatalf("semantic corruption error=%v", err)
	}
}

func TestSQLiteStoreReadyFailsAfterClose(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(); err == nil {
		t.Fatal("closed store reported ready")
	}
}

func TestSQLiteStoreUpdateRowsCommitsDeclaredRowsAndEvent(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sandbox := sqliteTestSandbox()
	unrelated := sandbox
	unrelated.ID = "sbx-unrelated"
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		state.Sandboxes[ScopedKey(unrelated.ProjectID, unrelated.ID)] = unrelated
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendSandboxEvent(domain.Event{ID: "evt-existing", ProjectID: sandbox.ProjectID, ResourceID: sandbox.ID, Type: "sandbox.running", At: sandbox.CreatedAt}); err != nil {
		t.Fatal(err)
	}

	op := domain.Operation{
		ID: "op-pause", ProjectID: sandbox.ProjectID, Kind: "pause_sandbox:" + sandbox.ID,
		ResourceID: sandbox.ID, State: domain.OperationRunning, IdempotencyKey: "request-one",
		CreatedAt: sandbox.CreatedAt, UpdatedAt: sandbox.UpdatedAt,
	}
	idempotency := IdempotencyKey(sandbox.ProjectID, op.Kind, op.IdempotencyKey)
	sandbox.State = domain.SandboxPausing
	sandbox.Revision++
	if err := s.UpdateRows(MutationScope{
		Sandboxes:    []string{ScopedKey(sandbox.ProjectID, sandbox.ID)},
		Operations:   []string{ScopedKey(op.ProjectID, op.ID)},
		Idempotency:  []string{idempotency},
		EventStreams: []EventStream{{ProjectID: sandbox.ProjectID, ResourceID: sandbox.ID}},
	}, func(state *State) error {
		state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		state.Operations[ScopedKey(op.ProjectID, op.ID)] = op
		state.Idempotency[idempotency] = op.ID
		state.Events = append(state.Events, domain.Event{
			ID: "evt-pausing", Sequence: nextSandboxEventSequence(state.Events, sandbox.ProjectID, sandbox.ID),
			ProjectID: sandbox.ProjectID, ResourceID: sandbox.ID, OperationID: op.ID,
			Type: "sandbox.pausing", State: sandbox.State, At: sandbox.UpdatedAt,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.View(func(state State) error {
		if got := state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)]; !reflect.DeepEqual(got, sandbox) {
			t.Fatalf("sandbox mismatch: %#v", got)
		}
		if got := state.Sandboxes[ScopedKey(unrelated.ProjectID, unrelated.ID)]; !reflect.DeepEqual(got, unrelated) {
			t.Fatalf("unrelated row changed: %#v", got)
		}
		if state.Idempotency[idempotency] != op.ID || !reflect.DeepEqual(state.Operations[ScopedKey(op.ProjectID, op.ID)], op) {
			t.Fatal("operation and idempotency claim were not committed atomically")
		}
		if len(state.Events) != 2 || state.Events[1].Sequence != 2 {
			t.Fatalf("events=%#v", state.Events)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteStoreUpdateRowsRollsBackAndRejectsUndeclaredChanges(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sandbox := sqliteTestSandbox()
	key := ScopedKey(sandbox.ProjectID, sandbox.ID)
	if err := s.Update(func(state *State) error {
		state.Sandboxes[key] = sandbox
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("stop")
	if err := s.UpdateRows(MutationScope{Sandboxes: []string{key}}, func(state *State) error {
		changed := state.Sandboxes[key]
		changed.Revision++
		state.Sandboxes[key] = changed
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("rollback error=%v", err)
	}
	undeclaredKey := ScopedKey(sandbox.ProjectID, "sbx-undeclared")
	if err := s.UpdateRows(MutationScope{Sandboxes: []string{key}}, func(state *State) error {
		changed := sandbox
		changed.ID = "sbx-undeclared"
		state.Sandboxes[undeclaredKey] = changed
		return nil
	}); err == nil || !strings.Contains(err.Error(), "undeclared resource") {
		t.Fatalf("undeclared mutation error=%v", err)
	}
	if err := s.View(func(state State) error {
		if state.Sandboxes[key].Revision != sandbox.Revision {
			t.Fatal("rolled-back change persisted")
		}
		if _, ok := state.Sandboxes[undeclaredKey]; ok {
			t.Fatal("undeclared row persisted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteStoreUpdateRowsLoadsIdempotencyReplayDependencies(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sandbox := sqliteTestSandbox()
	op := domain.Operation{
		ID: "op-existing", ProjectID: sandbox.ProjectID, Kind: "create_sandbox", ResourceID: sandbox.ID,
		State: domain.OperationSucceeded, IdempotencyKey: "same-request", CreatedAt: sandbox.CreatedAt, UpdatedAt: sandbox.UpdatedAt,
	}
	idempotency := IdempotencyKey(sandbox.ProjectID, op.Kind, op.IdempotencyKey)
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		state.Operations[ScopedKey(op.ProjectID, op.ID)] = op
		state.Idempotency[idempotency] = op.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var gotOperation domain.Operation
	var gotSandbox domain.Sandbox
	if err := s.UpdateRows(MutationScope{
		Sandboxes:   []string{ScopedKey(sandbox.ProjectID, "sbx-new")},
		Operations:  []string{ScopedKey(op.ProjectID, "op-new")},
		Idempotency: []string{idempotency},
	}, func(state *State) error {
		operationID := state.Idempotency[idempotency]
		gotOperation = state.Operations[ScopedKey(sandbox.ProjectID, operationID)]
		gotSandbox = state.Sandboxes[ScopedKey(sandbox.ProjectID, gotOperation.ResourceID)]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotOperation, op) || !reflect.DeepEqual(gotSandbox, sandbox) {
		t.Fatalf("replay dependencies missing: op=%#v sandbox=%#v", gotOperation, gotSandbox)
	}
}

func TestSQLiteStoreUpdateRowsSerializesConcurrentMutations(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sandbox := sqliteTestSandbox()
	key := ScopedKey(sandbox.ProjectID, sandbox.ID)
	if err := s.Update(func(state *State) error { state.Sandboxes[key] = sandbox; return nil }); err != nil {
		t.Fatal(err)
	}
	const workers = 32
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.UpdateRows(MutationScope{Sandboxes: []string{key}}, func(state *State) error {
				current := state.Sandboxes[key]
				current.Revision++
				state.Sandboxes[key] = current
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
	got, err := s.GetSandbox(sandbox.ProjectID, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != sandbox.Revision+workers {
		t.Fatalf("revision=%d want=%d", got.Revision, sandbox.Revision+workers)
	}
}

func TestSQLiteStoreUpdateRowsDoesNotMaterializeUnrelatedHistory(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const unrelated = 2000
	state := makeBenchmarkState(unrelated)
	if err := s.Update(func(next *State) error {
		*next = state
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	key := ScopedKey("project-bench", "sbx-000000")
	if err := s.UpdateRows(MutationScope{Sandboxes: []string{key}}, func(scoped *State) error {
		if len(scoped.Sandboxes) != 1 || len(scoped.Environments) != 0 || len(scoped.Operations) != 0 || len(scoped.Events) != 0 {
			t.Fatalf("scoped callback materialized unrelated rows: sandboxes=%d environments=%d operations=%d events=%d", len(scoped.Sandboxes), len(scoped.Environments), len(scoped.Operations), len(scoped.Events))
		}
		sandbox := scoped.Sandboxes[key]
		sandbox.Revision++
		scoped.Sandboxes[key] = sandbox
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var sandboxCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM resources WHERE kind = ?`, resourceSandbox).Scan(&sandboxCount); err != nil {
		t.Fatal(err)
	}
	if sandboxCount != unrelated {
		t.Fatalf("sandbox rows=%d want=%d", sandboxCount, unrelated)
	}
}

func TestSQLiteReconcileScanDoesNotMaterializeEventOrIdempotencyLedger(t *testing.T) {
	path := privateTestPath(t, "state.db")
	s, err := OpenSQLite(path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sandbox := sqliteTestSandbox()
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2_000; index++ {
		resourceID := fmt.Sprintf("unrelated-%04d", index)
		if _, err := tx.Exec(`INSERT INTO sandbox_events(project_id, resource_id, sequence, event_id, payload) VALUES (?, ?, ?, ?, ?)`, "project-a", resourceID, 1, "event-"+resourceID, []byte("{")); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO idempotency(idempotency_key, operation_id, digest) VALUES (?, ?, ?)`, []byte(fmt.Sprintf("project-a\x00operation\x00key-%04d", index)), "missing-operation", "digest"); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.View(func(State) error { return nil }); err == nil {
		t.Fatal("whole-state view unexpectedly accepted deliberately unreadable unrelated history")
	}
	sandboxes, err := s.ListSandboxesForReconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(sandboxes) != 1 || sandboxes[0].ID != sandbox.ID {
		t.Fatalf("reconcile scan = %#v", sandboxes)
	}
}

func TestSQLiteLifecycleOperationHeadsAreBoundedAndSettledHeadSuppressesHistory(t *testing.T) {
	path := privateTestPath(t, "state.db")
	s, err := OpenSQLite(path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Unix(1_700_000_000, 0).UTC()
	sandbox := sqliteTestSandbox()
	oldIncomplete := sqliteLifecycleOperation("op-create-old", sandbox.ID, "create_sandbox", domain.OperationRunning, now)
	settledHead := sqliteLifecycleOperation("op-create-settled", sandbox.ID, "create_sandbox", domain.OperationSucceeded, now.Add(time.Second))
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		state.Operations[ScopedKey(oldIncomplete.ProjectID, oldIncomplete.ID)] = oldIncomplete
		state.Operations[ScopedKey(settledHead.ProjectID, settledHead.ID)] = settledHead
		for index := 0; index < 500; index++ {
			guest := sqliteLifecycleOperation(fmt.Sprintf("op-guest-%04d", index), sandbox.ID, "run_command", domain.OperationSucceeded, now.Add(time.Duration(index)*time.Millisecond))
			state.Operations[ScopedKey(guest.ProjectID, guest.ID)] = guest
			historical := sqliteLifecycleOperation(fmt.Sprintf("op-create-history-%04d", index), sandbox.ID, "create_sandbox", domain.OperationRunning, now.Add(-time.Duration(index+1)*time.Second))
			state.Operations[ScopedKey(historical.ProjectID, historical.ID)] = historical
		}
		nearPrefix := sqliteLifecycleOperation("op-near-prefix", sandbox.ID, "create_sandbox_extra", domain.OperationRunning, now.Add(3*time.Second))
		state.Operations[ScopedKey(nearPrefix.ProjectID, nearPrefix.ID)] = nearPrefix
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	operations, err := s.ListLifecycleOperationsForReconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 0 {
		t.Fatalf("settled head did not suppress older incomplete operations: %#v", operations)
	}
	var heads int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM lifecycle_operation_heads`).Scan(&heads); err != nil {
		t.Fatal(err)
	}
	if heads != 1 {
		t.Fatalf("head rows=%d want=1 for 502 lifecycle/guest operations", heads)
	}

	failedResume := sqliteLifecycleOperation("op-resume-failed", sandbox.ID, "resume_sandbox:"+sandbox.ID, domain.OperationFailed, now.Add(2*time.Second))
	if err := s.UpdateRows(MutationScope{Operations: []string{ScopedKey(failedResume.ProjectID, failedResume.ID)}}, func(state *State) error {
		state.Operations[ScopedKey(failedResume.ProjectID, failedResume.ID)] = failedResume
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	operations, err = s.ListLifecycleOperationsForReconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].ID != failedResume.ID {
		t.Fatalf("recoverable heads=%#v", operations)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM lifecycle_operation_heads`).Scan(&heads); err != nil {
		t.Fatal(err)
	}
	if heads != 2 {
		t.Fatalf("head rows=%d want=2 families", heads)
	}
	rollbackErr := errors.New("rollback projection")
	deleteAttempt := sqliteLifecycleOperation("op-delete-rollback", sandbox.ID, "delete_sandbox:"+sandbox.ID, domain.OperationRunning, now.Add(4*time.Second))
	if err := s.UpdateRows(MutationScope{Operations: []string{ScopedKey(deleteAttempt.ProjectID, deleteAttempt.ID)}}, func(state *State) error {
		state.Operations[ScopedKey(deleteAttempt.ProjectID, deleteAttempt.ID)] = deleteAttempt
		return rollbackErr
	}); !errors.Is(err, rollbackErr) {
		t.Fatalf("rollback error=%v", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM lifecycle_operation_heads`).Scan(&heads); err != nil {
		t.Fatal(err)
	}
	if heads != 2 {
		t.Fatalf("rolled-back projection changed head rows to %d", heads)
	}
}

func TestSQLiteLifecycleOperationHeadsMigrateRebuildAndChooseDeterministically(t *testing.T) {
	path := privateTestPath(t, "state.db")
	s, err := OpenSQLite(path, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	sandbox := sqliteTestSandbox()
	olderCreated := sqliteLifecycleOperation("op-z", sandbox.ID, "create_sandbox", domain.OperationRunning, now)
	olderCreated.CreatedAt = now.Add(-time.Second)
	newerCreated := sqliteLifecycleOperation("op-a", sandbox.ID, "create_sandbox", domain.OperationFailed, now)
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		state.Operations[ScopedKey(olderCreated.ProjectID, olderCreated.ID)] = olderCreated
		state.Operations[ScopedKey(newerCreated.ProjectID, newerCreated.ID)] = newerCreated
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE lifecycle_operation_heads`); err != nil {
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
	operations, err := reopened.ListLifecycleOperationsForReconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].ID != newerCreated.ID {
		t.Fatalf("rebuilt deterministic head=%#v", operations)
	}
	var operationID string
	if err := reopened.db.QueryRow(`SELECT operation_id FROM lifecycle_operation_heads`).Scan(&operationID); err != nil {
		t.Fatal(err)
	}
	if operationID != newerCreated.ID {
		t.Fatalf("operation head=%q want=%q", operationID, newerCreated.ID)
	}
}

func TestSQLiteLifecycleOperationHeadPreservesInsertionOrderAcrossClockRegressionAndReopen(t *testing.T) {
	path := privateTestPath(t, "state.db")
	s, err := OpenSQLite(path, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	sandbox := sqliteTestSandbox()
	older := sqliteLifecycleOperation("op-pause-older", sandbox.ID, "pause_sandbox:"+sandbox.ID, domain.OperationFailed, now.Add(time.Hour))
	newer := sqliteLifecycleOperation("op-pause-newer", sandbox.ID, "pause_sandbox:"+sandbox.ID, domain.OperationRunning, now)
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		state.Operations[ScopedKey(older.ProjectID, older.ID)] = older
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRows(MutationScope{Operations: []string{ScopedKey(newer.ProjectID, newer.ID)}}, func(state *State) error {
		state.Operations[ScopedKey(newer.ProjectID, newer.ID)] = newer
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Updating an older non-head after the new attempt was admitted must not
	// reorder the family, even when its wall-clock timestamp moves farther ahead.
	older.UpdatedAt = now.Add(2 * time.Hour)
	older.Failure = &domain.Failure{Code: "older_attempt", Message: "historical failure", Retryable: true}
	if err := s.UpdateRows(MutationScope{Operations: []string{ScopedKey(older.ProjectID, older.ID)}}, func(state *State) error {
		state.Operations[ScopedKey(older.ProjectID, older.ID)] = older
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	operations, err := s.ListLifecycleOperationsForReconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].ID != newer.ID {
		t.Fatalf("clock-regressed insertion head=%#v", operations)
	}
	var projectionVersion string
	if err := s.db.QueryRow(`SELECT value FROM metadata WHERE key = ?`, lifecycleOperationHeadsProjectionMetadataKey).Scan(&projectionVersion); err != nil {
		t.Fatal(err)
	}
	if projectionVersion != strconv.Itoa(lifecycleOperationHeadsProjectionVersion) {
		t.Fatalf("projection version=%q", projectionVersion)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLite(path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	operations, err = reopened.ListLifecycleOperationsForReconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].ID != newer.ID {
		t.Fatalf("ordinary reopen rebuilt insertion-ordered head using wall clock: %#v", operations)
	}
	var operationID string
	if err := reopened.db.QueryRow(`SELECT operation_id FROM lifecycle_operation_heads`).Scan(&operationID); err != nil {
		t.Fatal(err)
	}
	if operationID != newer.ID {
		t.Fatalf("durable operation head=%q want=%q", operationID, newer.ID)
	}
}

func TestSQLiteLifecycleOperationHeadsRebuildWhenProjectionMarkerIsMissing(t *testing.T) {
	path := privateTestPath(t, "state.db")
	s, err := OpenSQLite(path, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	sandbox := sqliteTestSandbox()
	older := sqliteLifecycleOperation("op-pause-older", sandbox.ID, "pause_sandbox:"+sandbox.ID, domain.OperationRunning, now)
	newer := sqliteLifecycleOperation("op-pause-newer", sandbox.ID, "pause_sandbox:"+sandbox.ID, domain.OperationFailed, now.Add(time.Second))
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		state.Operations[ScopedKey(older.ProjectID, older.ID)] = older
		state.Operations[ScopedKey(newer.ProjectID, newer.ID)] = newer
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM metadata WHERE key = ?`, lifecycleOperationHeadsProjectionMetadataKey); err != nil {
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
	operations, err := reopened.ListLifecycleOperationsForReconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].ID != newer.ID {
		t.Fatalf("marker-loss rebuild head=%#v", operations)
	}
	var projectionVersion string
	if err := reopened.db.QueryRow(`SELECT value FROM metadata WHERE key = ?`, lifecycleOperationHeadsProjectionMetadataKey).Scan(&projectionVersion); err != nil {
		t.Fatal(err)
	}
	if projectionVersion != strconv.Itoa(lifecycleOperationHeadsProjectionVersion) {
		t.Fatalf("rebuilt projection version=%q", projectionVersion)
	}
}

func TestSQLiteLifecycleOperationHeadsTrackUpdatesDeletionAndTerminalOwners(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Unix(1_700_000_000, 0).UTC()
	sandbox := sqliteTestSandbox()
	workspace := domain.Workspace{ID: sandbox.ID, ProjectID: sandbox.ProjectID, Name: "workspace", BackendName: "workspace", State: domain.WorkspacePreparing, CreatedAt: now, UpdatedAt: now}
	oldCreate := sqliteLifecycleOperation("op-create-old", sandbox.ID, "create_sandbox", domain.OperationRunning, now)
	newCreate := sqliteLifecycleOperation("op-create-new", sandbox.ID, "create_sandbox", domain.OperationRunning, now.Add(time.Second))
	workspaceCreate := sqliteLifecycleOperation("op-workspace-create", workspace.ID, "create_workspace", domain.OperationRunning, now)
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		state.Workspaces[ScopedKey(workspace.ProjectID, workspace.ID)] = workspace
		for _, operation := range []domain.Operation{oldCreate, newCreate, workspaceCreate} {
			state.Operations[ScopedKey(operation.ProjectID, operation.ID)] = operation
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	newCreate.State = domain.OperationSucceeded
	if err := s.UpdateRows(MutationScope{Operations: []string{ScopedKey(newCreate.ProjectID, newCreate.ID)}}, func(state *State) error {
		state.Operations[ScopedKey(newCreate.ProjectID, newCreate.ID)] = newCreate
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	operations, err := s.ListLifecycleOperationsForReconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].ID != workspaceCreate.ID {
		t.Fatalf("state-only settled head update did not suppress old sandbox operation: %#v", operations)
	}

	if err := s.Update(func(state *State) error {
		delete(state.Operations, ScopedKey(newCreate.ProjectID, newCreate.ID))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	operations, err = s.ListLifecycleOperationsForReconcile()
	if err != nil {
		t.Fatal(err)
	}
	if !operationIDsEqual(operations, oldCreate.ID, workspaceCreate.ID) {
		t.Fatalf("deleting current head did not promote runner-up: %#v", operations)
	}

	if err := s.Update(func(state *State) error {
		current := state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)]
		current.State = domain.SandboxFailed
		state.Sandboxes[ScopedKey(current.ProjectID, current.ID)] = current
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	operations, err = s.ListLifecycleOperationsForReconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].ID != workspaceCreate.ID {
		t.Fatalf("terminal sandbox removed wrong-owner heads: %#v", operations)
	}

	if err := s.Update(func(state *State) error {
		current := state.Workspaces[ScopedKey(workspace.ProjectID, workspace.ID)]
		current.State = domain.WorkspaceReady
		state.Workspaces[ScopedKey(current.ProjectID, current.ID)] = current
		settled := state.Operations[ScopedKey(workspaceCreate.ProjectID, workspaceCreate.ID)]
		settled.State = domain.OperationSucceeded
		state.Operations[ScopedKey(settled.ProjectID, settled.ID)] = settled
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	operations, err = s.ListLifecycleOperationsForReconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 0 {
		t.Fatalf("ready workspace retained recovery head: %#v", operations)
	}

	deleteWorkspace := sqliteLifecycleOperation("op-workspace-delete", workspace.ID, "delete_workspace:"+workspace.ID, domain.OperationRunning, now.Add(2*time.Second))
	if err := s.Update(func(state *State) error {
		current := state.Workspaces[ScopedKey(workspace.ProjectID, workspace.ID)]
		current.State = domain.WorkspaceDeleting
		state.Workspaces[ScopedKey(current.ProjectID, current.ID)] = current
		state.Operations[ScopedKey(deleteWorkspace.ProjectID, deleteWorkspace.ID)] = deleteWorkspace
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	operations, err = s.ListLifecycleOperationsForReconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].ID != deleteWorkspace.ID {
		t.Fatalf("workspace re-entry did not rebuild delete head: %#v", operations)
	}
	if err := s.Update(func(state *State) error {
		current := state.Workspaces[ScopedKey(workspace.ProjectID, workspace.ID)]
		current.State = domain.WorkspaceDeleted
		state.Workspaces[ScopedKey(current.ProjectID, current.ID)] = current
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	operations, err = s.ListLifecycleOperationsForReconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 0 {
		t.Fatalf("terminal workspace retained recovery head: %#v", operations)
	}
	var heads int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM lifecycle_operation_heads`).Scan(&heads); err != nil {
		t.Fatal(err)
	}
	if heads != 0 {
		t.Fatalf("terminal owners retained %d projection rows", heads)
	}
}

func TestSQLiteLifecycleOperationScanDoesNotDecodeUnrelatedOperationHistory(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Unix(1_700_000_000, 0).UTC()
	sandbox := sqliteTestSandbox()
	operation := sqliteLifecycleOperation("op-resume", sandbox.ID, "resume_sandbox:"+sandbox.ID, domain.OperationRunning, now)
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		state.Operations[ScopedKey(operation.ProjectID, operation.ID)] = operation
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO resources(kind, resource_key, payload) VALUES (?, ?, ?)`, resourceOperation, []byte(ScopedKey("project-a", "op-corrupt-unrelated")), []byte("{")); err != nil {
		t.Fatal(err)
	}
	operations, err := s.ListLifecycleOperationsForReconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].ID != operation.ID {
		t.Fatalf("lifecycle heads=%#v", operations)
	}
	if err := s.View(func(State) error { return nil }); err == nil {
		t.Fatal("whole-state view unexpectedly accepted corrupt unrelated operation history")
	}
}

func TestSQLiteLifecycleHeadAdmissionDoesNotDecodeUnrelatedOperationHistory(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`INSERT INTO resources(kind, resource_key, payload) VALUES (?, ?, ?)`, resourceOperation, []byte(ScopedKey("project-a", "op-corrupt-unrelated")), []byte("{")); err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1_700_000_000, 0).UTC()
	sandbox := sqliteTestSandbox()
	operation := sqliteLifecycleOperation("op-create", sandbox.ID, "create_sandbox", domain.OperationRunning, now)
	if err := s.UpdateRows(MutationScope{
		Sandboxes:  []string{ScopedKey(sandbox.ProjectID, sandbox.ID)},
		Operations: []string{ScopedKey(operation.ProjectID, operation.ID)},
	}, func(state *State) error {
		state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		state.Operations[ScopedKey(operation.ProjectID, operation.ID)] = operation
		return nil
	}); err != nil {
		t.Fatalf("create admission decoded unrelated operation history: %v", err)
	}
	operations, err := s.ListLifecycleOperationsForReconcile()
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].ID != operation.ID {
		t.Fatalf("lifecycle heads=%#v", operations)
	}
}

func sqliteLifecycleOperation(id, resourceID, kind string, state domain.OperationState, updatedAt time.Time) domain.Operation {
	return domain.Operation{ID: id, ProjectID: "project-a", ResourceID: resourceID, Kind: kind, State: state, CreatedAt: updatedAt, UpdatedAt: updatedAt}
}

func operationIDsEqual(operations []domain.Operation, expected ...string) bool {
	if len(operations) != len(expected) {
		return false
	}
	seen := make(map[string]struct{}, len(operations))
	for _, operation := range operations {
		seen[operation.ID] = struct{}{}
	}
	for _, id := range expected {
		if _, exists := seen[id]; !exists {
			return false
		}
	}
	return true
}

func sqliteTestSandbox() domain.Sandbox {
	now := time.Unix(1_700_000_000, 0).UTC()
	return domain.Sandbox{
		ID: "sbx-one", ProjectID: "project-a", EnvironmentRevision: "env-one",
		Backend: "e2b", BackendID: "backend-one", State: domain.SandboxRunning,
		Lifecycle: domain.Lifecycle{StandbyAfterSeconds: 30, StandbyGraceSeconds: 15, ExpiresAfterSeconds: 3600, AutoResume: true},
		Network:   domain.NetworkPolicy{AllowOut: []string{"models.example.com"}},
		CreatedAt: now, UpdatedAt: now, LastActiveAt: now,
		StandbyEligibleAt: now.Add(45 * time.Second), ExpiresAt: now.Add(time.Hour), Revision: 7,
	}
}
