package conformance

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/infercrane/brezel/internal/domain"
	"github.com/infercrane/brezel/internal/receipt"
)

const conformanceToken = "conformance-test-token-that-is-long-enough"

type fakeControl struct {
	mu                    sync.Mutex
	signer                *receipt.Signer
	public                ed25519.PublicKey
	sandboxID             string
	sandboxIDs            map[string]string
	sandboxBodies         map[string]string
	checkpointBodies      map[string]string
	sandboxCreates        int
	workspaceID           string
	project               string
	deleteCalls           int
	checkpointDeleteCalls int
	workspaceDeleteCalls  int
	failPause             bool
	guestFile             []byte
}

func newFakeControl(t *testing.T) *fakeControl {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := receipt.NewSigner(private)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeControl{signer: signer, public: public, sandboxID: "sbx-conformance-1", sandboxIDs: map[string]string{}, sandboxBodies: map[string]string{}, checkpointBodies: map[string]string{}, workspaceID: "wrk-conformance", project: "project-a"}
}

func (f *fakeControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/readyz" && !strings.HasPrefix(r.URL.Path, "/p/") {
		if r.Header.Get("Authorization") != "Bearer "+conformanceToken || r.Header.Get("X-Project-ID") == "" {
			writeFake(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/readyz":
		writeFake(w, http.StatusOK, map[string]string{"status": "ready"})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/capabilities":
		writeFake(w, http.StatusOK, map[string]any{"qualification": "unverified", "implemented": map[string]bool{"durable_workspaces": true}})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/receipt-public-key":
		writeFake(w, http.StatusOK, map[string]string{"algorithm": "Ed25519", "public_key": base64.StdEncoding.EncodeToString(f.public)})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/environments":
		writeFake(w, http.StatusCreated, map[string]any{"resource": map[string]string{"revision_id": "envr-conformance"}})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/workspaces":
		writeFake(w, http.StatusAccepted, map[string]any{"resource": map[string]string{"id": f.workspaceID, "state": "ready"}})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/workspaces/"+f.workspaceID:
		if r.Header.Get("X-Project-ID") != f.project {
			writeFake(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		writeFake(w, http.StatusOK, map[string]string{"id": f.workspaceID, "state": "ready"})
	case r.Method == http.MethodDelete && r.URL.Path == "/v1/workspaces/"+f.workspaceID:
		f.mu.Lock()
		f.workspaceDeleteCalls++
		f.mu.Unlock()
		writeFake(w, http.StatusAccepted, map[string]any{"resource": map[string]string{"id": f.workspaceID, "state": "deleted"}})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
		f.mu.Lock()
		defer f.mu.Unlock()
		key := r.Header.Get("Idempotency-Key")
		body, _ := io.ReadAll(r.Body)
		id := f.sandboxIDs[key]
		if id != "" && f.sandboxBodies[key] != string(body) {
			writeFake(w, http.StatusConflict, map[string]string{"error": "idempotency payload changed"})
			return
		}
		if id == "" {
			f.sandboxCreates++
			id = fmt.Sprintf("sbx-conformance-%d", f.sandboxCreates)
			f.sandboxIDs[key] = id
			f.sandboxBodies[key] = string(body)
			if f.sandboxCreates == 1 {
				f.sandboxID = id
			}
		}
		writeFake(w, http.StatusAccepted, mutation(id, "running"))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/sandboxes/") && !strings.Contains(strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/"), "/"):
		if r.Header.Get("X-Project-ID") != f.project {
			writeFake(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		writeFake(w, http.StatusOK, map[string]string{"id": strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/"), "state": "running"})
	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/files"):
		if r.Header.Get("X-Project-ID") != f.project {
			writeFake(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		f.guestFile, _ = io.ReadAll(r.Body)
		writeFake(w, http.StatusOK, map[string]any{"size": len(f.guestFile)})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/files"):
		if r.Header.Get("X-Project-ID") != f.project {
			writeFake(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(f.guestFile)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commands"):
		if r.Header.Get("X-Project-ID") != f.project {
			writeFake(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "{\"type\":\"started\",\"pid\":1}\n{\"type\":\"stdout\",\"data\":%q}\n{\"type\":\"exited\",\"exited\":true}\n", base64.StdEncoding.EncodeToString(f.guestFile))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ports/8080/leases"):
		writeFake(w, http.StatusCreated, map[string]string{"path": "/p/conformance-lease/"})
	case r.Method == http.MethodGet && r.URL.Path == "/p/conformance-lease/":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(f.guestFile)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":pause"):
		if f.failPause {
			writeFake(w, http.StatusBadGateway, map[string]string{"error": "backend failure"})
			return
		}
		writeFake(w, http.StatusAccepted, mutation(f.sandboxID, "standby"))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":resume"):
		writeFake(w, http.StatusAccepted, mutation(f.sandboxID, "running"))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/checkpoints"):
		key := r.Header.Get("Idempotency-Key")
		body, _ := io.ReadAll(r.Body)
		if previous := f.checkpointBodies[key]; previous != "" && previous != string(body) {
			writeFake(w, http.StatusConflict, map[string]string{"error": "idempotency payload changed"})
			return
		}
		f.checkpointBodies[key] = string(body)
		writeFake(w, http.StatusAccepted, map[string]any{"resource": map[string]string{"id": "chk-conformance", "kind": "filesystem"}})
	case r.Method == http.MethodDelete && r.URL.Path == "/v1/checkpoints/chk-conformance":
		f.mu.Lock()
		f.checkpointDeleteCalls++
		f.mu.Unlock()
		writeFake(w, http.StatusAccepted, map[string]any{"operation": map[string]string{"state": "succeeded"}})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
		writeFake(w, http.StatusOK, map[string]any{"events": []map[string]int{{"sequence": 1}, {"sequence": 2}, {"sequence": 3}, {"sequence": 4}, {"sequence": 5}, {"sequence": 6}, {"sequence": 7}, {"sequence": 8}, {"sequence": 9}}})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/sandboxes/"):
		f.mu.Lock()
		f.deleteCalls++
		f.mu.Unlock()
		writeFake(w, http.StatusAccepted, mutation(strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/"), "deleted"))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/receipt"):
		sandboxID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/"), "/receipt")
		statement := receipt.Statement{
			Type: "https://in-toto.io/Statement/v1", PredicateType: "test",
			Predicate: receipt.Predicate{ProjectID: f.project, SandboxID: sandboxID, State: domain.SandboxDeleted},
		}
		envelope, _ := f.signer.Sign(statement)
		writeFake(w, http.StatusOK, envelope)
	default:
		writeFake(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func mutation(id, state string) map[string]any {
	return map[string]any{"resource": map[string]string{"id": id, "state": state}}
}

func writeFake(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func TestRunnerQualifiesOnlyAfterLifecycleAndReceipt(t *testing.T) {
	control := newFakeControl(t)
	server := httptest.NewServer(control)
	defer server.Close()
	runner, err := New(Config{
		BaseURL: server.URL, Token: conformanceToken, ProjectID: "project-a",
		OtherProjectID: "project-b", BackendTemplate: "template", Target: "local-test", Execute: true,
		Client: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Qualification != "sandbox_runtime_conformant" || report.FinishedAt.IsZero() {
		t.Fatalf("report = %#v", report)
	}
	if len(report.Steps) != 29 {
		t.Fatalf("got %d steps", len(report.Steps))
	}
	for _, step := range report.Steps {
		if step.Status != "passed" {
			t.Fatalf("step failed: %#v", step)
		}
	}
	if control.deleteCalls != 2 || control.checkpointDeleteCalls != 1 || control.workspaceDeleteCalls != 1 {
		t.Fatalf("cleanup calls = sandboxes:%d checkpoints:%d workspaces:%d", control.deleteCalls, control.checkpointDeleteCalls, control.workspaceDeleteCalls)
	}
}

func TestRunnerCleansUpAfterFailureAndNeverQualifies(t *testing.T) {
	control := newFakeControl(t)
	control.failPause = true
	server := httptest.NewServer(control)
	defer server.Close()
	runner, err := New(Config{
		BaseURL: server.URL, Token: conformanceToken, ProjectID: "project-a",
		OtherProjectID: "project-b", BackendTemplate: "template", Target: "failure-test", Execute: true,
		Client: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := runner.Run(context.Background())
	if err == nil || report.Qualification != "failed" || report.FinishedAt.IsZero() {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	last := report.Steps[len(report.Steps)-2:]
	if last[0].Name != "cleanup_after_failure" || last[0].Status != "passed" || last[1].Name != "cleanup_workspace_after_failure" || last[1].Status != "passed" {
		t.Fatalf("cleanup steps = %#v", last)
	}
	if control.deleteCalls != 1 || control.workspaceDeleteCalls != 1 {
		t.Fatalf("cleanup calls = sandboxes:%d workspaces:%d", control.deleteCalls, control.workspaceDeleteCalls)
	}
}

func TestNewRequiresExplicitExecutionAndProtectsTokensFromPlaintextRemote(t *testing.T) {
	base := Config{BaseURL: "https://brezel.example.com", Token: conformanceToken, ProjectID: "project-a", BackendTemplate: "template", Target: "test"}
	if _, err := New(base); err == nil {
		t.Fatal("runner allowed execution without explicit acknowledgement")
	}
	base.Execute = true
	base.BaseURL = "http://brezel.example.com"
	if _, err := New(base); err == nil {
		t.Fatal("runner allowed service token over remote plaintext")
	}
}
