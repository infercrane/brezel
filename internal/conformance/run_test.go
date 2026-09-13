package conformance

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/infercrane/sandbox-runtime-lab/internal/domain"
	"github.com/infercrane/sandbox-runtime-lab/internal/receipt"
)

const conformanceToken = "conformance-test-token-that-is-long-enough"

type fakeControl struct {
	mu          sync.Mutex
	signer      *receipt.Signer
	public      ed25519.PublicKey
	sandboxID   string
	project     string
	createKey   string
	deleteCalls int
	failPause   bool
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
	return &fakeControl{signer: signer, public: public, sandboxID: "sbx-conformance", project: "project-a"}
}

func (f *fakeControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/readyz" {
		if r.Header.Get("Authorization") != "Bearer "+conformanceToken || r.Header.Get("X-Project-ID") == "" {
			writeFake(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/readyz":
		writeFake(w, http.StatusOK, map[string]string{"status": "ready"})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/capabilities":
		writeFake(w, http.StatusOK, map[string]string{"qualification": "unverified"})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/receipt-public-key":
		writeFake(w, http.StatusOK, map[string]string{"algorithm": "Ed25519", "public_key": base64.StdEncoding.EncodeToString(f.public)})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/environments":
		writeFake(w, http.StatusCreated, map[string]any{"resource": map[string]string{"revision_id": "envr-conformance"}})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
		f.mu.Lock()
		defer f.mu.Unlock()
		key := r.Header.Get("Idempotency-Key")
		if f.createKey == "" {
			f.createKey = key
		} else if f.createKey != key {
			writeFake(w, http.StatusConflict, map[string]string{"error": "different key"})
			return
		}
		writeFake(w, http.StatusAccepted, mutation("running"))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/"+f.sandboxID:
		if r.Header.Get("X-Project-ID") != f.project {
			writeFake(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		writeFake(w, http.StatusOK, map[string]string{"id": f.sandboxID, "state": "running"})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":pause"):
		if f.failPause {
			writeFake(w, http.StatusBadGateway, map[string]string{"error": "backend failure"})
			return
		}
		writeFake(w, http.StatusAccepted, mutation("standby"))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":resume"):
		writeFake(w, http.StatusAccepted, mutation("running"))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/checkpoints"):
		writeFake(w, http.StatusAccepted, map[string]any{"resource": map[string]string{"id": "chk-conformance", "kind": "filesystem"}})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
		writeFake(w, http.StatusOK, map[string]any{"events": []map[string]int{{"sequence": 1}, {"sequence": 2}, {"sequence": 3}, {"sequence": 4}, {"sequence": 5}}})
	case r.Method == http.MethodDelete && r.URL.Path == "/v1/sandboxes/"+f.sandboxID:
		f.mu.Lock()
		f.deleteCalls++
		f.mu.Unlock()
		writeFake(w, http.StatusAccepted, mutation("deleted"))
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/receipt"):
		statement := receipt.Statement{
			Type: "https://in-toto.io/Statement/v1", PredicateType: "test",
			Predicate: receipt.Predicate{ProjectID: f.project, SandboxID: f.sandboxID, State: domain.SandboxDeleted},
		}
		envelope, _ := f.signer.Sign(statement)
		writeFake(w, http.StatusOK, envelope)
	default:
		writeFake(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func mutation(state string) map[string]any {
	return map[string]any{"resource": map[string]string{"id": "sbx-conformance", "state": state}}
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
	if report.Qualification != "control_plane_conformant" || report.FinishedAt.IsZero() {
		t.Fatalf("report = %#v", report)
	}
	if len(report.Steps) != 13 {
		t.Fatalf("got %d steps", len(report.Steps))
	}
	for _, step := range report.Steps {
		if step.Status != "passed" {
			t.Fatalf("step failed: %#v", step)
		}
	}
	if control.deleteCalls != 1 {
		t.Fatalf("delete calls = %d", control.deleteCalls)
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
	last := report.Steps[len(report.Steps)-1]
	if last.Name != "cleanup_after_failure" || last.Status != "passed" {
		t.Fatalf("cleanup step = %#v", last)
	}
	if control.deleteCalls != 1 {
		t.Fatalf("delete calls = %d", control.deleteCalls)
	}
}

func TestNewRequiresExplicitExecutionAndProtectsTokensFromPlaintextRemote(t *testing.T) {
	base := Config{BaseURL: "https://runtime.example.com", Token: conformanceToken, ProjectID: "project-a", BackendTemplate: "template", Target: "test"}
	if _, err := New(base); err == nil {
		t.Fatal("runner allowed execution without explicit acknowledgement")
	}
	base.Execute = true
	base.BaseURL = "http://runtime.example.com"
	if _, err := New(base); err == nil {
		t.Fatal("runner allowed service token over remote plaintext")
	}
}
