package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/sandbox-runtime-lab/internal/backend"
	"github.com/infercrane/sandbox-runtime-lab/internal/conformance"
	"github.com/infercrane/sandbox-runtime-lab/internal/connector"
	"github.com/infercrane/sandbox-runtime-lab/internal/domain"
	"github.com/infercrane/sandbox-runtime-lab/internal/receipt"
	"github.com/infercrane/sandbox-runtime-lab/internal/service"
	"github.com/infercrane/sandbox-runtime-lab/internal/store"
)

const testToken = "a-test-service-token-that-is-long-enough"

type testBackend struct {
	mu                sync.Mutex
	createCalls       int
	sandboxes         map[string]backend.Sandbox
	byLocal           map[string]string
	capabilities      backend.Capabilities
	createUnconfirmed bool
	deleteFailures    int
	lastCreate        backend.CreateRequest
}

func newTestBackend() *testBackend {
	return &testBackend{
		sandboxes: map[string]backend.Sandbox{}, byLocal: map[string]string{},
		capabilities: backend.Capabilities{HostileCodeIsolation: true, DenyByDefaultEgress: true, FilesystemStandby: true, FullStateStandby: true, FilesystemCheckpoint: true, FullStateCheckpoint: true, AutoResume: true},
	}
}
func (b *testBackend) Name() string                       { return "e2b" }
func (b *testBackend) Capabilities() backend.Capabilities { return b.capabilities }
func (b *testBackend) Create(_ context.Context, in backend.CreateRequest) (backend.Sandbox, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.createCalls++
	b.lastCreate = in
	id := fmt.Sprintf("remote-%d", b.createCalls)
	value := backend.Sandbox{ID: id, State: domain.SandboxRunning}
	b.sandboxes[id] = value
	b.byLocal[in.ProjectID+"/"+in.LocalSandboxID] = id
	if b.createUnconfirmed {
		return backend.Sandbox{}, errors.New("unconfirmed create")
	}
	return value, nil
}
func (b *testBackend) Find(_ context.Context, local, project string) (backend.Sandbox, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.byLocal[project+"/"+local]
	if !ok {
		return backend.Sandbox{}, backend.ErrNotFound
	}
	return b.sandboxes[id], nil
}
func (b *testBackend) Inspect(_ context.Context, id string) (backend.Sandbox, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, ok := b.sandboxes[id]
	if !ok {
		return backend.Sandbox{}, backend.ErrNotFound
	}
	return value, nil
}
func (b *testBackend) Pause(_ context.Context, id string, _ domain.CheckpointKind) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, ok := b.sandboxes[id]
	if !ok {
		return backend.ErrNotFound
	}
	value.State = domain.SandboxStandby
	b.sandboxes[id] = value
	return nil
}
func (b *testBackend) Resume(_ context.Context, id string, _ domain.CheckpointKind, _ int64) (backend.Sandbox, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, ok := b.sandboxes[id]
	if !ok {
		return backend.Sandbox{}, backend.ErrNotFound
	}
	value.State = domain.SandboxRunning
	b.sandboxes[id] = value
	return value, nil
}
func (b *testBackend) Delete(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deleteFailures > 0 {
		b.deleteFailures--
		return errors.New("unconfirmed delete")
	}
	if _, ok := b.sandboxes[id]; !ok {
		return backend.ErrNotFound
	}
	delete(b.sandboxes, id)
	return nil
}
func (b *testBackend) Checkpoint(_ context.Context, id string, kind domain.CheckpointKind, name string) (backend.Checkpoint, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.sandboxes[id]; !ok {
		return backend.Checkpoint{}, backend.ErrNotFound
	}
	return backend.Checkpoint{Ref: "snapshot-" + name, Kind: kind}, nil
}

type harness struct {
	server    *httptest.Server
	backend   *testBackend
	service   *service.Service
	broker    *connector.Broker
	public    ed25519.PublicKey
	statePath string
}

func newHarness(t *testing.T) harness {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "state.json")
	st, err := store.OpenFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := receipt.NewSigner(private)
	be := newTestBackend()
	svc, err := service.New(st, be, signer)
	if err != nil {
		t.Fatal(err)
	}
	api, err := New(svc, testToken)
	if err != nil {
		t.Fatal(err)
	}
	return harness{server: httptest.NewServer(api.Handler()), backend: be, service: svc, public: public, statePath: statePath}
}

func newBrokerHarness(t *testing.T) harness {
	t.Helper()
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	st, err := store.OpenFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := receipt.NewSigner(private)
	secretDirectory := filepath.Join(directory, "secrets")
	if err := os.Mkdir(secretDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretDirectory, "model-api"), []byte("upstream-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver, _ := connector.NewFileResolver(secretDirectory)
	broker, err := connector.NewBroker(st, private, resolver, "http://127.0.0.1/connector/v1", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	be := newTestBackend()
	svc, err := service.New(st, be, signer, service.WithConnectorBroker(broker))
	if err != nil {
		t.Fatal(err)
	}
	api, err := New(svc, testToken, WithConnectorHandler(broker))
	if err != nil {
		t.Fatal(err)
	}
	return harness{server: httptest.NewServer(api.Handler()), backend: be, service: svc, broker: broker, public: public, statePath: statePath}
}

func (h harness) close() { h.server.Close() }

func request(t *testing.T, h harness, method, path, project, idem string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req, _ := http.NewRequest(method, h.server.URL+path, bytes.NewReader(encoded))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("X-Project-ID", project)
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp, out
}

func createEnvironment(t *testing.T, h harness, project string) string {
	resp, out := request(t, h, http.MethodPost, "/v1/environments", project, "environment-0001", map[string]any{"name": "python", "backend": "e2b", "backend_template": "python-313", "image_digest": "sha256:0123456789abcdef"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create environment: %d %#v", resp.StatusCode, out)
	}
	return out["resource"].(map[string]any)["revision_id"].(string)
}

func createSandbox(t *testing.T, h harness, project, envRevision, idem string, connectors []string) (string, map[string]any) {
	body := map[string]any{"environment_revision": envRevision, "lifecycle": map[string]any{"standby_after_seconds": 30, "expires_after_seconds": 3600, "standby_checkpoint_kind": "full_state", "auto_resume": true}, "network": map[string]any{"allow_internet": false}}
	if connectors != nil {
		body["connector_revisions"] = connectors
	}
	resp, out := request(t, h, http.MethodPost, "/v1/sandboxes", project, idem, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create sandbox: %d %#v", resp.StatusCode, out)
	}
	return out["resource"].(map[string]any)["id"].(string), out
}

func TestLifecycleIdempotencyTenantIsolationAndReceipt(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	envRevision := createEnvironment(t, h, "project-a")
	sandboxID, first := createSandbox(t, h, "project-a", envRevision, "sandbox-create-0001", nil)
	_, second := createSandbox(t, h, "project-a", envRevision, "sandbox-create-0001", nil)
	if first["resource"].(map[string]any)["id"] != second["resource"].(map[string]any)["id"] {
		t.Fatal("idempotent create returned another sandbox")
	}
	if h.backend.createCalls != 1 {
		t.Fatalf("backend create called %d times", h.backend.createCalls)
	}

	resp, _ := request(t, h, http.MethodGet, "/v1/sandboxes/"+sandboxID, "project-b", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant read returned %d", resp.StatusCode)
	}
	resp, out := request(t, h, http.MethodPost, "/v1/sandboxes/"+sandboxID+":pause", "project-a", "pause-sandbox-0001", nil)
	if resp.StatusCode != http.StatusAccepted || out["resource"].(map[string]any)["state"] != "standby" {
		t.Fatalf("pause failed: %d %#v", resp.StatusCode, out)
	}
	resp, out = request(t, h, http.MethodPost, "/v1/sandboxes/"+sandboxID+":resume", "project-a", "resume-sandbox-0001", nil)
	if resp.StatusCode != http.StatusAccepted || out["resource"].(map[string]any)["state"] != "running" {
		t.Fatalf("resume failed: %d %#v", resp.StatusCode, out)
	}
	resp, _ = request(t, h, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/checkpoints", "project-a", "checkpoint-0001", map[string]any{"name": "after-clone", "kind": "filesystem"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("checkpoint returned %d", resp.StatusCode)
	}
	resp, out = request(t, h, http.MethodGet, "/v1/sandboxes/"+sandboxID+"/receipt", "project-a", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("receipt returned %d %#v", resp.StatusCode, out)
	}
	envelope := receipt.Envelope{PayloadType: out["payloadType"].(string), Payload: out["payload"].(string)}
	for _, raw := range out["signatures"].([]any) {
		item := raw.(map[string]any)
		envelope.Signatures = append(envelope.Signatures, receipt.Signature{KeyID: item["keyid"].(string), Sig: item["sig"].(string)})
	}
	if err := receipt.Verify(envelope, h.public); err != nil {
		t.Fatal(err)
	}
	decoded, _ := base64.StdEncoding.DecodeString(envelope.Payload)
	if bytes.Contains(decoded, []byte("remote-1")) {
		t.Fatal("receipt leaked backend identity")
	}
}

func TestConnectorAttachmentFailsBeforeBackendWithoutCredentialBroker(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	h.backend.capabilities.EndpointCredentialBroker = false
	envRevision := createEnvironment(t, h, "project-a")
	resp, out := request(t, h, http.MethodPost, "/v1/connectors", "project-a", "connector-0001", map[string]any{"name": "private-model", "destination": "https://models.internal/v1", "allowed_methods": []string{"POST"}, "allowed_paths": []string{"/chat/completions"}, "credential_ref": "secret://vault/model-api"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connector: %d %#v", resp.StatusCode, out)
	}
	connector := out["resource"].(map[string]any)["revision_id"].(string)
	body := map[string]any{"environment_revision": envRevision, "connector_revisions": []string{connector}, "lifecycle": map[string]any{"expires_after_seconds": 3600}, "network": map[string]any{"allow_internet": false}}
	resp, _ = request(t, h, http.MethodPost, "/v1/sandboxes", "project-a", "sandbox-create-0002", body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("attachment returned %d", resp.StatusCode)
	}
	if h.backend.createCalls != 0 {
		t.Fatal("backend called despite failed capability preflight")
	}
}

func TestConfiguredBrokerIssuesOnlyShortLeaseAndAttachesGatewayPolicy(t *testing.T) {
	h := newBrokerHarness(t)
	defer h.close()
	envRevision := createEnvironment(t, h, "project-a")
	resp, out := request(t, h, http.MethodPost, "/v1/connectors", "project-a", "connector-broker-0001", map[string]any{"name": "private-model", "destination": "https://models.example.com/v1", "allowed_methods": []string{"POST"}, "allowed_paths": []string{"/chat/completions"}, "credential_ref": "secret://file/model-api"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connector: %d %#v", resp.StatusCode, out)
	}
	connectorRevision := out["resource"].(map[string]any)["revision_id"].(string)
	sandboxID, _ := createSandbox(t, h, "project-a", envRevision, "sandbox-with-broker-0001", []string{connectorRevision})
	h.backend.mu.Lock()
	lease := h.backend.lastCreate.Environment["RUNTIME_CONNECTOR_LEASE"]
	allowOut := append([]string(nil), h.backend.lastCreate.Network.AllowOut...)
	h.backend.mu.Unlock()
	if lease == "" {
		t.Fatal("backend did not receive a short connector lease")
	}
	if _, err := h.broker.Verify(lease, connectorRevision); err != nil {
		t.Fatal(err)
	}
	if len(allowOut) != 1 || allowOut[0] != "127.0.0.1" {
		t.Fatalf("effective allow_out = %#v", allowOut)
	}
	data, err := os.ReadFile(h.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(lease)) || bytes.Contains(data, []byte("upstream-secret")) {
		t.Fatal("durable control state contains credential material")
	}
	resp, got := request(t, h, http.MethodGet, "/v1/sandboxes/"+sandboxID, "project-a", "", nil)
	if resp.StatusCode != http.StatusOK || got["state"] != "running" {
		t.Fatalf("sandbox: %d %#v", resp.StatusCode, got)
	}
}

func TestReconcileRecoversUnconfirmedCreateAndRetriesCleanup(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	h.backend.createUnconfirmed = true
	envRevision := createEnvironment(t, h, "project-a")
	body := map[string]any{"environment_revision": envRevision, "lifecycle": map[string]any{"expires_after_seconds": 3600}, "network": map[string]any{"allow_internet": false}}
	resp, _ := request(t, h, http.MethodPost, "/v1/sandboxes", "project-a", "uncertain-create-0001", body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unconfirmed create returned %d", resp.StatusCode)
	}
	h.backend.createUnconfirmed = false
	if err := h.service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, retry := request(t, h, http.MethodPost, "/v1/sandboxes", "project-a", "uncertain-create-0001", body)
	sandbox := retry["resource"].(map[string]any)
	if sandbox["state"] != "running" {
		t.Fatalf("recovered state = %#v", sandbox["state"])
	}
	sandboxID := sandbox["id"].(string)

	h.backend.deleteFailures = 1
	resp, _ = request(t, h, http.MethodDelete, "/v1/sandboxes/"+sandboxID, "project-a", "uncertain-delete-0001", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unconfirmed delete returned %d", resp.StatusCode)
	}
	if err := h.service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	resp, got := request(t, h, http.MethodGet, "/v1/sandboxes/"+sandboxID, "project-a", "", nil)
	if resp.StatusCode != http.StatusOK || got["state"] != "deleted" {
		t.Fatalf("cleanup was not reconciled: %d %#v", resp.StatusCode, got)
	}
}

func TestRejectsUnknownFieldsAndUnauthenticatedRequests(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/v1/capabilities", nil)
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status %d", resp.StatusCode)
	}
	resp, _ = request(t, h, http.MethodPost, "/v1/environments", "project-a", "environment-0002", map[string]any{"name": "x", "backend": "e2b", "backend_template": "x", "secret": "should-not-be-accepted"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field status %d", resp.StatusCode)
	}
}

func TestConformanceRunnerAgainstControlAPI(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	runner, err := conformance.New(conformance.Config{
		BaseURL: h.server.URL, Token: testToken, ProjectID: "conformance-a",
		OtherProjectID: "conformance-b", BackendTemplate: "python-313",
		Target: "in-process-control-api", Execute: true, Client: h.server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Qualification != "control_plane_conformant" {
		t.Fatalf("qualification = %q", report.Qualification)
	}
}

var _ backend.Backend = (*testBackend)(nil)
var _ = errors.Is
