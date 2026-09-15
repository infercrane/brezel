package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
