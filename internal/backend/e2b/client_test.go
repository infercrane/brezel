package e2b

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/infercrane/sandbox-runtime-lab/internal/backend"
	"github.com/infercrane/sandbox-runtime-lab/internal/domain"
)

func TestCreateMapsSecurityLifecycleAndTenantMetadata(t *testing.T) {
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
		metadata := body["metadata"].(map[string]any)
		if metadata["runtime.project_id"] != "project-a" {
			t.Fatalf("tenant metadata missing: %#v", metadata)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sandboxID":"upstream-1","envdAccessToken":"guest-token"}`))
	}))
	defer server.Close()

	client, err := New(server.URL, "test-key", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Create(context.Background(), backend.CreateRequest{
		LocalSandboxID: "sbx-1", ProjectID: "project-a", TemplateID: "template-1",
		Lifecycle: domain.Lifecycle{StandbyAfterSeconds: 30, ExpiresAfterSeconds: 3600, StandbyCheckpoint: domain.CheckpointFullState, AutoResume: true},
		Network:   domain.NetworkPolicy{AllowInternet: false},
	})
	if err != nil || got.ID != "upstream-1" {
		t.Fatalf("Create() = %#v, %v", got, err)
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

func TestFindRecoversByTenantBoundMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/sandboxes" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("metadata") != "runtime.sandbox_id=local-1&runtime.project_id=project-a" {
			t.Fatalf("metadata = %q", r.URL.Query().Get("metadata"))
		}
		_, _ = w.Write([]byte(`[{"sandboxID":"upstream-1","state":"paused","envdAccessToken":"guest-token","metadata":{"runtime.sandbox_id":"local-1","runtime.project_id":"project-a"}}]`))
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
	if _, err := New("http://runtime.example.com", "test-key", nil); err == nil {
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
