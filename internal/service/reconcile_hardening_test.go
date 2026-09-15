package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/brezel/internal/backend"
	"github.com/infercrane/brezel/internal/domain"
	"github.com/infercrane/brezel/internal/nodeledger"
	"github.com/infercrane/brezel/internal/store"
)

type reconcileTestStore struct {
	mu             sync.Mutex
	state          store.State
	failNextUpdate error
	operationErr   error
	viewErr        error
}

func newReconcileTestStore() *reconcileTestStore {
	return &reconcileTestStore{state: store.NewState()}
}

func (s *reconcileTestStore) View(fn func(store.State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.viewErr != nil {
		return s.viewErr
	}
	return fn(s.state)
}

func (s *reconcileTestStore) Update(fn func(*store.State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNextUpdate != nil {
		err := s.failNextUpdate
		s.failNextUpdate = nil
		return err
	}
	return fn(&s.state)
}

func (s *reconcileTestStore) ListLifecycleOperationsForReconcile() ([]domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.operationErr != nil {
		return nil, s.operationErr
	}
	operations := make([]domain.Operation, 0, len(s.state.Operations))
	for _, operation := range s.state.Operations {
		operations = append(operations, operation)
	}
	return operations, nil
}

type reconcileTestBackend struct {
	mu                   sync.Mutex
	sandboxes            map[string]backend.Sandbox
	localSandboxes       map[string]string
	inspectErrors        map[string]error
	deleteErr            error
	deleteCalls          int
	inspectCalls         int
	inspectStarted       chan struct{}
	releaseInspect       chan struct{}
	inspectOnce          sync.Once
	pauseCalls           int
	workspaces           map[string]backend.Workspace
	workspaceFindStarted chan struct{}
	releaseWorkspaceFind chan struct{}
	workspaceFindOnce    sync.Once
	workspaceFindErr     error
}

func newReconcileTestBackend() *reconcileTestBackend {
	return &reconcileTestBackend{
		sandboxes:      make(map[string]backend.Sandbox),
		localSandboxes: make(map[string]string),
		inspectErrors:  make(map[string]error),
		workspaces:     make(map[string]backend.Workspace),
	}
}

func (b *reconcileTestBackend) Name() string { return backend.DefaultName }
func (b *reconcileTestBackend) Capabilities() backend.Capabilities {
	return backend.Capabilities{
		HostileCodeIsolation: true, DenyByDefaultEgress: true, DurableWorkspaces: true,
		CommandStreaming: true, FileReadWrite: true, AuthenticatedPorts: true,
	}
}
func (b *reconcileTestBackend) Create(context.Context, backend.CreateRequest) (backend.Sandbox, error) {
	return backend.Sandbox{}, errors.New("unexpected create")
}
func (b *reconcileTestBackend) Find(_ context.Context, localID, projectID string) (backend.Sandbox, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.localSandboxes[projectID+"/"+localID]
	if !ok {
		return backend.Sandbox{}, backend.ErrNotFound
	}
	return b.sandboxes[id], nil
}
func (b *reconcileTestBackend) Inspect(ctx context.Context, id string) (backend.Sandbox, error) {
	if b.inspectStarted != nil {
		b.inspectOnce.Do(func() { close(b.inspectStarted) })
	}
	if b.releaseInspect != nil {
		select {
		case <-b.releaseInspect:
		case <-ctx.Done():
			return backend.Sandbox{}, ctx.Err()
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inspectCalls++
	if err := b.inspectErrors[id]; err != nil {
		return backend.Sandbox{}, err
	}
	value, ok := b.sandboxes[id]
	if !ok {
		return backend.Sandbox{}, backend.ErrNotFound
	}
	return value, nil
}
func (b *reconcileTestBackend) Pause(_ context.Context, id string, _ domain.CheckpointKind) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pauseCalls++
	sandbox, ok := b.sandboxes[id]
	if !ok {
		return backend.ErrNotFound
	}
	sandbox.State = domain.SandboxStandby
	b.sandboxes[id] = sandbox
	return nil
}
func (b *reconcileTestBackend) Resume(context.Context, string, domain.CheckpointKind, int64) (backend.Sandbox, error) {
	return backend.Sandbox{}, errors.New("unexpected resume")
}
func (b *reconcileTestBackend) Delete(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deleteCalls++
	if b.deleteErr != nil {
		return b.deleteErr
	}
	if _, ok := b.sandboxes[id]; !ok {
		return backend.ErrNotFound
	}
	delete(b.sandboxes, id)
	return nil
}
func (b *reconcileTestBackend) Checkpoint(context.Context, string, domain.CheckpointKind, string) (backend.Checkpoint, error) {
	return backend.Checkpoint{}, errors.New("unexpected checkpoint")
}
func (b *reconcileTestBackend) DeleteCheckpoint(context.Context, string) error {
	return errors.New("unexpected checkpoint delete")
}
func (b *reconcileTestBackend) Run(_ context.Context, _ string, _ backend.CommandRequest, emit func(backend.CommandEvent) error) error {
	return emit(backend.CommandEvent{Type: backend.CommandExited, Exited: true, ExitCode: 0})
}
func (b *reconcileTestBackend) WriteFile(context.Context, string, string, io.Reader) (backend.FileInfo, error) {
	return backend.FileInfo{Path: "/tmp/output", Size: 2}, nil
}
func (b *reconcileTestBackend) ReadFile(_ context.Context, _ string, path string, destination io.Writer) (backend.FileInfo, error) {
	written, err := destination.Write([]byte("ok"))
	return backend.FileInfo{Path: path, Size: int64(written)}, err
}
func (b *reconcileTestBackend) ValidatePort(port uint16) error {
	if port == 0 {
		return errors.New("invalid port")
	}
	return nil
}
func (b *reconcileTestBackend) RoundTripPort(context.Context, string, uint16, *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
}
func (b *reconcileTestBackend) CreateWorkspace(context.Context, backend.WorkspaceCreateRequest) (backend.Workspace, error) {
	return backend.Workspace{}, errors.New("unexpected workspace create")
}
func (b *reconcileTestBackend) FindWorkspace(ctx context.Context, name string) (backend.Workspace, error) {
	if b.workspaceFindStarted != nil {
		b.workspaceFindOnce.Do(func() { close(b.workspaceFindStarted) })
	}
	if b.releaseWorkspaceFind != nil {
		select {
		case <-b.releaseWorkspaceFind:
		case <-ctx.Done():
			return backend.Workspace{}, ctx.Err()
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.workspaceFindErr != nil {
		return backend.Workspace{}, b.workspaceFindErr
	}
	workspace, ok := b.workspaces[name]
	if !ok {
		return backend.Workspace{}, backend.ErrNotFound
	}
	return workspace, nil
}
func (b *reconcileTestBackend) DeleteWorkspace(context.Context, string) error { return nil }

func newReconcileTestService(t *testing.T, state *reconcileTestStore, runtime *reconcileTestBackend, options ...Option) *Service {
	t.Helper()
	service, err := New(state, runtime, nil, options...)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func reconcileSandboxFixture(now time.Time, id, backendID string) domain.Sandbox {
	return domain.Sandbox{
		ID: id, ProjectID: "project-a", Backend: backend.DefaultName, BackendID: backendID,
		State: domain.SandboxRunning, Lifecycle: domain.Lifecycle{ExpiresAfterSeconds: 3600},
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour), Revision: 1,
	}
}

func TestReconcileExpirationDoesNotDeleteWithoutDurableIntent(t *testing.T) {
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	state := newReconcileTestStore()
	runtime := newReconcileTestBackend()
	sandbox := reconcileSandboxFixture(now.Add(-2*time.Hour), "sandbox-expired", "engine-expired")
	sandbox.ExpiresAt = now.Add(-time.Minute)
	state.state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
	runtime.sandboxes[sandbox.BackendID] = backend.Sandbox{ID: sandbox.BackendID, State: domain.SandboxRunning}
	state.failNextUpdate = errors.New("ledger unavailable")
	service := newReconcileTestService(t, state, runtime, WithClock(func() time.Time { return now }))

	err := service.reconcileSandboxBackendState(context.Background(), sandbox.ProjectID, sandbox.ID, nil)
	if err == nil || runtime.deleteCalls != 0 {
		t.Fatalf("err=%v delete calls=%d", err, runtime.deleteCalls)
	}
}

func TestWorkspaceReconcileDoesNotHoldServiceLockDuringBackendLookup(t *testing.T) {
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	state := newReconcileTestStore()
	runtime := newReconcileTestBackend()
	workspace := domain.Workspace{ID: "workspace-one", ProjectID: "project-a", BackendName: "workspace-one", State: domain.WorkspacePreparing, CreatedAt: now, UpdatedAt: now}
	sandbox := reconcileSandboxFixture(now, "sandbox-independent", "engine-independent")
	state.state.Workspaces[store.ScopedKey(workspace.ProjectID, workspace.ID)] = workspace
	state.state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
	runtime.workspaces[workspace.BackendName] = backend.Workspace{ID: "remote-workspace-one", Name: workspace.BackendName}
	runtime.sandboxes[sandbox.BackendID] = backend.Sandbox{ID: sandbox.BackendID, State: domain.SandboxRunning}
	runtime.workspaceFindStarted = make(chan struct{})
	runtime.releaseWorkspaceFind = make(chan struct{})
	service := newReconcileTestService(t, state, runtime, WithClock(func() time.Time { return now }))

	reconciled := make(chan error, 1)
	go func() {
		reconciled <- service.reconcileWorkspace(context.Background(), runtime, workspace.ProjectID, workspace.ID, nil)
	}()
	<-runtime.workspaceFindStarted
	if _, err := service.RunCommand(context.Background(), sandbox.ProjectID, sandbox.ID, RunCommandInput{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil }); err != nil {
		t.Fatalf("independent command waited behind workspace lookup: %v", err)
	}
	if err := state.Update(func(ledger *store.State) error {
		staleFence := ledger.Workspaces[store.ScopedKey(workspace.ProjectID, workspace.ID)]
		staleFence.State, staleFence.CleanupTarget, staleFence.UpdatedAt = domain.WorkspaceDeleting, domain.WorkspaceDeleted, now.Add(time.Minute)
		ledger.Workspaces[store.ScopedKey(workspace.ProjectID, workspace.ID)] = staleFence
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	close(runtime.releaseWorkspaceFind)
	if err := <-reconciled; err != nil {
		t.Fatal(err)
	}
	persisted, _ := service.getWorkspace(workspace.ProjectID, workspace.ID)
	if persisted.State != domain.WorkspaceDeleting || persisted.CleanupTarget != domain.WorkspaceDeleted {
		t.Fatalf("stale workspace observation overwrote current state: %#v", persisted)
	}
}

func TestRouteReconcileDoesNotHoldServiceLockDuringNodeLookup(t *testing.T) {
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	state := newReconcileTestStore()
	runtime := newReconcileTestBackend()
	target := reconcileSandboxFixture(now, "sandbox-routed", "engine-routed")
	target.NodeID, target.NodeRouteID, target.NodeGeneration = "node-a", "route-a", 1
	independent := reconcileSandboxFixture(now, "sandbox-independent", "engine-independent")
	state.state.Sandboxes[store.ScopedKey(target.ProjectID, target.ID)] = target
	state.state.Sandboxes[store.ScopedKey(independent.ProjectID, independent.ID)] = independent
	runtime.sandboxes[target.BackendID] = backend.Sandbox{ID: target.BackendID, State: domain.SandboxRunning}
	runtime.sandboxes[independent.BackendID] = backend.Sandbox{ID: independent.BackendID, State: domain.SandboxRunning}
	admin := &memoryRouteAdmin{
		nodeID: "node-a", route: nodeledger.Route{RouteID: target.NodeRouteID, ProjectID: target.ProjectID, SandboxID: target.ID, Generation: 1, State: nodeledger.StateReady},
		resolveStarted: make(chan struct{}), releaseResolve: make(chan struct{}),
	}
	service := newReconcileTestService(t, state, runtime, WithClock(func() time.Time { return now }), WithNodeRouteAdministrator(admin))

	reconciled := make(chan error, 1)
	go func() {
		reconciled <- service.reconcileSandboxBackendState(context.Background(), target.ProjectID, target.ID, nil)
	}()
	<-admin.resolveStarted
	if _, err := service.RunCommand(context.Background(), independent.ProjectID, independent.ID, RunCommandInput{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil }); err != nil {
		t.Fatalf("independent command waited behind route lookup: %v", err)
	}
	if err := state.Update(func(ledger *store.State) error {
		staleFence := ledger.Sandboxes[store.ScopedKey(target.ProjectID, target.ID)]
		staleFence.State, staleFence.Revision, staleFence.UpdatedAt = domain.SandboxStandby, staleFence.Revision+1, now.Add(time.Minute)
		ledger.Sandboxes[store.ScopedKey(target.ProjectID, target.ID)] = staleFence
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	close(admin.releaseResolve)
	if err := <-reconciled; err != nil {
		t.Fatal(err)
	}
	persisted, _ := service.getSandbox(target.ProjectID, target.ID)
	if persisted.State != domain.SandboxStandby || persisted.Revision != target.Revision+1 {
		t.Fatalf("stale route observation overwrote current state: %#v", persisted)
	}
}

func TestHealthyRouteObservationDoesNotFenceSameSandboxGuestOperations(t *testing.T) {
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	state := newReconcileTestStore()
	runtime := newReconcileTestBackend()
	sandbox := reconcileSandboxFixture(now, "sandbox-routed", "route-a")
	sandbox.NodeID, sandbox.NodeRouteID, sandbox.NodeGeneration = "node-a", "route-a", 1
	state.state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
	runtime.sandboxes[sandbox.BackendID] = backend.Sandbox{ID: sandbox.BackendID, State: domain.SandboxRunning}
	admin := &memoryRouteAdmin{
		nodeID: "node-a", route: nodeledger.Route{RouteID: sandbox.NodeRouteID, ProjectID: sandbox.ProjectID, SandboxID: sandbox.ID, Generation: 1, State: nodeledger.StateReady},
		resolveStarted: make(chan struct{}), releaseResolve: make(chan struct{}),
	}
	service := newReconcileTestService(t, state, runtime, WithClock(func() time.Time { return now }), WithNodeRouteAdministrator(admin))

	reconciled := make(chan error, 1)
	go func() {
		reconciled <- service.reconcileSandboxBackendState(context.Background(), sandbox.ProjectID, sandbox.ID, nil)
	}()
	<-admin.resolveStarted

	if _, err := service.RunCommand(context.Background(), sandbox.ProjectID, sandbox.ID, RunCommandInput{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil }); err != nil {
		t.Fatalf("same-sandbox command was fenced by route observation: %v", err)
	}
	if _, err := service.WriteFile(context.Background(), sandbox.ProjectID, sandbox.ID, "/tmp/output", strings.NewReader("ok")); err != nil {
		t.Fatalf("same-sandbox file write was fenced by route observation: %v", err)
	}
	if result, err := service.ReadFile(context.Background(), sandbox.ProjectID, sandbox.ID, "/tmp/output"); err != nil || string(result.Data) != "ok" {
		t.Fatalf("same-sandbox file read result=%q error=%v", result.Data, err)
	}
	request, err := http.NewRequest(http.MethodGet, "http://sandbox.local/", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.RoundTripPort(context.Background(), sandbox.ProjectID, sandbox.ID, 3000, request)
	if err != nil {
		t.Fatalf("same-sandbox preview was fenced by route observation: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}

	close(admin.releaseResolve)
	if err := <-reconciled; err != nil {
		t.Fatal(err)
	}
}

func TestMissingBackendRemovesNodeRouteBeforeTerminalState(t *testing.T) {
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	state := newReconcileTestStore()
	runtime := newReconcileTestBackend()
	sandbox := reconcileSandboxFixture(now, "sandbox-missing", "engine-missing")
	sandbox.NodeID, sandbox.NodeRouteID, sandbox.NodeGeneration = "node-a", "route-a", 1
	state.state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
	runtime.inspectErrors[sandbox.BackendID] = backend.ErrNotFound
	admin := &memoryRouteAdmin{nodeID: "node-a", route: nodeledger.Route{
		RouteID: sandbox.NodeRouteID, ProjectID: sandbox.ProjectID, SandboxID: sandbox.ID,
		Generation: 1, State: nodeledger.StateReady,
	}}
	service := newReconcileTestService(t, state, runtime, WithClock(func() time.Time { return now }), WithNodeRouteAdministrator(admin))

	if err := service.reconcileSandboxBackendState(context.Background(), sandbox.ProjectID, sandbox.ID, nil); err != nil {
		t.Fatal(err)
	}
	persisted, err := service.getSandbox(sandbox.ProjectID, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != domain.SandboxFailed || persisted.NodeRouteID != "" || !admin.removed {
		t.Fatalf("persisted=%#v route removed=%v", persisted, admin.removed)
	}
}

func TestReconcilePropagatesCleanupBackendFailure(t *testing.T) {
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	state := newReconcileTestStore()
	runtime := newReconcileTestBackend()
	sandbox := reconcileSandboxFixture(now, "sandbox-cleanup", "engine-cleanup")
	sandbox.State, sandbox.CleanupTarget = domain.SandboxDeleting, domain.SandboxDeleted
	state.state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
	runtime.sandboxes[sandbox.BackendID] = backend.Sandbox{ID: sandbox.BackendID, State: domain.SandboxRunning}
	runtime.deleteErr = errors.New("delete unavailable")
	service := newReconcileTestService(t, state, runtime, WithClock(func() time.Time { return now }))

	if err := service.reconcileSandboxBackendState(context.Background(), sandbox.ProjectID, sandbox.ID, nil); !errors.Is(err, ErrBackend) {
		t.Fatalf("error=%v", err)
	}
	persisted, _ := service.getSandbox(sandbox.ProjectID, sandbox.ID)
	if persisted.State != domain.SandboxUnknown || persisted.CleanupTarget != domain.SandboxDeleted {
		t.Fatalf("persisted=%#v", persisted)
	}
}

func TestReconcilePropagatesObservationBackendFailure(t *testing.T) {
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	state := newReconcileTestStore()
	runtime := newReconcileTestBackend()
	sandbox := reconcileSandboxFixture(now, "sandbox-observation", "engine-observation")
	state.state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
	runtime.inspectErrors[sandbox.BackendID] = errors.New("inspect unavailable")
	service := newReconcileTestService(t, state, runtime, WithClock(func() time.Time { return now.Add(time.Minute) }))

	if err := service.Reconcile(context.Background()); !errors.Is(err, ErrBackend) {
		t.Fatalf("error=%v", err)
	}
	persisted, _ := service.getSandbox(sandbox.ProjectID, sandbox.ID)
	if persisted.State != domain.SandboxUnknown || persisted.Failure == nil || persisted.Failure.Code != "backend_reconcile_failed" {
		t.Fatalf("persisted=%#v", persisted)
	}
}

func TestReconcileSettlesIncompleteOperationWithoutSandboxChurn(t *testing.T) {
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	state := newReconcileTestStore()
	runtime := newReconcileTestBackend()
	sandbox := reconcileSandboxFixture(now, "sandbox-running", "engine-running")
	operation := domain.Operation{ID: "operation-create", ProjectID: sandbox.ProjectID, ResourceID: sandbox.ID, Kind: "create_sandbox", State: domain.OperationRunning, CreatedAt: now, UpdatedAt: now}
	state.state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
	state.state.Operations[store.ScopedKey(operation.ProjectID, operation.ID)] = operation
	runtime.sandboxes[sandbox.BackendID] = backend.Sandbox{ID: sandbox.BackendID, State: domain.SandboxRunning}
	service := newReconcileTestService(t, state, runtime, WithClock(func() time.Time { return now.Add(time.Minute) }))

	if err := service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	persisted, _ := service.getSandbox(sandbox.ProjectID, sandbox.ID)
	persistedOperation, _ := service.GetOperation(operation.ProjectID, operation.ID)
	if persisted.Revision != sandbox.Revision || !persisted.UpdatedAt.Equal(sandbox.UpdatedAt) {
		t.Fatalf("sandbox churned: %#v", persisted)
	}
	if persistedOperation.State != domain.OperationSucceeded {
		t.Fatalf("operation=%#v", persistedOperation)
	}
}

func TestReconcileStopsWhenLifecycleOperationReadFails(t *testing.T) {
	state := newReconcileTestStore()
	runtime := newReconcileTestBackend()
	state.operationErr = errors.New("operation ledger unavailable")
	service := newReconcileTestService(t, state, runtime)

	if err := service.Reconcile(context.Background()); err == nil {
		t.Fatal("reconcile succeeded despite operation read failure")
	}
	if runtime.inspectCalls != 0 {
		t.Fatalf("backend was inspected %d times", runtime.inspectCalls)
	}
}

func TestReconcileRunsAutomaticStandbyWhenBackendReconciliationFails(t *testing.T) {
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	state := newReconcileTestStore()
	runtime := newReconcileTestBackend()
	workspace := domain.Workspace{ID: "workspace-failing", ProjectID: "project-a", BackendName: "workspace-failing", State: domain.WorkspacePreparing, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
	sandbox := reconcileSandboxFixture(now.Add(-time.Hour), "sandbox-idle", "engine-idle")
	sandbox.ExpiresAt = now.Add(time.Hour)
	sandbox.Lifecycle.StandbyAfterSeconds = 30
	sandbox.LastActiveAt = now.Add(-time.Minute)
	state.state.Workspaces[store.ScopedKey(workspace.ProjectID, workspace.ID)] = workspace
	state.state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
	runtime.sandboxes[sandbox.BackendID] = backend.Sandbox{ID: sandbox.BackendID, State: domain.SandboxRunning}
	runtime.workspaceFindErr = errors.New("workspace control unavailable")
	service := newReconcileTestService(t, state, runtime, WithClock(func() time.Time { return now }))

	if err := service.Reconcile(context.Background()); !errors.Is(err, ErrBackend) {
		t.Fatalf("error=%v", err)
	}
	persisted, _ := service.getSandbox(sandbox.ProjectID, sandbox.ID)
	if runtime.pauseCalls != 1 || persisted.State != domain.SandboxStandby {
		t.Fatalf("pause calls=%d sandbox=%#v", runtime.pauseCalls, persisted)
	}
}

func TestReconcileAggregatesBackendAndAutomaticStandbyErrors(t *testing.T) {
	state := newReconcileTestStore()
	runtime := newReconcileTestBackend()
	operationErr := errors.New("operation recovery unavailable")
	standbyErr := errors.New("standby scan unavailable")
	state.operationErr = operationErr
	state.viewErr = standbyErr
	service := newReconcileTestService(t, state, runtime)

	err := service.Reconcile(context.Background())
	if !errors.Is(err, operationErr) || !errors.Is(err, standbyErr) {
		t.Fatalf("error=%v", err)
	}
}

func TestReconcileCanceledContextDoesNotStartAutomaticStandby(t *testing.T) {
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	state := newReconcileTestStore()
	runtime := newReconcileTestBackend()
	sandbox := reconcileSandboxFixture(now.Add(-time.Hour), "sandbox-idle", "engine-idle")
	sandbox.ExpiresAt = now.Add(time.Hour)
	sandbox.Lifecycle.StandbyAfterSeconds = 30
	sandbox.LastActiveAt = now.Add(-time.Minute)
	state.state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
	runtime.sandboxes[sandbox.BackendID] = backend.Sandbox{ID: sandbox.BackendID, State: domain.SandboxRunning}
	service := newReconcileTestService(t, state, runtime, WithClock(func() time.Time { return now }))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := service.Reconcile(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	persisted, err := service.getSandbox(sandbox.ProjectID, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.pauseCalls != 0 || persisted.State != sandbox.State || persisted.Revision != sandbox.Revision ||
		!persisted.UpdatedAt.Equal(sandbox.UpdatedAt) || len(state.state.Operations) != 0 || len(state.state.Events) != 0 {
		t.Fatalf("pause calls=%d sandbox=%#v operations=%#v events=%#v", runtime.pauseCalls, persisted, state.state.Operations, state.state.Events)
	}
}

func TestSandboxReconcileCanceledDuringInspectDoesNotPersistObservation(t *testing.T) {
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	state := newReconcileTestStore()
	runtime := newReconcileTestBackend()
	sandbox := reconcileSandboxFixture(now.Add(-time.Hour), "sandbox-cancel-inspect", "engine-cancel-inspect")
	sandbox.ExpiresAt = now.Add(time.Hour)
	state.state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
	runtime.sandboxes[sandbox.BackendID] = backend.Sandbox{ID: sandbox.BackendID, State: domain.SandboxRunning}
	runtime.inspectStarted = make(chan struct{})
	runtime.releaseInspect = make(chan struct{})
	service := newReconcileTestService(t, state, runtime, WithClock(func() time.Time { return now }))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- service.reconcileSandboxBackendState(ctx, sandbox.ProjectID, sandbox.ID, nil)
	}()
	<-runtime.inspectStarted
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	persisted, err := service.getSandbox(sandbox.ProjectID, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != sandbox.State || persisted.Revision != sandbox.Revision ||
		!persisted.UpdatedAt.Equal(sandbox.UpdatedAt) || len(state.state.Operations) != 0 || len(state.state.Events) != 0 {
		t.Fatalf("sandbox=%#v operations=%#v events=%#v", persisted, state.state.Operations, state.state.Events)
	}
}

func TestWorkspaceReconcileCanceledDuringLookupDoesNotPersistObservation(t *testing.T) {
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	state := newReconcileTestStore()
	runtime := newReconcileTestBackend()
	workspace := domain.Workspace{ID: "workspace-cancel", ProjectID: "project-a", BackendName: "workspace-cancel", State: domain.WorkspacePreparing, CreatedAt: now, UpdatedAt: now}
	state.state.Workspaces[store.ScopedKey(workspace.ProjectID, workspace.ID)] = workspace
	runtime.workspaces[workspace.BackendName] = backend.Workspace{ID: "remote-workspace-cancel", Name: workspace.BackendName}
	runtime.workspaceFindStarted = make(chan struct{})
	runtime.releaseWorkspaceFind = make(chan struct{})
	service := newReconcileTestService(t, state, runtime, WithClock(func() time.Time { return now }))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- service.reconcileWorkspace(ctx, runtime, workspace.ProjectID, workspace.ID, nil)
	}()
	<-runtime.workspaceFindStarted
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	persisted, err := service.getWorkspace(workspace.ProjectID, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameWorkspaceReconcileVersion(persisted, workspace) || len(state.state.Operations) != 0 {
		t.Fatalf("workspace=%#v operations=%#v", persisted, state.state.Operations)
	}
}
