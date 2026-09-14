package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/brezel/internal/domain"
)

func TestFileStorePersistsAtomicallyAndScopesResources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey("a", "same")] = domain.Sandbox{ID: "same", ProjectID: "a"}
		state.Sandboxes[ScopedKey("b", "same")] = domain.Sandbox{ID: "same", ProjectID: "b"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.View(func(state State) error {
		if len(state.Sandboxes) != 2 {
			t.Fatalf("got %d sandboxes", len(state.Sandboxes))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
}

func TestFailedUpdateDoesNotMutateMemoryOrDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := errorsForTest("stop")
	err = s.Update(func(state *State) error {
		state.Idempotency["bad"] = "write"
		return want
	})
	if err != want {
		t.Fatalf("got %v", err)
	}
	_ = s.View(func(state State) error {
		if _, ok := state.Idempotency["bad"]; ok {
			t.Fatal("failed update mutated state")
		}
		return nil
	})
}

func TestFailedUpdateDoesNotAliasNestedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey("project-a", "sbx-one")] = domain.Sandbox{
			ID: "sbx-one", ProjectID: "project-a",
			Network: domain.NetworkPolicy{AllowOut: []string{"models.example.com"}},
			Failure: &domain.Failure{Code: "original"},
		}
		state.Events = append(state.Events, domain.Event{Details: map[string]any{"status": map[string]any{"value": "original"}}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := errorsForTest("stop")
	if err := s.Update(func(state *State) error {
		sandbox := state.Sandboxes[ScopedKey("project-a", "sbx-one")]
		sandbox.Network.AllowOut[0] = "changed.example.com"
		sandbox.Failure.Code = "changed"
		state.Events[0].Details["status"].(map[string]any)["value"] = "changed"
		return want
	}); err != want {
		t.Fatalf("got %v", err)
	}
	assertOriginal := func(state State) error {
		sandbox := state.Sandboxes[ScopedKey("project-a", "sbx-one")]
		if sandbox.Network.AllowOut[0] != "models.example.com" || sandbox.Failure.Code != "original" {
			t.Fatal("failed update mutated nested sandbox state")
		}
		if state.Events[0].Details["status"].(map[string]any)["value"] != "original" {
			t.Fatal("failed update mutated nested event state")
		}
		return nil
	}
	if err := s.View(assertOriginal); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.View(assertOriginal); err != nil {
		t.Fatal(err)
	}
}

func TestFileStoreRejectsConcurrentOpenAndUnsafePermissions(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.json")
	first, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(path); err == nil {
		t.Fatal("second controller opened the same state file")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(path); err == nil {
		t.Fatal("group-readable state file was accepted")
	}
}

func TestFileStoreRejectsSymlinkState(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "state.json")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(path); err == nil {
		t.Fatal("symlink state file was accepted")
	}
}

func TestFileStoreReadinessFailsAfterClose(t *testing.T) {
	s, err := OpenFile(filepath.Join(t.TempDir(), "state.json"))
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

func TestFileStoreGetSandboxIsKeyedAndDoesNotAliasState(t *testing.T) {
	s, err := OpenFile(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey("project-a", "sbx-one")] = domain.Sandbox{
			ID:        "sbx-one",
			ProjectID: "project-a",
			Network:   domain.NetworkPolicy{AllowOut: []string{"models.example.com"}, DenyOut: []string{"metadata.example.com"}},
			Failure:   &domain.Failure{Code: "original"},
			Revision:  1,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			UpdatedAt: time.Unix(1_700_000_000, 0).UTC(),
			ExpiresAt: time.Unix(1_700_003_600, 0).UTC(),
			Lifecycle: domain.Lifecycle{ExpiresAfterSeconds: 3600},
			State:     domain.SandboxRunning,
			Backend:   "e2b",
			BackendID: "backend-one",
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetSandbox("project-a", "sbx-one")
	if err != nil {
		t.Fatal(err)
	}
	got.Network.AllowOut[0] = "changed.example.com"
	got.Network.DenyOut[0] = "changed.example.com"
	got.Failure.Code = "changed"

	again, err := s.GetSandbox("project-a", "sbx-one")
	if err != nil {
		t.Fatal(err)
	}
	if again.Network.AllowOut[0] != "models.example.com" || again.Network.DenyOut[0] != "metadata.example.com" || again.Failure.Code != "original" {
		t.Fatalf("keyed read aliased store state: %#v", again)
	}
	if _, err := s.GetSandbox("project-b", "sbx-one"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project lookup error=%v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSandbox("project-a", "sbx-one"); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed store lookup error=%v", err)
	}
}

func TestFileStoreAppendSandboxEventIsOrderedDurableAndDoesNotAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey("project-a", "sbx-one")] = domain.Sandbox{
			ID: "sbx-one", ProjectID: "project-a", State: domain.SandboxRunning,
		}
		state.Sandboxes[ScopedKey("project-a", "sbx-two")] = domain.Sandbox{
			ID: "sbx-two", ProjectID: "project-a", State: domain.SandboxStandby,
		}
		state.Events = append(state.Events, domain.Event{
			ID: "evt-existing", Sequence: 4, ProjectID: "project-a", ResourceID: "sbx-one",
			Type: "sandbox.running", State: domain.SandboxRunning, At: now,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	details := map[string]any{"result": map[string]any{"status": "exited"}}
	if err := s.AppendSandboxEvent(domain.Event{
		ID: "evt-command", ProjectID: "project-a", ResourceID: "sbx-one",
		Type: "command.finished", State: domain.SandboxStandby, At: now.Add(time.Second), Details: details,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendSandboxEvent(domain.Event{
		ID: "evt-other", ProjectID: "project-a", ResourceID: "sbx-two",
		Type: "file.read", At: now.Add(2 * time.Second), Details: map[string]any{"bytes": 12},
	}); err != nil {
		t.Fatal(err)
	}
	details["result"].(map[string]any)["status"] = "changed"

	assertEvents := func(state State) error {
		if len(state.Events) != 3 {
			t.Fatalf("events=%d, want 3", len(state.Events))
		}
		command := state.Events[1]
		if command.Sequence != 5 || command.State != domain.SandboxRunning {
			t.Fatalf("command event sequence/state=%d/%s", command.Sequence, command.State)
		}
		if command.Details["result"].(map[string]any)["status"] != "exited" {
			t.Fatal("appended event details alias caller state")
		}
		other := state.Events[2]
		if other.Sequence != 1 || other.State != domain.SandboxStandby {
			t.Fatalf("other event sequence/state=%d/%s", other.Sequence, other.State)
		}
		return nil
	}
	if err := s.View(assertEvents); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.View(assertEvents); err != nil {
		t.Fatal(err)
	}
}

func TestFileStoreAppendSandboxEventFailsClosed(t *testing.T) {
	s, err := OpenFile(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey("project-a", "sbx-one")] = domain.Sandbox{
			ID: "sbx-one", ProjectID: "project-a", State: domain.SandboxRunning,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	if err := s.AppendSandboxEvent(domain.Event{
		ID: "evt-missing", ProjectID: "project-b", ResourceID: "sbx-one", Type: "file.read", At: now,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project append error=%v", err)
	}
	if err := s.AppendSandboxEvent(domain.Event{
		ID: "evt-invalid", ProjectID: "project-a", ResourceID: "sbx-one", Type: "file.read", At: now,
		Details: map[string]any{"invalid": make(chan struct{})},
	}); err == nil || !strings.Contains(err.Error(), "event details") {
		t.Fatalf("invalid event details error=%v", err)
	}
	if err := s.AppendSandboxEvent(domain.Event{
		ProjectID: "project-a", ResourceID: "sbx-one", Type: "file.read", At: now,
	}); err == nil || !strings.Contains(err.Error(), "event has invalid identity") {
		t.Fatalf("invalid event identity error=%v", err)
	}
	if err := s.View(func(state State) error {
		if len(state.Events) != 0 {
			t.Fatal("failed append mutated event state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFileStoreAppendSandboxEventPublishFollowsPersistence(t *testing.T) {
	directory := t.TempDir()
	originalPath := filepath.Join(directory, "state.json")
	s, err := OpenFile(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey("project-a", "sbx-one")] = domain.Sandbox{
			ID: "sbx-one", ProjectID: "project-a", State: domain.SandboxRunning,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.path = filepath.Join(directory, "missing", "state.json")
	if err := s.AppendSandboxEvent(domain.Event{
		ID: "evt-one", ProjectID: "project-a", ResourceID: "sbx-one",
		Type: "file.read", At: time.Unix(1_700_000_000, 0).UTC(),
	}); err == nil || !strings.Contains(err.Error(), "create temporary state") {
		t.Fatalf("persistence failure error=%v", err)
	}
	if err := s.View(func(state State) error {
		if len(state.Events) != 0 {
			t.Fatal("event became visible before durable persistence")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.path = originalPath
}

func TestFileStoreMigratesLegacyStateAndRejectsFutureSchema(t *testing.T) {
	directory := t.TempDir()
	legacyPath := filepath.Join(directory, "legacy.json")
	if err := os.WriteFile(legacyPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := OpenFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.View(func(state State) error {
		if state.SchemaVersion != CurrentSchemaVersion {
			t.Fatalf("schema version=%d", state.SchemaVersion)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	var migrated State
	if err := json.Unmarshal(persisted, &migrated); err != nil {
		t.Fatalf("decode migrated state: %v", err)
	}
	if migrated.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("legacy state was not migrated: %s", persisted)
	}

	future := NewState()
	future.SchemaVersion = CurrentSchemaVersion + 1
	encoded, err := json.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}
	futurePath := filepath.Join(directory, "future.json")
	if err := os.WriteFile(futurePath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(futurePath); err == nil || !strings.Contains(err.Error(), "unsupported schema version") {
		t.Fatalf("future schema error=%v", err)
	}
}

func TestFileStoreRejectsCrossProjectIdentityCorruptionBeforeCommit(t *testing.T) {
	s, err := OpenFile(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey("project-a", "sbx-one")] = domain.Sandbox{ID: "sbx-one", ProjectID: "project-b"}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "sandbox key does not match") {
		t.Fatalf("identity validation error=%v", err)
	}
	if err := s.View(func(state State) error {
		if len(state.Sandboxes) != 0 {
			t.Fatal("invalid update mutated in-memory state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCloneStateDoesNotAliasMutableFields(t *testing.T) {
	original := NewState()
	original.Sandboxes[ScopedKey("project-a", "sbx-one")] = domain.Sandbox{
		ID:        "sbx-one",
		ProjectID: "project-a",
		Network: domain.NetworkPolicy{
			AllowOut: []string{"models.example.com"},
			DenyOut:  []string{"metadata.example.com"},
		},
		ConnectorRevisions: []string{"connector-one"},
		WorkspaceMounts:    []domain.WorkspaceMount{{WorkspaceID: "workspace-one", Path: "/workspace"}},
		Failure:            &domain.Failure{Code: "sandbox-failure"},
	}
	original.Operations[ScopedKey("project-a", "op-one")] = domain.Operation{
		ID: "op-one", ProjectID: "project-a", Failure: &domain.Failure{Code: "operation-failure"},
	}
	original.Workspaces[ScopedKey("project-a", "workspace-one")] = domain.Workspace{
		ID: "workspace-one", ProjectID: "project-a", Failure: &domain.Failure{Code: "workspace-failure"},
	}
	original.Connectors[ScopedKey("project-a", "connector-one")] = domain.Connector{
		RevisionID: "connector-one", ProjectID: "project-a", AllowedMethods: []string{"POST"}, AllowedPaths: []string{"/v1/*"},
	}
	original.Events = []domain.Event{{Details: map[string]any{"nested": map[string]any{"value": "original"}}}}

	clone, err := cloneState(original)
	if err != nil {
		t.Fatal(err)
	}
	sandbox := clone.Sandboxes[ScopedKey("project-a", "sbx-one")]
	sandbox.Network.AllowOut[0] = "changed.example.com"
	sandbox.Network.DenyOut[0] = "changed.example.com"
	sandbox.ConnectorRevisions[0] = "changed"
	sandbox.WorkspaceMounts[0].Path = "/changed"
	sandbox.Failure.Code = "changed"
	clone.Sandboxes[ScopedKey("project-a", "sbx-one")] = sandbox
	operation := clone.Operations[ScopedKey("project-a", "op-one")]
	operation.Failure.Code = "changed"
	workspace := clone.Workspaces[ScopedKey("project-a", "workspace-one")]
	workspace.Failure.Code = "changed"
	connector := clone.Connectors[ScopedKey("project-a", "connector-one")]
	connector.AllowedMethods[0] = "GET"
	connector.AllowedPaths[0] = "/changed"
	clone.Events[0].Details["nested"].(map[string]any)["value"] = "changed"

	originalSandbox := original.Sandboxes[ScopedKey("project-a", "sbx-one")]
	if originalSandbox.Network.AllowOut[0] != "models.example.com" ||
		originalSandbox.Network.DenyOut[0] != "metadata.example.com" ||
		originalSandbox.ConnectorRevisions[0] != "connector-one" ||
		originalSandbox.WorkspaceMounts[0].Path != "/workspace" ||
		originalSandbox.Failure.Code != "sandbox-failure" {
		t.Fatal("sandbox clone aliases original mutable state")
	}
	if original.Operations[ScopedKey("project-a", "op-one")].Failure.Code != "operation-failure" {
		t.Fatal("operation clone aliases original failure")
	}
	if original.Workspaces[ScopedKey("project-a", "workspace-one")].Failure.Code != "workspace-failure" {
		t.Fatal("workspace clone aliases original failure")
	}
	if original.Connectors[ScopedKey("project-a", "connector-one")].AllowedMethods[0] != "POST" ||
		original.Connectors[ScopedKey("project-a", "connector-one")].AllowedPaths[0] != "/v1/*" {
		t.Fatal("connector clone aliases original slices")
	}
	if original.Events[0].Details["nested"].(map[string]any)["value"] != "original" {
		t.Fatal("event clone aliases original details")
	}
}

func TestCloneStateRejectsNonJSONEventDetails(t *testing.T) {
	state := NewState()
	state.Events = []domain.Event{{Details: map[string]any{"invalid": make(chan struct{})}}}
	if _, err := cloneState(state); err == nil {
		t.Fatal("non-JSON event detail was accepted")
	}
}

func TestCloneStateMatchesPersistedJSONShape(t *testing.T) {
	original := makeBenchmarkState(3)
	want, err := cloneStateWithJSON(original)
	if err != nil {
		t.Fatal(err)
	}
	got, err := cloneState(original)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("typed clone differs from the persisted JSON state shape")
	}
}

type errorsForTest string

func (e errorsForTest) Error() string { return string(e) }
