package e2b

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/brezel/internal/backend"
	"github.com/infercrane/brezel/internal/domain"
)

func newHealthyGuestServer(t *testing.T, next http.Handler) *httptest.Server {
	t.Helper()
	if next == nil {
		next = http.NotFoundHandler()
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/health" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func TestReadyUsesAuthenticatedEngineHealth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || r.Header.Get("X-API-Key") != "engine-secret" {
			t.Fatalf("health request = %s %q", r.URL.Path, r.Header.Get("X-API-Key"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client, err := New(server.URL, "engine-secret", server.Client(), WithGuestURLTemplate("http://127.0.0.1:3002"))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCreateMapsSecurityLifecycleAndTenantMetadata(t *testing.T) {
	guest := newHealthyGuestServer(t, nil)
	defer guest.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Fatalf("missing API key")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["secure"] != true || body["allow_internet_access"] != false || body["autoPauseMemory"] != true {
			t.Fatalf("unsafe create body: %#v", body)
		}
		if body["autoPause"] != false || body["timeout"] != float64(3600) {
			t.Fatalf("product-owned standby timer leaked to engine: %#v", body)
		}
		metadata := body["metadata"].(map[string]any)
		if metadata["brezel.project_id"] != "project-a" {
			t.Fatalf("tenant metadata missing: %#v", metadata)
		}
		network := body["network"].(map[string]any)
		if network["allowPublicTraffic"] != false {
			t.Fatalf("public traffic was not disabled: %#v", network)
		}
		if _, exists := network["allowOut"]; exists {
			t.Fatalf("empty allowOut must be omitted, not serialized as null: %#v", network)
		}
		if _, exists := network["denyOut"]; exists {
			t.Fatalf("empty denyOut must be omitted, not serialized as null: %#v", network)
		}
		mounts := body["volumeMounts"].([]any)
		if len(mounts) != 1 || mounts[0].(map[string]any)["name"] != "wrk_1" || mounts[0].(map[string]any)["path"] != "/workspace" {
			t.Fatalf("workspace mounts = %#v", mounts)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sandboxID":"upstream-1","envdAccessToken":"guest-token"}`))
	}))
	defer server.Close()

	client, err := New(server.URL, "test-key", server.Client(), WithGuestURLTemplate(guest.URL))
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Create(context.Background(), backend.CreateRequest{
		LocalSandboxID: "sbx-1", ProjectID: "project-a", TemplateID: "template-1",
		Lifecycle:       domain.Lifecycle{StandbyAfterSeconds: 30, ExpiresAfterSeconds: 3600, StandbyCheckpoint: domain.CheckpointFullState, AutoResume: true},
		Network:         domain.NetworkPolicy{AllowInternet: false},
		WorkspaceMounts: []backend.WorkspaceMount{{Name: "wrk_1", Path: "/workspace"}},
	})
	if err != nil || got.ID != "upstream-1" {
		t.Fatalf("Create() = %#v, %v", got, err)
	}
}

func TestCreateIncludesNonEmptyNetworkRules(t *testing.T) {
	guest := newHealthyGuestServer(t, nil)
	defer guest.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		network := body["network"].(map[string]any)
		allowOut := network["allowOut"].([]any)
		denyOut := network["denyOut"].([]any)
		if len(allowOut) != 1 || allowOut[0] != "api.example.com:443" {
			t.Fatalf("allowOut = %#v", allowOut)
		}
		if len(denyOut) != 1 || denyOut[0] != "169.254.169.254/32" {
			t.Fatalf("denyOut = %#v", denyOut)
		}
		if _, exists := body["volumeMounts"]; exists {
			t.Fatalf("empty volumeMounts must be omitted, not serialized as null: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sandboxID":"upstream-1","envdAccessToken":"guest-token"}`))
	}))
	defer server.Close()

	client, err := New(server.URL, "test-key", server.Client(), WithGuestURLTemplate(guest.URL))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Create(context.Background(), backend.CreateRequest{
		LocalSandboxID: "sbx-1", ProjectID: "project-a", TemplateID: "template-1",
		Lifecycle: domain.Lifecycle{ExpiresAfterSeconds: 3600},
		Network: domain.NetworkPolicy{
			AllowOut: []string{"api.example.com:443"},
			DenyOut:  []string{"169.254.169.254/32"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCreateWaitsForGuestDataPlaneReadiness(t *testing.T) {
	healthCalls := 0
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/health" {
			t.Fatalf("guest request = %s %s", r.Method, r.URL.Path)
		}
		healthCalls++
		if healthCalls < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer guest.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sandboxes" {
			t.Fatalf("engine request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sandboxID":"upstream-1","state":"running","envdAccessToken":"guest-token"}`))
	}))
	defer api.Close()
	client, err := New(api.URL, "test-key", api.Client(), WithGuestURLTemplate(guest.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Create(context.Background(), backend.CreateRequest{
		LocalSandboxID: "sbx-1", ProjectID: "project-a", TemplateID: "base",
		Lifecycle: domain.Lifecycle{ExpiresAfterSeconds: 600},
	}); err != nil {
		t.Fatal(err)
	}
	if healthCalls != 3 {
		t.Fatalf("health calls = %d, want 3", healthCalls)
	}
	if !client.recentlyLive("upstream-1") {
		t.Fatal("successful readiness did not establish the bounded liveness lease")
	}
}

func TestCreateDoesNotPublishGuestWhenReadinessTimesOut(t *testing.T) {
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer guest.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sandboxID":"upstream-1","state":"running","envdAccessToken":"guest-token"}`))
	}))
	defer api.Close()
	client, err := New(api.URL, "test-key", api.Client(), WithGuestURLTemplate(guest.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err = client.Create(ctx, backend.CreateRequest{
		LocalSandboxID: "sbx-1", ProjectID: "project-a", TemplateID: "base",
		Lifecycle: domain.Lifecycle{ExpiresAfterSeconds: 600},
	})
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("Create() error = %v", err)
	}
	if client.recentlyLive("upstream-1") {
		t.Fatal("failed readiness retained a liveness lease")
	}
	if _, ok := client.cachedGuestConnection("upstream-1"); ok {
		t.Fatal("failed readiness retained a guest credential")
	}
}

func TestWorkspaceProtocolNeverReturnsContentToken(t *testing.T) {
	var created bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Fatal("missing engine credential")
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/volumes":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["name"] != "wrk_local" {
				t.Fatalf("engine workspace name = %#v", body["name"])
			}
			created = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"volumeID":"remote-volume","name":"wrk_local","token":"must-not-leak"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/volumes":
			_, _ = w.Write([]byte(`[{"volumeID":"remote-volume","name":"wrk_local"}]`))
		case r.Method == http.MethodDelete && r.URL.Path == "/volumes/remote-volume":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "test-key", server.Client(), WithDurableWorkspaces(true))
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := client.CreateWorkspace(context.Background(), backend.WorkspaceCreateRequest{LocalWorkspaceID: "wrk_local", ProjectID: "project-a", Name: "customer-name"})
	if err != nil || !created || workspace.ID != "remote-volume" || workspace.Name != "wrk_local" {
		t.Fatalf("CreateWorkspace() = %#v, %v", workspace, err)
	}
	found, err := client.FindWorkspace(context.Background(), "wrk_local")
	if err != nil || found != workspace {
		t.Fatalf("FindWorkspace() = %#v, %v", found, err)
	}
	if err := client.DeleteWorkspace(context.Background(), workspace.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDurableWorkspacesAreExplicitlyAdvertised(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	client, err := New(server.URL, "test-key", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if client.Capabilities().DurableWorkspaces {
		t.Fatal("stock engine configuration advertised durable workspaces")
	}

	configured, err := New(server.URL, "test-key", server.Client(), WithDurableWorkspaces(true))
	if err != nil {
		t.Fatal(err)
	}
	if !configured.Capabilities().DurableWorkspaces {
		t.Fatal("configured engine did not advertise durable workspaces")
	}
}

func TestCheckpointUsesImmutableReferenceAndDeletesIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Fatal("missing engine credential")
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes/sandbox-1/snapshots":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body) != 0 {
				t.Fatalf("checkpoint request created a mutable alias: %#v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"snapshotID":"7c6f17c2-5f3a-4b8b-8fd8-d86ff29e7077:default"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/templates/7c6f17c2-5f3a-4b8b-8fd8-d86ff29e7077:default":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "test-key", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := client.Checkpoint(context.Background(), "sandbox-1", domain.CheckpointFilesystem, "customer-name")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteCheckpoint(context.Background(), checkpoint.Ref); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointRejectsUnsafeBackendReference(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"snapshotID":"../other-team"}`))
	}))
	defer server.Close()
	client, _ := New(server.URL, "test-key", server.Client())
	if _, err := client.Checkpoint(context.Background(), "sandbox-1", domain.CheckpointFilesystem, "name"); err == nil {
		t.Fatal("unsafe checkpoint backend reference was accepted")
	}
	if err := client.DeleteCheckpoint(context.Background(), "../other-team"); err == nil {
		t.Fatal("unsafe checkpoint backend reference was deleted")
	}
}

func TestUnknownRemoteStateStaysUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sandboxID":"upstream-1","state":"migrating","envdAccessToken":"guest-token"}`))
	}))
	defer server.Close()
	client, _ := New(server.URL, "test-key", server.Client())
	got, err := client.Inspect(context.Background(), "upstream-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.SandboxUnknown {
		t.Fatalf("unknown upstream state became %q", got.State)
	}
}

func TestInspectConfirmsRunningGuestWithoutExtendingKeepalive(t *testing.T) {
	guestCalls := 0
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		guestCalls++
		if r.Method != http.MethodGet || r.URL.Path != "/health" {
			t.Fatalf("guest probe = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Access-Token") != "guest-token" || r.Header.Get("E2b-Sandbox-Id") != "upstream-1" || r.Header.Get("E2b-Sandbox-Port") != "49983" {
			t.Fatalf("guest probe headers = %#v", r.Header)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer guest.Close()
	apiCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls++
		if r.Method != http.MethodGet || r.URL.Path != "/sandboxes/upstream-1" {
			t.Fatalf("engine inspection mutated state: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"sandboxID":"upstream-1","state":"running","envdAccessToken":"guest-token"}`))
	}))
	defer server.Close()
	client, _ := New(server.URL, "test-key", server.Client(), WithGuestURLTemplate(guest.URL))
	got, err := client.Inspect(context.Background(), "upstream-1")
	if err != nil || got.ID != "upstream-1" || got.State != domain.SandboxRunning {
		t.Fatalf("Inspect() = %#v, %v", got, err)
	}
	if apiCalls != 1 || guestCalls != 1 {
		t.Fatalf("calls = api:%d guest:%d, want one read-only call to each", apiCalls, guestCalls)
	}
	got, err = client.Inspect(context.Background(), "upstream-1")
	if err != nil || got.State != domain.SandboxRunning {
		t.Fatalf("cached Inspect() = %#v, %v", got, err)
	}
	if apiCalls != 2 || guestCalls != 1 {
		t.Fatalf("liveness lease calls = api:%d guest:%d, want two metadata reads and one guest probe", apiCalls, guestCalls)
	}
}

func TestInspectNeverClaimsRunningWhenGuestIsMissing(t *testing.T) {
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer guest.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sandboxID":"upstream-1","state":"running","envdAccessToken":"guest-token"}`))
	}))
	defer server.Close()
	client, _ := New(server.URL, "test-key", server.Client(), WithGuestURLTemplate(guest.URL))
	if got, err := client.Inspect(context.Background(), "upstream-1"); err == nil || got.State == domain.SandboxRunning {
		t.Fatalf("stale running state was accepted: %#v, %v", got, err)
	}
}

func TestFindConfirmsRecoveredRunningGuest(t *testing.T) {
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer guest.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/sandboxes" {
			t.Fatalf("engine lookup = %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"sandboxID":"upstream-1","state":"running","envdAccessToken":"guest-token","metadata":{"brezel.sandbox_id":"local-1","brezel.project_id":"project-a"}}]`))
	}))
	defer server.Close()
	client, _ := New(server.URL, "test-key", server.Client(), WithGuestURLTemplate(guest.URL))
	got, err := client.Find(context.Background(), "local-1", "project-a")
	if err != nil || got.State != domain.SandboxRunning {
		t.Fatalf("Find() = %#v, %v", got, err)
	}
}

func TestFindRecoversByTenantBoundMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/sandboxes" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("metadata") != "brezel.sandbox_id=local-1&brezel.project_id=project-a" {
			t.Fatalf("metadata = %q", r.URL.Query().Get("metadata"))
		}
		_, _ = w.Write([]byte(`[{"sandboxID":"upstream-1","state":"paused","envdAccessToken":"guest-token","metadata":{"brezel.sandbox_id":"local-1","brezel.project_id":"project-a"}}]`))
	}))
	defer server.Close()
	client, _ := New(server.URL, "test-key", server.Client())
	got, err := client.Find(context.Background(), "local-1", "project-a")
	if err != nil || got.ID != "upstream-1" || got.State != domain.SandboxStandby {
		t.Fatalf("Find() = %#v, %v", got, err)
	}
}

func TestRejectsBackendThatDoesNotConfirmSecuredGuestAccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sandboxID":"upstream-1"}`))
	}))
	defer server.Close()
	client, _ := New(server.URL, "test-key", server.Client())
	_, err := client.Create(context.Background(), backend.CreateRequest{
		LocalSandboxID: "sbx-1", ProjectID: "project-a", TemplateID: "template-1",
		Lifecycle: domain.Lifecycle{ExpiresAfterSeconds: 3600},
		Network:   domain.NetworkPolicy{AllowInternet: false},
	})
	if err == nil {
		t.Fatal("backend response without secured guest access was accepted")
	}
}

func TestRejectsRemotePlaintextAndRedirects(t *testing.T) {
	if _, err := New("http://brezel.example.com", "test-key", nil); err == nil {
		t.Fatal("expected remote plaintext URL to fail")
	}
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "" {
			t.Fatal("credential followed redirect")
		}
	}))
	defer redirectTarget.Close()
	server := httptest.NewServer(http.RedirectHandler(redirectTarget.URL, http.StatusTemporaryRedirect))
	defer server.Close()
	client, _ := New(server.URL, "test-key", server.Client())
	if _, err := client.Inspect(context.Background(), "sandbox-1"); err == nil {
		t.Fatal("expected redirect to be rejected")
	}
}
