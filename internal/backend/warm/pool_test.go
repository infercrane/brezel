package warm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/brezel/internal/backend"
	"github.com/infercrane/brezel/internal/domain"
)

type fakeSandbox struct {
	remote  backend.Sandbox
	localID string
	project string
}

type fakeBackend struct {
	mu         sync.Mutex
	next       int
	created    int
	deleted    int
	inspected  int
	byID       map[string]fakeSandbox
	timeout    map[string]int64
	deleteFail error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{byID: make(map[string]fakeSandbox), timeout: make(map[string]int64)}
}

func (*fakeBackend) Name() string { return backend.DefaultName }
func (*fakeBackend) Capabilities() backend.Capabilities {
	return backend.Capabilities{HostileCodeIsolation: true, DenyByDefaultEgress: true}
}
func (f *fakeBackend) Create(_ context.Context, request backend.CreateRequest) (backend.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, current := range f.byID {
		if current.localID == request.LocalSandboxID && current.project == request.ProjectID {
			return current.remote, nil
		}
	}
	f.next++
	f.created++
	remote := backend.Sandbox{ID: fmt.Sprintf("remote-%d", f.next), State: domain.SandboxRunning}
	f.byID[remote.ID] = fakeSandbox{remote: remote, localID: request.LocalSandboxID, project: request.ProjectID}
	return remote, nil
}
func (f *fakeBackend) Find(_ context.Context, localID, project string) (backend.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, current := range f.byID {
		if current.localID == localID && current.project == project {
			return current.remote, nil
		}
	}
	return backend.Sandbox{}, backend.ErrNotFound
}
func (f *fakeBackend) Inspect(_ context.Context, id string) (backend.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspected++
	current, ok := f.byID[id]
	if !ok {
		return backend.Sandbox{}, backend.ErrNotFound
	}
	return current.remote, nil
}
func (*fakeBackend) Pause(context.Context, string, domain.CheckpointKind) error { return nil }
func (f *fakeBackend) Resume(ctx context.Context, id string, _ domain.CheckpointKind, _ int64) (backend.Sandbox, error) {
	return f.Inspect(ctx, id)
}
func (f *fakeBackend) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteFail != nil {
		return f.deleteFail
	}
	if _, ok := f.byID[id]; !ok {
		return backend.ErrNotFound
	}
	delete(f.byID, id)
	f.deleted++
	return nil
}
func (*fakeBackend) Checkpoint(context.Context, string, domain.CheckpointKind, string) (backend.Checkpoint, error) {
	return backend.Checkpoint{}, backend.ErrCapabilityUnavailable
}
func (*fakeBackend) DeleteCheckpoint(context.Context, string) error { return nil }
func (*fakeBackend) Ready(context.Context) error                    { return nil }
func (f *fakeBackend) SetTimeout(_ context.Context, id string, ttlSeconds int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.byID[id]
	if !ok {
		return backend.ErrNotFound
	}
	if current.remote.State != domain.SandboxRunning {
		return errors.New("sandbox is not running")
	}
	f.timeout[id] = ttlSeconds
	return nil
}

func testConfig(t *testing.T, target int) Config {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("restrict test state directory: %v", err)
	}
	return Config{
		Path:             filepath.Join(directory, "warm.db"),
		Target:           target,
		TemplateID:       "base",
		SlotTTL:          10 * time.Minute,
		MaxClaimTTL:      5 * time.Minute,
		PrimeConcurrency: target,
		Network:          domain.NetworkPolicy{},
	}
}

func claimRequest(index int) backend.CreateRequest {
	return backend.CreateRequest{
		LocalSandboxID: fmt.Sprintf("sandbox-%d", index),
		ProjectID:      "project-a",
		TemplateID:     "base",
		Lifecycle:      domain.Lifecycle{ExpiresAfterSeconds: 120},
		Network:        domain.NetworkPolicy{},
	}
}

func TestPoolPrimesClaimsOnceAndRefillsAfterDelete(t *testing.T) {
	inner := newFakeBackend()
	pool, err := New(inner, testConfig(t, 2))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if inner.created != 2 {
		t.Fatalf("created = %d, want 2", inner.created)
	}

	first, err := pool.Create(context.Background(), claimRequest(1))
	if err != nil {
		t.Fatal(err)
	}
	retried, err := pool.Create(context.Background(), claimRequest(1))
	if err != nil || retried.ID != first.ID {
		t.Fatalf("retry = %#v, %v; want %q", retried, err, first.ID)
	}
	if inner.created != 2 {
		t.Fatalf("claim started a new sandbox; created = %d", inner.created)
	}
	if inner.inspected != 1 {
		t.Fatalf("inspect calls = %d, want only the idempotent retry to inspect", inner.inspected)
	}
	if got := inner.timeout[first.ID]; got < 119 || got > 120 {
		t.Fatalf("claimed timeout = %d, want the original 120-second deadline", got)
	}
	if err := pool.Delete(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	if inner.deleted != 1 || inner.created != 3 {
		t.Fatalf("delete/refill counts = deleted %d, created %d", inner.deleted, inner.created)
	}
	second, err := pool.Create(context.Background(), claimRequest(2))
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatal("a customer sandbox was returned to the warm pool")
	}
}

func TestPoolClaimsAreExclusiveUnderConcurrency(t *testing.T) {
	const count = 32
	inner := newFakeBackend()
	pool, err := New(inner, testConfig(t, count))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}

	ids := make(chan string, count)
	errs := make(chan error, count)
	var wait sync.WaitGroup
	for i := 0; i < count; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			claimed, claimErr := pool.Create(context.Background(), claimRequest(index))
			if claimErr != nil {
				errs <- claimErr
				return
			}
			ids <- claimed.ID
		}(i)
	}
	wait.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	unique := make(map[string]bool, count)
	for id := range ids {
		if unique[id] {
			t.Fatalf("warm slot %q was claimed more than once", id)
		}
		unique[id] = true
	}
	if len(unique) != count || inner.created != count {
		t.Fatalf("unique claims = %d, creates = %d", len(unique), inner.created)
	}
}

func TestPoolRecoversAvailableCapacityWithoutRecreating(t *testing.T) {
	inner := newFakeBackend()
	config := testConfig(t, 3)
	first, err := New(inner, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(inner, config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if inner.created != 3 {
		t.Fatalf("restart recreated healthy warm capacity; creates = %d", inner.created)
	}
}

func TestPoolRecoversAClaimAndPreservesItsOriginalDeadline(t *testing.T) {
	inner := newFakeBackend()
	config := testConfig(t, 1)
	first, err := New(inner, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	claim, found, err := first.beginClaim("project-a", "sandbox-recovered", 120*time.Second)
	if err != nil || !found {
		t.Fatalf("begin claim = %#v, %v, %v", claim, found, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(inner, config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.Find(context.Background(), "sandbox-recovered", "project-a")
	if err != nil || recovered.ID != claim.BackendID {
		t.Fatalf("recovered claim = %#v, %v", recovered, err)
	}
	if got := inner.timeout[recovered.ID]; got < 119 || got > 120 {
		t.Fatalf("recovered timeout = %d, want remaining original deadline", got)
	}
	persisted, found, err := restarted.findClaim("project-a", "sandbox-recovered")
	if err != nil || !found || persisted.State != stateClaimed {
		t.Fatalf("persisted claim = %#v, %v, %v", persisted, found, err)
	}
}

func TestPoolRejectsPublicDurableStatePermissions(t *testing.T) {
	inner := newFakeBackend()
	config := testConfig(t, 1)
	pool, err := New(inner, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(config.Path, 0o644); err != nil {
		t.Fatal(err)
	}
	if reopened, err := New(inner, config); err == nil {
		reopened.Close()
		t.Fatal("warm pool accepted a publicly readable database")
	}
}

func TestPoolRejectsASecondWriter(t *testing.T) {
	inner := newFakeBackend()
	config := testConfig(t, 1)
	pool, err := New(inner, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if second, err := New(inner, config); err == nil {
		second.Close()
		t.Fatal("warm pool admitted a second ledger writer")
	}
}

func TestPoolDelegatesRequestsWithCustomerConfiguration(t *testing.T) {
	inner := newFakeBackend()
	pool, err := New(inner, testConfig(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	request := claimRequest(1)
	request.Environment = map[string]string{"CUSTOM": "true"}
	created, err := pool.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "remote-2" || inner.created != 2 {
		t.Fatalf("configured request incorrectly used warm state: %#v, creates %d", created, inner.created)
	}
}

func TestStrictPoolRejectsOversubscriptionBeforeBackendCreate(t *testing.T) {
	inner := newFakeBackend()
	config := testConfig(t, 1)
	config.Strict = true
	pool, err := New(inner, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Create(context.Background(), claimRequest(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Create(context.Background(), claimRequest(2)); !errors.Is(err, backend.ErrCapacityUnavailable) {
		t.Fatalf("second claim error = %v, want capacity unavailable", err)
	}
	configured := claimRequest(3)
	configured.Environment = map[string]string{"CUSTOM": "true"}
	if _, err := pool.Create(context.Background(), configured); !errors.Is(err, backend.ErrCapacityUnavailable) {
		t.Fatalf("configured claim error = %v, want capacity unavailable", err)
	}
	if inner.created != 1 {
		t.Fatalf("strict capacity oversubscribed backend: creates = %d", inner.created)
	}
}

func TestMaintenanceCannotRetireAClaimedSlot(t *testing.T) {
	inner := newFakeBackend()
	pool, err := New(inner, testConfig(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	available, err := pool.listStates(stateAvailable)
	if err != nil || len(available) != 1 {
		t.Fatalf("available = %#v, %v", available, err)
	}
	if _, found, err := pool.beginClaim("project-a", "claimed-during-maintenance", 120*time.Second); err != nil || !found {
		t.Fatalf("claim = %v, %v", found, err)
	}
	retired, err := pool.retireAvailable(available[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if retired {
		t.Fatal("maintenance retired a customer-owned slot")
	}
	if inner.deleted != 0 {
		t.Fatalf("maintenance deleted %d claimed resources", inner.deleted)
	}
}

func TestUnusableClaimIsQuarantinedBeforeDestruction(t *testing.T) {
	inner := newFakeBackend()
	config := testConfig(t, 1)
	config.Strict = true
	pool, err := New(inner, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	available, err := pool.listStates(stateAvailable)
	if err != nil || len(available) != 1 {
		t.Fatalf("available = %#v, %v", available, err)
	}
	inner.mu.Lock()
	failed := inner.byID[available[0].BackendID]
	failed.remote.State = domain.SandboxFailed
	inner.byID[available[0].BackendID] = failed
	inner.deleteFail = errors.New("backend deletion unavailable")
	inner.mu.Unlock()

	if _, err := pool.Create(context.Background(), claimRequest(1)); err == nil || errors.Is(err, backend.ErrCapacityUnavailable) {
		t.Fatalf("claim error = %v, want ambiguous cleanup failure", err)
	}
	quarantined, err := pool.listStates(stateDeleting)
	if err != nil || len(quarantined) != 1 {
		t.Fatalf("deleting = %#v, %v, want one quarantined slot", quarantined, err)
	}
	if available, err := pool.listStates(stateAvailable); err != nil || len(available) != 0 {
		t.Fatalf("available = %#v, %v, want no reusable capacity", available, err)
	}
}

var _ backend.Backend = (*fakeBackend)(nil)
var _ backend.ReadinessBackend = (*fakeBackend)(nil)
var _ backend.TimeoutRuntime = (*fakeBackend)(nil)
