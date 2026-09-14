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
	path := filepath.Join(t.TempDir(), "state.db")
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
	directory := t.TempDir()
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
	directory := t.TempDir()
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
	path := filepath.Join(t.TempDir(), "state.db")
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
	path := filepath.Join(t.TempDir(), "state.db")
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
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "state.db"), "")
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
