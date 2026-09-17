package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSimpleLifecycleClientUsesProductAPI(t *testing.T) {
	t.Helper()
	var environmentCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer a-service-token-that-is-long-enough" || r.Header.Get("X-Project-ID") != "project-a" {
			http.Error(w, "bad identity", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/environments":
			environmentCalls++
			if r.Header.Get("Idempotency-Key") != stableKey("environment", "base") {
				http.Error(w, "unstable environment key", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"resource": map[string]any{"revision_id": "envr_base"}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if body["environment_revision"] != "envr_base" {
				http.Error(w, "missing environment", http.StatusBadRequest)
				return
			}
			mounts, ok := body["workspace_mounts"].([]any)
			if !ok || len(mounts) != 1 || mounts[0].(map[string]any)["workspace_id"] != "wrk_123" || mounts[0].(map[string]any)["path"] != "/workspace" {
				http.Error(w, "missing workspace mount", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"resource": map[string]any{"id": "sbx_test", "state": "running"}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes":
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := newClient(server.URL, "a-service-token-that-is-long-enough", "project-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.newSandbox([]string{"--workspace", "wrk_123:/workspace"}); err != nil {
		t.Fatal(err)
	}
	if environmentCalls != 1 {
		t.Fatalf("environment calls = %d", environmentCalls)
	}
	if err := client.listSandboxes(nil); err != nil {
		t.Fatal(err)
	}
}

func TestSimpleLifecycleClientPreservesImmutableTemplateReference(t *testing.T) {
	const template = "template_123:01234567-89ab-cdef-0123-456789abcdef"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/environments":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			name, _ := body["name"].(string)
			if body["template"] != template || !strings.HasPrefix(name, "brezel-") || strings.Contains(name, ":") {
				http.Error(w, "immutable template identity was not separated from display name", http.StatusBadRequest)
				return
			}
			if r.Header.Get("Idempotency-Key") != stableKey("environment", template) {
				http.Error(w, "unstable environment key", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"resource": map[string]any{"revision_id": "envr_exact"}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			_ = json.NewEncoder(w).Encode(map[string]any{"resource": map[string]any{"id": "sbx_exact", "state": "running"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := newClient(server.URL, "a-service-token-that-is-long-enough", "project-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.newSandbox([]string{"--template", template}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceClientUsesProductAPI(t *testing.T) {
	var created bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/workspaces":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["name"] != "agent-state" || r.Header.Get("Idempotency-Key") == "" {
				http.Error(w, "bad workspace create", http.StatusBadRequest)
				return
			}
			created = true
			_ = json.NewEncoder(w).Encode(map[string]any{"resource": map[string]any{"id": "wrk_123", "state": "ready"}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/workspaces":
			_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": []any{}})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/workspaces/wrk_123":
			_ = json.NewEncoder(w).Encode(map[string]any{"resource": map[string]any{"id": "wrk_123", "state": "deleted"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := newClient(server.URL, "a-service-token-that-is-long-enough", "project-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.workspace([]string{"create", "agent-state"}); err != nil || !created {
		t.Fatalf("workspace create = %v, created=%v", err, created)
	}
	if err := client.workspace([]string{"list"}); err != nil {
		t.Fatal(err)
	}
	if err := client.workspace([]string{"delete", "wrk_123"}); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceMounts([]string{"wrk_123:relative"}); err == nil {
		t.Fatal("expected relative workspace mount to be rejected")
	}
}

func TestClientRejectsPlaintextRemoteAndWeakTokenFiles(t *testing.T) {
	if _, err := newClient("http://brezel.example.com", "a-service-token-that-is-long-enough", "project-a"); err == nil {
		t.Fatal("expected plaintext remote URL to be rejected")
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("a-service-token-that-is-long-enough"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readTokenFile(path); err == nil {
		t.Fatal("expected group-readable token file to be rejected")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if token, err := readTokenFile(path); err != nil || token == "" {
		t.Fatalf("private token file = %q, %v", token, err)
	}
}

func TestRunRequiresTokenFileAndDoesNotReadTokenValueEnvironment(t *testing.T) {
	t.Setenv("BREZEL_SERVICE_TOKEN", "a-service-token-that-is-long-enough")
	t.Setenv("BREZEL_SERVICE_TOKEN_FILE", "")
	err := run([]string{"list"})
	if err == nil || !strings.Contains(err.Error(), "token-file") {
		t.Fatalf("run() error = %v, want protected token-file requirement", err)
	}
}

func TestHelpAndVersionDoNotRequireCredentials(t *testing.T) {
	t.Setenv("BREZEL_SERVICE_TOKEN_FILE", "")
	for _, args := range [][]string{{"help"}, {"--help"}, {"version"}, {"version", "--json"}, {"--version"}} {
		if err := run(args); err != nil {
			t.Fatalf("run(%q) = %v", args, err)
		}
	}
}
