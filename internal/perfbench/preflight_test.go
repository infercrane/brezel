package perfbench

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInspectEmptyProjectUsesAuthenticatedReadOnlyLists(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer 0123456789abcdef0123456789abcdef" {
			t.Error("preflight omitted its bearer credential")
		}
		if r.Header.Get("X-Project-ID") != "brezel-benchmark" {
			t.Error("preflight omitted its project binding")
		}
		if r.URL.Query().Get("include_terminal") != "false" {
			t.Error("preflight did not explicitly exclude terminal resources")
		}
		w.Header().Set("X-Request-ID", "request-"+r.URL.Path)
		switch r.URL.Path {
		case "/v1/sandboxes":
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []any{}})
		case "/v1/workspaces":
			_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": []any{}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	report, err := InspectEmptyProject(context.Background(), ProjectPreflightConfig{
		BaseURL: server.URL, Token: "0123456789abcdef0123456789abcdef", ProjectID: "brezel-benchmark",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Empty || report.ActiveSandboxes != 0 || report.ActiveWorkspaces != 0 || requests != 2 {
		t.Fatalf("report = %#v, requests = %d", report, requests)
	}
}

func TestInspectEmptyProjectRejectsExistingResourcesWithoutMutation(t *testing.T) {
	mutations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations++
		}
		switch r.URL.Path {
		case "/v1/sandboxes":
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []any{map[string]string{"id": "not-retained"}}})
		case "/v1/workspaces":
			_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": []any{map[string]string{"id": "also-not-retained"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	report, err := InspectEmptyProject(context.Background(), ProjectPreflightConfig{
		BaseURL: server.URL, Token: "0123456789abcdef0123456789abcdef", ProjectID: "brezel-benchmark",
	})
	if err == nil {
		t.Fatal("non-empty benchmark project was accepted")
	}
	if report.Empty || report.ActiveSandboxes != 1 || report.ActiveWorkspaces != 1 {
		t.Fatalf("report = %#v", report)
	}
	if mutations != 0 {
		t.Fatalf("preflight issued %d mutating requests", mutations)
	}
}

func TestInspectEmptyProjectRejectsMissingOrNullResourceArrays(t *testing.T) {
	for _, response := range []string{`{}`, `{"sandboxes":null}`} {
		t.Run(response, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(response))
			}))
			defer server.Close()

			report, err := InspectEmptyProject(context.Background(), ProjectPreflightConfig{
				BaseURL: server.URL, Token: "0123456789abcdef0123456789abcdef", ProjectID: "brezel-benchmark",
			})
			if err == nil {
				t.Fatalf("malformed project inventory was accepted: %#v", report)
			}
			if report.Empty {
				t.Fatalf("malformed project inventory was reported empty: %#v", report)
			}
		})
	}
}
