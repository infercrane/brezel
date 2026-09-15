package service

import (
	"testing"
	"time"

	"github.com/infercrane/brezel/internal/domain"
	"github.com/infercrane/brezel/internal/store"
)

type fallbackAdmissionStore struct {
	state     store.State
	viewCalls int
}

func (s *fallbackAdmissionStore) View(fn func(store.State) error) error {
	s.viewCalls++
	return fn(s.state)
}

func (s *fallbackAdmissionStore) Update(fn func(*store.State) error) error {
	return fn(&s.state)
}

func TestAdmissionFallbackPreservesCustomStoreWorkspaceBehavior(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	state := store.NewState()
	active := domain.Workspace{ID: "workspace-active", ProjectID: "project-a", State: domain.WorkspaceReady, CreatedAt: now, UpdatedAt: now}
	terminal := domain.Workspace{ID: "workspace-terminal", ProjectID: "project-a", State: domain.WorkspaceDeleted, CreatedAt: now, UpdatedAt: now}
	state.Workspaces[store.ScopedKey(active.ProjectID, active.ID)] = active
	state.Workspaces[store.ScopedKey(terminal.ProjectID, terminal.ID)] = terminal
	state.Sandboxes[store.ScopedKey("project-a", "sandbox-a")] = domain.Sandbox{
		ID: "sandbox-a", ProjectID: "project-a", State: domain.SandboxRunning,
		WorkspaceMounts: []domain.WorkspaceMount{{WorkspaceID: active.ID, Path: "/workspace"}},
	}
	target := &fallbackAdmissionStore{state: state}
	svc := &Service{store: target}

	op, count, err := svc.admitWorkspaceCreate("project-a", "workspace-request-0001", "digest")
	if err != nil || op.ID != "" || count != 1 {
		t.Fatalf("workspace fallback admission = %#v, %d, %v", op, count, err)
	}
	got, err := svc.getWorkspace("project-a", active.ID)
	if err != nil || got.ID != active.ID {
		t.Fatalf("workspace fallback get = %#v, %v", got, err)
	}
	attached, err := svc.workspaceAttachedLocked("project-a", active.ID)
	if err != nil || !attached {
		t.Fatalf("workspace fallback attachment = %t, %v", attached, err)
	}
	if target.viewCalls != 3 {
		t.Fatalf("fallback view calls = %d, want 3", target.viewCalls)
	}
}

func TestAdmissionFallbackPreservesCustomStoreEnvironmentReplayAndReuse(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	state := store.NewState()
	environment := domain.Environment{RevisionID: "environment-a", ProjectID: "project-a", Name: "python", Backend: "test", BackendTemplate: "python", CreatedAt: now}
	state.Environments[store.ScopedKey(environment.ProjectID, environment.RevisionID)] = environment
	target := &fallbackAdmissionStore{state: state}
	svc := &Service{store: target}

	reuse, err := svc.admitEnvironmentCreate("project-a", "environment-reuse-0001", "digest-a", environment.RevisionID)
	if err != nil || !reuse.EnvironmentFound || reuse.Environment.RevisionID != environment.RevisionID {
		t.Fatalf("environment fallback reuse = %#v, %v", reuse, err)
	}
	missing, err := svc.admitEnvironmentCreate("project-a", "environment-missing-0001", "digest-b", "environment-missing")
	if err != nil || missing.EnvironmentFound || missing.CountForProject != 1 {
		t.Fatalf("environment fallback count = %#v, %v", missing, err)
	}

	op := domain.Operation{
		ID: "operation-a", ProjectID: "project-a", Kind: "create_environment", ResourceID: environment.RevisionID,
		State: domain.OperationSucceeded, IdempotencyKey: "environment-replay-0001", CreatedAt: now, UpdatedAt: now,
	}
	indexKey := store.IdempotencyKey(op.ProjectID, op.Kind, op.IdempotencyKey)
	target.state.Operations[store.ScopedKey(op.ProjectID, op.ID)] = op
	target.state.Idempotency[indexKey] = op.ID
	target.state.IdempotencyDigests[indexKey] = "digest-replay"
	replay, err := svc.admitEnvironmentCreate("project-a", op.IdempotencyKey, "digest-replay", "different-revision")
	if err != nil || !replay.Idempotency.Found || replay.Idempotency.Operation.ID != op.ID || replay.Environment.RevisionID != environment.RevisionID {
		t.Fatalf("environment fallback replay = %#v, %v", replay, err)
	}
}
