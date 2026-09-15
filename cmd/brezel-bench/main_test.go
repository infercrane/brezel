package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadServiceTokenRequiresPrivateRegularFile(t *testing.T) {
	directory := t.TempDir()
	private := filepath.Join(directory, "service.token")
	if err := os.WriteFile(private, []byte("0123456789abcdef0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BREZEL_SERVICE_TOKEN_FILE", private)
	token, err := loadServiceToken()
	if err != nil || token != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("loadServiceToken() = %q, %v", token, err)
	}
	public := filepath.Join(directory, "public.token")
	if err := os.WriteFile(public, []byte("0123456789abcdef0123456789abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BREZEL_SERVICE_TOKEN_FILE", public)
	if _, err := loadServiceToken(); err == nil {
		t.Fatal("world-readable token was accepted")
	}
}

func TestRunProjectPreflightRejectsExistingSandboxWithoutExecutionFlag(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "service.token")
	if err := os.WriteFile(tokenPath, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BREZEL_SERVICE_TOKEN_FILE", tokenPath)
	mutations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations++
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/sandboxes":
			_, _ = w.Write([]byte(`{"sandboxes":[{"id":"existing"}]}`))
		case "/v1/workspaces":
			_, _ = w.Write([]byte(`{"workspaces":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stdout strings.Builder
	err := run(context.Background(), []string{"-preflight-empty-project", "-base-url", server.URL, "-project", "brezel-benchmark"}, &stdout, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("preflight error = %v", err)
	}
	if !strings.Contains(stdout.String(), `"active_sandboxes": 1`) || !strings.Contains(stdout.String(), `"empty": false`) {
		t.Fatalf("preflight report = %s", stdout.String())
	}
	if mutations != 0 {
		t.Fatalf("preflight issued %d mutations", mutations)
	}
}

func TestRunRequiresExplicitExecutionBeforeCredentials(t *testing.T) {
	t.Setenv("BREZEL_SERVICE_TOKEN_FILE", "")
	err := run(context.Background(), nil, &strings.Builder{}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "explicit") {
		t.Fatalf("run() error = %v", err)
	}
}

func TestRunRejectsOutOfRangeScenarioParametersBeforeCredentials(t *testing.T) {
	t.Setenv("BREZEL_SERVICE_TOKEN_FILE", "")
	for _, args := range [][]string{
		{"-execute", "-io-bytes", "0"},
		{"-execute", "-io-bytes", "33554433"},
		{"-execute", "-preview-port", "0"},
		{"-execute", "-preview-port", "65536"},
	} {
		if err := run(context.Background(), args, &strings.Builder{}, &strings.Builder{}); err == nil || (!strings.Contains(err.Error(), "io-bytes") && !strings.Contains(err.Error(), "preview-port")) {
			t.Fatalf("run(%v) error = %v", args, err)
		}
	}
}

func TestRunProjectPreflightHonorsParentCancellation(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "service.token")
	if err := os.WriteFile(tokenPath, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BREZEL_SERVICE_TOKEN_FILE", tokenPath)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := run(ctx, []string{"-preflight-empty-project"}, &strings.Builder{}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("run() error = %v, want context cancellation", err)
	}
}
