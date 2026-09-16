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
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/brezel/internal/backend"
	"github.com/infercrane/brezel/internal/conformance"
	"github.com/infercrane/brezel/internal/connector"
	"github.com/infercrane/brezel/internal/domain"
	"github.com/infercrane/brezel/internal/receipt"
	"github.com/infercrane/brezel/internal/service"
	"github.com/infercrane/brezel/internal/store"
	"github.com/infercrane/brezel/internal/telemetry"
)

const testToken = "a-test-service-token-that-is-long-enough"

type testBackend struct {
	mu                         sync.Mutex
	createCalls                int
	sandboxes                  map[string]backend.Sandbox
	byLocal                    map[string]string
	capabilities               backend.Capabilities
	createUnconfirmed          bool
	createError                error
	deleteFailures             int
	lastCreate                 backend.CreateRequest
	lastCommand                backend.CommandRequest
	lastPort                   uint16
	lastPortPath               string
	files                      map[string][]byte
	sandboxMounts              map[string][]backend.WorkspaceMount
	workspaceFiles             map[string][]byte
	workspaces                 map[string]backend.Workspace
	workspaceByName            map[string]string
	checkpoints                map[string]backend.Checkpoint
	workspaceCreates           int
	workspaceCreateUnconfirmed bool
	workspaceDeleteFailures    int
	checkpointDeleteFailures   int
	readyErr                   error
	runStarted                 chan struct{}
	releaseRun                 chan struct{}
	runStartedOnce             sync.Once
	createStarted              chan struct{}
	releaseCreate              chan struct{}
	resumeStarted              chan struct{}
	releaseResume              chan struct{}
	pauseStarted               chan string
	releasePause               chan struct{}
	pauseCalls                 int
	pauseFailures              int
	checkpointStarted          chan struct{}
	releaseCheckpoint          chan struct{}
	inspectStarted             chan string
	releaseInspect             chan struct{}
}

func newTestBackend() *testBackend {
	return &testBackend{
		sandboxes: map[string]backend.Sandbox{}, byLocal: map[string]string{}, files: map[string][]byte{}, sandboxMounts: map[string][]backend.WorkspaceMount{}, workspaceFiles: map[string][]byte{},
		workspaces: map[string]backend.Workspace{}, workspaceByName: map[string]string{}, checkpoints: map[string]backend.Checkpoint{},
		capabilities: backend.Capabilities{HostileCodeIsolation: true, DenyByDefaultEgress: true, FilesystemStandby: true, FullStateStandby: true, FilesystemCheckpoint: true, FullStateCheckpoint: true, AutoResume: true, CommandStreaming: true, FileReadWrite: true, AuthenticatedPorts: true, DurableWorkspaces: true},
	}
}
func (b *testBackend) Name() string                       { return backend.DefaultName }
func (b *testBackend) Capabilities() backend.Capabilities { return b.capabilities }
func (b *testBackend) Ready(context.Context) error        { return b.readyErr }
func (b *testBackend) Create(ctx context.Context, in backend.CreateRequest) (backend.Sandbox, error) {
	b.mu.Lock()
	b.createCalls++
	b.lastCreate = in
	id := fmt.Sprintf("remote-%d", b.createCalls)
	createUnconfirmed := b.createUnconfirmed
	createError := b.createError
	createStarted := b.createStarted
	releaseCreate := b.releaseCreate
	b.mu.Unlock()
	if createStarted != nil {
		select {
		case createStarted <- struct{}{}:
		case <-ctx.Done():
			return backend.Sandbox{}, ctx.Err()
		}
	}
	if releaseCreate != nil {
		select {
		case <-releaseCreate:
		case <-ctx.Done():
			return backend.Sandbox{}, ctx.Err()
		}
	}
	if createError != nil {
		return backend.Sandbox{}, createError
	}
	value := backend.Sandbox{ID: id, State: domain.SandboxRunning}
	b.mu.Lock()
	b.sandboxes[id] = value
	b.sandboxMounts[id] = append([]backend.WorkspaceMount(nil), in.WorkspaceMounts...)
	b.byLocal[in.ProjectID+"/"+in.LocalSandboxID] = id
	b.mu.Unlock()
	if createUnconfirmed {
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
func (b *testBackend) Inspect(ctx context.Context, id string) (backend.Sandbox, error) {
	b.mu.Lock()
	value, ok := b.sandboxes[id]
	inspectStarted := b.inspectStarted
	releaseInspect := b.releaseInspect
	b.mu.Unlock()
	if !ok {
		return backend.Sandbox{}, backend.ErrNotFound
	}
	if inspectStarted != nil {
		select {
		case inspectStarted <- id:
		case <-ctx.Done():
			return backend.Sandbox{}, ctx.Err()
		}
	}
	if releaseInspect != nil {
		select {
		case <-releaseInspect:
		case <-ctx.Done():
			return backend.Sandbox{}, ctx.Err()
		}
	}
	return value, nil
}
func (b *testBackend) Pause(ctx context.Context, id string, _ domain.CheckpointKind) error {
	b.mu.Lock()
	b.pauseCalls++
	pauseStarted := b.pauseStarted
	releasePause := b.releasePause
	pauseFailures := b.pauseFailures
	if b.pauseFailures > 0 {
		b.pauseFailures--
	}
	b.mu.Unlock()
	if pauseStarted != nil {
		select {
		case pauseStarted <- id:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if releasePause != nil {
		select {
		case <-releasePause:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if pauseFailures > 0 {
		return errors.New("unconfirmed pause")
	}
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
func (b *testBackend) Resume(ctx context.Context, id string, _ domain.CheckpointKind, _ int64) (backend.Sandbox, error) {
	b.mu.Lock()
	value, ok := b.sandboxes[id]
	resumeStarted := b.resumeStarted
	releaseResume := b.releaseResume
	b.mu.Unlock()
	if !ok {
		return backend.Sandbox{}, backend.ErrNotFound
	}
	if resumeStarted != nil {
		select {
		case resumeStarted <- struct{}{}:
		case <-ctx.Done():
			return backend.Sandbox{}, ctx.Err()
		}
	}
	if releaseResume != nil {
		select {
		case <-releaseResume:
		case <-ctx.Done():
			return backend.Sandbox{}, ctx.Err()
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	value, ok = b.sandboxes[id]
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
	delete(b.sandboxMounts, id)
	return nil
}
func (b *testBackend) Checkpoint(ctx context.Context, id string, kind domain.CheckpointKind, name string) (backend.Checkpoint, error) {
	b.mu.Lock()
	if _, ok := b.sandboxes[id]; !ok {
		b.mu.Unlock()
		return backend.Checkpoint{}, backend.ErrNotFound
	}
	checkpointStarted := b.checkpointStarted
	releaseCheckpoint := b.releaseCheckpoint
	b.mu.Unlock()
	if checkpointStarted != nil {
		select {
		case checkpointStarted <- struct{}{}:
		case <-ctx.Done():
			return backend.Checkpoint{}, ctx.Err()
		}
	}
	if releaseCheckpoint != nil {
		select {
		case <-releaseCheckpoint:
		case <-ctx.Done():
			return backend.Checkpoint{}, ctx.Err()
		}
	}
	checkpoint := backend.Checkpoint{Ref: "snapshot-" + name, Kind: kind}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.checkpoints[checkpoint.Ref] = checkpoint
	return checkpoint, nil
}
func (b *testBackend) DeleteCheckpoint(_ context.Context, ref string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.checkpointDeleteFailures > 0 {
		b.checkpointDeleteFailures--
		return errors.New("unconfirmed checkpoint delete")
	}
	if _, ok := b.checkpoints[ref]; !ok {
		return backend.ErrNotFound
	}
	delete(b.checkpoints, ref)
	return nil
}
func (b *testBackend) CreateWorkspace(_ context.Context, in backend.WorkspaceCreateRequest) (backend.Workspace, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.workspaceCreates++
	id := fmt.Sprintf("remote-workspace-%d", b.workspaceCreates)
	value := backend.Workspace{ID: id, Name: in.LocalWorkspaceID}
	b.workspaces[id] = value
	b.workspaceByName[value.Name] = id
	if b.workspaceCreateUnconfirmed {
		return backend.Workspace{}, errors.New("unconfirmed workspace create")
	}
	return value, nil
}
func (b *testBackend) FindWorkspace(_ context.Context, name string) (backend.Workspace, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id, ok := b.workspaceByName[name]
	if !ok {
		return backend.Workspace{}, backend.ErrNotFound
	}
	return b.workspaces[id], nil
}
func (b *testBackend) DeleteWorkspace(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.workspaceDeleteFailures > 0 {
		b.workspaceDeleteFailures--
		return errors.New("unconfirmed workspace delete")
	}
	value, ok := b.workspaces[id]
	if !ok {
		return backend.ErrNotFound
	}
	delete(b.workspaces, id)
	delete(b.workspaceByName, value.Name)
	return nil
}
func (b *testBackend) Run(ctx context.Context, id string, in backend.CommandRequest, emit func(backend.CommandEvent) error) error {
	b.mu.Lock()
	if _, ok := b.sandboxes[id]; !ok {
		b.mu.Unlock()
		return backend.ErrNotFound
	}
	b.lastCommand = in
	output := []byte("hello\n")
	if len(in.Argv) == 3 && in.Argv[0] == "/bin/sh" && strings.HasPrefix(in.Argv[2], "cat ") {
		output = append([]byte(nil), b.readFileLocked(id, strings.TrimPrefix(in.Argv[2], "cat "))...)
	}
	b.mu.Unlock()
	if b.runStarted != nil {
		b.runStartedOnce.Do(func() { close(b.runStarted) })
	}
	if b.releaseRun != nil {
		select {
		case <-b.releaseRun:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, event := range []backend.CommandEvent{{Type: backend.CommandStarted, PID: 12}, {Type: backend.CommandStdout, Data: output}, {Type: backend.CommandExited, Exited: true, ExitCode: 0, Status: "exited"}} {
		if err := emit(event); err != nil {
			return err
		}
	}
	return nil
}
func (b *testBackend) WriteFile(_ context.Context, id, path string, source io.Reader) (backend.FileInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.sandboxes[id]; !ok {
		return backend.FileInfo{}, backend.ErrNotFound
	}
	data, err := io.ReadAll(source)
	if err != nil {
		return backend.FileInfo{}, err
	}
	if key, ok := b.workspaceFileKeyLocked(id, path); ok {
		b.workspaceFiles[key] = data
	} else {
		b.files[id+"\x00"+path] = data
	}
	return backend.FileInfo{Path: path, Size: int64(len(data)), ContentType: "application/octet-stream"}, nil
}
func (b *testBackend) ReadFile(_ context.Context, id, path string, destination io.Writer) (backend.FileInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data := b.readFileLocked(id, path)
	if data == nil {
		return backend.FileInfo{}, backend.ErrNotFound
	}
	if _, err := destination.Write(data); err != nil {
		return backend.FileInfo{}, err
	}
	return backend.FileInfo{Path: path, Size: int64(len(data)), ContentType: "application/octet-stream"}, nil
}
func (b *testBackend) readFileLocked(id, path string) []byte {
	if key, ok := b.workspaceFileKeyLocked(id, path); ok {
		return b.workspaceFiles[key]
	}
	return b.files[id+"\x00"+path]
}
func (b *testBackend) workspaceFileKeyLocked(id, path string) (string, bool) {
	for _, mount := range b.sandboxMounts[id] {
		prefix := strings.TrimRight(mount.Path, "/")
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return mount.Name + "\x00" + strings.TrimPrefix(path, prefix), true
		}
	}
	return "", false
}
func (b *testBackend) ValidatePort(port uint16) error {
	if port == 0 || port == 49983 {
		return errors.New("reserved port")
	}
	return nil
}
func (b *testBackend) RoundTripPort(_ context.Context, id string, port uint16, request *http.Request) (*http.Response, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.sandboxes[id]; !ok {
		return nil, backend.ErrNotFound
	}
	b.lastPort = port
	b.lastPortPath = request.URL.RequestURI()
	body := "preview-ok"
	if data := b.readFileLocked(id, "/workspace/brezel-conformance.txt"); data != nil {
		body = string(data)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/plain"}, "X-Access-Token": []string{"must-not-leak"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

type harness struct {
	server    *httptest.Server
	backend   *testBackend
	service   *service.Service
	state     *store.FileStore
	broker    *connector.Broker
	public    ed25519.PublicKey
	statePath string
}

func newHarness(t *testing.T) harness {
	return newHarnessWithServiceOptions(t)
}

func newHarnessWithServiceOptions(t *testing.T, options ...service.Option) harness {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
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
	be := newTestBackend()
	svc, err := service.New(st, be, signer, options...)
	if err != nil {
		t.Fatal(err)
	}
	api, err := New(svc, testToken)
	if err != nil {
		t.Fatal(err)
	}
	return harness{server: httptest.NewServer(api.Handler()), backend: be, service: svc, state: st, public: public, statePath: statePath}
}

func newBrokerHarness(t *testing.T) harness {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
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
	return harness{server: httptest.NewServer(api.Handler()), backend: be, service: svc, state: st, broker: broker, public: public, statePath: statePath}
}

func (h harness) close() {
	h.server.Close()
	if h.state != nil {
		_ = h.state.Close()
	}
}

type projectAuthorizer struct {
	token   string
	project string
}

func (a projectAuthorizer) Authorize(token, project string) (bool, bool) {
	authenticated := token == a.token
	return authenticated, authenticated && project == a.project
}

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
	resp, out := request(t, h, http.MethodPost, "/v1/environments", project, "environment-0001", map[string]any{"name": "python", "template": "python-313", "image_digest": "sha256:0123456789abcdef"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create environment: %d %#v", resp.StatusCode, out)
	}
	return out["resource"].(map[string]any)["revision_id"].(string)
}

func TestContentIdenticalEnvironmentCreationConvergesAcrossIdempotencyKeys(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	body := map[string]any{"name": "python", "template": "python-313", "image_digest": "sha256:0123456789abcdef"}
	firstResponse, first := request(t, h, http.MethodPost, "/v1/environments", "project-a", "environment-converge-0001", body)
	secondResponse, second := request(t, h, http.MethodPost, "/v1/environments", "project-a", "environment-converge-0002", body)
	if firstResponse.StatusCode != http.StatusCreated || secondResponse.StatusCode != http.StatusCreated {
		t.Fatalf("statuses = %d, %d; bodies = %#v, %#v", firstResponse.StatusCode, secondResponse.StatusCode, first, second)
	}
	firstRevision := first["resource"].(map[string]any)["revision_id"]
	secondRevision := second["resource"].(map[string]any)["revision_id"]
	if firstRevision != secondRevision {
		t.Fatalf("content-identical revisions differ: %v != %v", firstRevision, secondRevision)
	}
	var environmentCount int
	if err := h.state.View(func(state store.State) error {
		environmentCount = len(state.Environments)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if environmentCount != 1 {
		t.Fatalf("environment count = %d, want 1", environmentCount)
	}
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

func TestIndependentSandboxCreatesProvisionConcurrently(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	environment, _, err := h.service.CreateEnvironment("project-a", "parallel-environment-0001", service.CreateEnvironmentInput{
		Name: "parallel", Template: "base",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.backend.createStarted = make(chan struct{}, 2)
	h.backend.releaseCreate = make(chan struct{})

	type result struct {
		sandbox domain.Sandbox
		err     error
	}
	results := make(chan result, 2)
	for index := 0; index < 2; index++ {
		index := index
		go func() {
			sandbox, _, createErr := h.service.CreateSandbox(context.Background(), "project-a", fmt.Sprintf("parallel-sandbox-%04d", index), service.CreateSandboxInput{
				EnvironmentRevision: environment.RevisionID,
				Lifecycle:           domain.Lifecycle{ExpiresAfterSeconds: 600},
				Network:             domain.NetworkPolicy{AllowInternet: false},
			})
			results <- result{sandbox: sandbox, err: createErr}
		}()
	}

	for index := 0; index < 2; index++ {
		select {
		case <-h.backend.createStarted:
		case <-time.After(2 * time.Second):
			t.Fatal("independent sandbox create was serialized behind backend provisioning")
		}
	}
	if err := h.service.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile during active creates: %v", err)
	}
	close(h.backend.releaseCreate)
	seen := map[string]bool{}
	for index := 0; index < 2; index++ {
		select {
		case got := <-results:
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.sandbox.State != domain.SandboxRunning || got.sandbox.BackendID == "" {
				t.Fatalf("sandbox = %#v", got.sandbox)
			}
			seen[got.sandbox.BackendID] = true
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent sandbox create did not finish")
		}
	}
	if len(seen) != 2 {
		t.Fatalf("backend identities = %#v", seen)
	}
}

func TestIndependentSandboxResumesReachBackendConcurrently(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	environment, _, err := h.service.CreateEnvironment("project-a", "parallel-resume-environment-0001", service.CreateEnvironmentInput{
		Name: "parallel-resume", Template: "base",
	})
	if err != nil {
		t.Fatal(err)
	}

	sandboxes := make([]domain.Sandbox, 2)
	for index := range sandboxes {
		sandbox, _, createErr := h.service.CreateSandbox(context.Background(), "project-a", fmt.Sprintf("parallel-resume-create-%04d", index), service.CreateSandboxInput{
			EnvironmentRevision: environment.RevisionID,
			Lifecycle: domain.Lifecycle{
				ExpiresAfterSeconds: 600,
				StandbyCheckpoint:   domain.CheckpointFullState,
			},
			Network: domain.NetworkPolicy{AllowInternet: false},
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		paused, _, pauseErr := h.service.Pause(context.Background(), "project-a", sandbox.ID, fmt.Sprintf("parallel-resume-pause-%04d", index))
		if pauseErr != nil {
			t.Fatal(pauseErr)
		}
		if paused.State != domain.SandboxStandby {
			t.Fatalf("paused sandbox = %#v", paused)
		}
		sandboxes[index] = paused
	}

	h.backend.resumeStarted = make(chan struct{}, len(sandboxes))
	h.backend.releaseResume = make(chan struct{})
	type result struct {
		sandbox domain.Sandbox
		err     error
	}
	results := make(chan result, len(sandboxes))
	for index, sandbox := range sandboxes {
		index, sandbox := index, sandbox
		go func() {
			resumed, _, resumeErr := h.service.Resume(context.Background(), "project-a", sandbox.ID, fmt.Sprintf("parallel-resume-%04d", index))
			results <- result{sandbox: resumed, err: resumeErr}
		}()
	}

	for range sandboxes {
		select {
		case <-h.backend.resumeStarted:
		case <-time.After(2 * time.Second):
			t.Fatal("independent sandbox resume was serialized behind another backend resume")
		}
	}
	if err := h.service.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile during active resumes: %v", err)
	}
	close(h.backend.releaseResume)
	for range sandboxes {
		select {
		case got := <-results:
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.sandbox.State != domain.SandboxRunning {
				t.Fatalf("resumed sandbox = %#v", got.sandbox)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent sandbox resumes did not finish")
		}
	}
}

func TestAutomaticStandbyUsesDurableIdleAndGraceDeadline(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	h := newHarnessWithServiceOptions(t, service.WithClock(func() time.Time { return now }))
	defer h.close()
	environment, _, err := h.service.CreateEnvironment("project-a", "automatic-standby-environment-0001", service.CreateEnvironmentInput{Name: "automatic-standby", Template: "base"})
	if err != nil {
		t.Fatal(err)
	}
	sandbox, _, err := h.service.CreateSandbox(context.Background(), "project-a", "automatic-standby-sandbox-0001", service.CreateSandboxInput{
		EnvironmentRevision: environment.RevisionID,
		Lifecycle: domain.Lifecycle{
			StandbyAfterSeconds: 30,
			ExpiresAfterSeconds: 3600,
			StandbyCheckpoint:   domain.CheckpointFullState,
		},
		Network: domain.NetworkPolicy{AllowInternet: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := sandbox.ExpiresAt
	if sandbox.Lifecycle.StandbyGraceSeconds != service.DefaultStandbyGraceSeconds {
		t.Fatalf("standby grace = %d, want %d", sandbox.Lifecycle.StandbyGraceSeconds, service.DefaultStandbyGraceSeconds)
	}
	if !sandbox.LastActiveAt.Equal(now) || !sandbox.StandbyEligibleAt.Equal(now.Add(45*time.Second)) {
		t.Fatalf("activity window = %s -> %s", sandbox.LastActiveAt, sandbox.StandbyEligibleAt)
	}

	now = now.Add(44 * time.Second)
	if err := h.service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	beforeGrace, err := h.service.GetSandbox(context.Background(), "project-a", sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if beforeGrace.State != domain.SandboxRunning || h.backend.pauseCalls != 0 {
		t.Fatalf("sandbox paused before grace elapsed: state=%s calls=%d", beforeGrace.State, h.backend.pauseCalls)
	}

	now = now.Add(2 * time.Second)
	if err := h.service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	standby, err := h.service.GetSandbox(context.Background(), "project-a", sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if standby.State != domain.SandboxStandby || h.backend.pauseCalls != 1 {
		t.Fatalf("automatic standby = state %s, pause calls %d", standby.State, h.backend.pauseCalls)
	}
	if !standby.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("automatic standby changed expiration: got %s want %s", standby.ExpiresAt, expiresAt)
	}
	var automaticOperation domain.Operation
	if err := h.state.View(func(state store.State) error {
		for _, operation := range state.Operations {
			if operation.ResourceID == sandbox.ID && strings.HasPrefix(operation.Kind, "auto_pause_sandbox:") {
				automaticOperation = operation
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if automaticOperation.State != domain.OperationSucceeded {
		t.Fatalf("automatic standby operation = %#v", automaticOperation)
	}
}

func TestAutomaticStandbyDoesNotPauseActiveGuestStream(t *testing.T) {
	now := time.Date(2026, 9, 14, 13, 0, 0, 0, time.UTC)
	h := newHarnessWithServiceOptions(t, service.WithClock(func() time.Time { return now }))
	defer h.close()
	environment, _, err := h.service.CreateEnvironment("project-a", "active-standby-environment-0001", service.CreateEnvironmentInput{Name: "active-standby", Template: "base"})
	if err != nil {
		t.Fatal(err)
	}
	sandbox, _, err := h.service.CreateSandbox(context.Background(), "project-a", "active-standby-sandbox-0001", service.CreateSandboxInput{
		EnvironmentRevision: environment.RevisionID,
		Lifecycle:           domain.Lifecycle{StandbyAfterSeconds: 1, StandbyGraceSeconds: 1, ExpiresAfterSeconds: 600, StandbyCheckpoint: domain.CheckpointFullState},
		Network:             domain.NetworkPolicy{AllowInternet: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Second)
	h.backend.runStarted = make(chan struct{}, 1)
	h.backend.releaseRun = make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, runErr := h.service.RunCommand(context.Background(), "project-a", sandbox.ID, service.RunCommandInput{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil })
		finished <- runErr
	}()
	select {
	case <-h.backend.runStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("guest stream did not reach backend")
	}
	if err := h.service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.backend.pauseCalls != 0 {
		t.Fatalf("automatic standby paused an active stream %d times", h.backend.pauseCalls)
	}
	close(h.backend.releaseRun)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	after, err := h.service.GetSandbox(context.Background(), "project-a", sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != domain.SandboxRunning || !after.LastActiveAt.Equal(now) || !after.ExpiresAt.Equal(sandbox.ExpiresAt) {
		t.Fatalf("sandbox after guest stream = %#v", after)
	}
}

func TestGuestAdmissionPersistsActivityBeforeBackendEntry(t *testing.T) {
	now := time.Date(2026, 9, 14, 13, 30, 0, 0, time.UTC)
	h := newHarnessWithServiceOptions(t, service.WithClock(func() time.Time { return now }))
	defer h.close()
	environment, _, err := h.service.CreateEnvironment("project-a", "durable-activity-environment-0001", service.CreateEnvironmentInput{Name: "durable-activity", Template: "base"})
	if err != nil {
		t.Fatal(err)
	}
	sandbox, _, err := h.service.CreateSandbox(context.Background(), "project-a", "durable-activity-sandbox-0001", service.CreateSandboxInput{
		EnvironmentRevision: environment.RevisionID,
		Lifecycle:           domain.Lifecycle{StandbyAfterSeconds: 1, StandbyGraceSeconds: 1, ExpiresAfterSeconds: 600, StandbyCheckpoint: domain.CheckpointFullState},
		Network:             domain.NetworkPolicy{AllowInternet: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Second)
	h.backend.runStarted = make(chan struct{}, 1)
	h.backend.releaseRun = make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, runErr := h.service.RunCommand(context.Background(), "project-a", sandbox.ID, service.RunCommandInput{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil })
		finished <- runErr
	}()
	select {
	case <-h.backend.runStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("guest stream did not reach backend")
	}
	var current domain.Sandbox
	if err := h.state.View(func(state store.State) error {
		current = state.Sandboxes[store.ScopedKey("project-a", sandbox.ID)]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !current.LastActiveAt.Equal(now) || !current.StandbyEligibleAt.Equal(now.Add(2*time.Second)) || current.Revision <= sandbox.Revision {
		t.Fatalf("activity was not durable before backend entry: %#v", current)
	}
	close(h.backend.releaseRun)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticStandbyReleasesServiceLockDuringBackendPause(t *testing.T) {
	now := time.Date(2026, 9, 14, 14, 0, 0, 0, time.UTC)
	h := newHarnessWithServiceOptions(t, service.WithClock(func() time.Time { return now }))
	defer h.close()
	environment, _, err := h.service.CreateEnvironment("project-a", "pause-lock-environment-0001", service.CreateEnvironmentInput{Name: "pause-lock", Template: "base"})
	if err != nil {
		t.Fatal(err)
	}
	var sandboxes []domain.Sandbox
	for index := 0; index < 2; index++ {
		sandbox, _, createErr := h.service.CreateSandbox(context.Background(), "project-a", fmt.Sprintf("pause-lock-sandbox-%04d", index), service.CreateSandboxInput{
			EnvironmentRevision: environment.RevisionID,
			Lifecycle:           domain.Lifecycle{StandbyAfterSeconds: 1, StandbyGraceSeconds: 1, ExpiresAfterSeconds: 600, StandbyCheckpoint: domain.CheckpointFullState},
			Network:             domain.NetworkPolicy{AllowInternet: false},
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		sandboxes = append(sandboxes, sandbox)
	}
	now = now.Add(3 * time.Second)
	h.backend.pauseStarted = make(chan string, 1)
	h.backend.releasePause = make(chan struct{})
	reconciled := make(chan error, 1)
	go func() { reconciled <- h.service.Reconcile(context.Background()) }()
	var pausingBackendID string
	select {
	case pausingBackendID = <-h.backend.pauseStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("automatic pause did not reach backend")
	}
	independent := sandboxes[0]
	if independent.BackendID == pausingBackendID {
		independent = sandboxes[1]
	}
	if _, err := h.service.RunCommand(context.Background(), "project-a", independent.ID, service.RunCommandInput{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil }); err != nil {
		t.Fatalf("independent guest operation was blocked by backend pause: %v", err)
	}
	close(h.backend.releasePause)
	select {
	case err := <-reconciled:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("automatic standby did not complete")
	}
}

func TestReconcileUnchangedObservationDoesNotChurnRevisionOrEvents(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	environment, _, err := h.service.CreateEnvironment("project-a", "reconcile-noop-environment-0001", service.CreateEnvironmentInput{Name: "reconcile-noop", Template: "base"})
	if err != nil {
		t.Fatal(err)
	}
	sandbox, _, err := h.service.CreateSandbox(context.Background(), "project-a", "reconcile-noop-sandbox-0001", service.CreateSandboxInput{
		EnvironmentRevision: environment.RevisionID,
		Lifecycle:           domain.Lifecycle{ExpiresAfterSeconds: 600},
		Network:             domain.NetworkPolicy{AllowInternet: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := h.service.Events("project-a", sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := h.service.GetSandbox(context.Background(), "project-a", sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterEvents, err := h.service.Events("project-a", sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != sandbox.Revision || !after.UpdatedAt.Equal(sandbox.UpdatedAt) {
		t.Fatalf("no-op observation mutated sandbox: before revision=%d updated=%s, after revision=%d updated=%s", sandbox.Revision, sandbox.UpdatedAt, after.Revision, after.UpdatedAt)
	}
	if len(afterEvents) != len(beforeEvents) {
		t.Fatalf("no-op observation appended events: before=%d after=%d", len(beforeEvents), len(afterEvents))
	}
}

func TestReconcileBackendObservationDoesNotHoldServiceLock(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	environment, _, err := h.service.CreateEnvironment("project-a", "reconcile-lock-environment-0001", service.CreateEnvironmentInput{Name: "reconcile-lock", Template: "base"})
	if err != nil {
		t.Fatal(err)
	}
	sandboxes := make([]domain.Sandbox, 2)
	for index := range sandboxes {
		sandbox, _, createErr := h.service.CreateSandbox(context.Background(), "project-a", fmt.Sprintf("reconcile-lock-sandbox-%04d", index), service.CreateSandboxInput{
			EnvironmentRevision: environment.RevisionID,
			Lifecycle:           domain.Lifecycle{ExpiresAfterSeconds: 600},
			Network:             domain.NetworkPolicy{AllowInternet: false},
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		sandboxes[index] = sandbox
	}
	h.backend.inspectStarted = make(chan string, 1)
	h.backend.releaseInspect = make(chan struct{})
	h.backend.runStarted = make(chan struct{})
	reconciled := make(chan error, 1)
	go func() { reconciled <- h.service.Reconcile(context.Background()) }()
	var observedBackendID string
	select {
	case observedBackendID = <-h.backend.inspectStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile did not reach backend inspection")
	}
	independent := sandboxes[0]
	if independent.BackendID == observedBackendID {
		independent = sandboxes[1]
	}
	runResult := make(chan error, 1)
	go func() {
		_, runErr := h.service.RunCommand(context.Background(), "project-a", independent.ID, service.RunCommandInput{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil })
		runResult <- runErr
	}()
	select {
	case <-h.backend.runStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("independent guest operation waited behind backend inspection")
	}
	select {
	case err := <-runResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("independent guest operation did not finish while inspection was blocked")
	}
	close(h.backend.releaseInspect)
	select {
	case err := <-reconciled:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile did not finish")
	}
}

func TestReconcileDiscardsObservationAfterGuestRevisionAdvances(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	environment, _, err := h.service.CreateEnvironment("project-a", "reconcile-stale-environment-0001", service.CreateEnvironmentInput{Name: "reconcile-stale", Template: "base"})
	if err != nil {
		t.Fatal(err)
	}
	sandbox, _, err := h.service.CreateSandbox(context.Background(), "project-a", "reconcile-stale-sandbox-0001", service.CreateSandboxInput{
		EnvironmentRevision: environment.RevisionID,
		Lifecycle:           domain.Lifecycle{StandbyAfterSeconds: 60, StandbyGraceSeconds: 15, ExpiresAfterSeconds: 600},
		Network:             domain.NetworkPolicy{AllowInternet: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.backend.inspectStarted = make(chan string, 1)
	h.backend.releaseInspect = make(chan struct{})
	h.backend.runStarted = make(chan struct{})
	reconciled := make(chan error, 1)
	go func() { reconciled <- h.service.Reconcile(context.Background()) }()
	select {
	case <-h.backend.inspectStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile did not reach backend inspection")
	}
	if _, err := h.service.RunCommand(context.Background(), "project-a", sandbox.ID, service.RunCommandInput{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	afterGuest, err := h.state.GetSandbox("project-a", sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterGuest.Revision <= sandbox.Revision {
		t.Fatalf("guest activity did not advance revision: before=%d after=%d", sandbox.Revision, afterGuest.Revision)
	}
	close(h.backend.releaseInspect)
	select {
	case err := <-reconciled:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile did not finish")
	}
	afterReconcile, err := h.state.GetSandbox("project-a", sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterReconcile.Revision != afterGuest.Revision || !afterReconcile.UpdatedAt.Equal(afterGuest.UpdatedAt) {
		t.Fatalf("stale observation mutated sandbox: after guest=%#v after reconcile=%#v", afterGuest, afterReconcile)
	}
	events, err := h.service.Events("project-a", sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == "sandbox.reconciled" {
			t.Fatal("stale observation appended a reconciliation event")
		}
	}
}

func TestAutomaticStandbyInitializesLegacyActivityInsteadOfImmediatePause(t *testing.T) {
	now := time.Date(2026, 9, 14, 15, 0, 0, 0, time.UTC)
	h := newHarnessWithServiceOptions(t, service.WithClock(func() time.Time { return now }))
	defer h.close()
	environment, _, err := h.service.CreateEnvironment("project-a", "legacy-activity-environment-0001", service.CreateEnvironmentInput{Name: "legacy-activity", Template: "base"})
	if err != nil {
		t.Fatal(err)
	}
	sandbox, _, err := h.service.CreateSandbox(context.Background(), "project-a", "legacy-activity-sandbox-0001", service.CreateSandboxInput{
		EnvironmentRevision: environment.RevisionID,
		Lifecycle:           domain.Lifecycle{StandbyAfterSeconds: 30, StandbyGraceSeconds: 15, ExpiresAfterSeconds: 600, StandbyCheckpoint: domain.CheckpointFullState},
		Network:             domain.NetworkPolicy{AllowInternet: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.state.Update(func(state *store.State) error {
		current := state.Sandboxes[store.ScopedKey("project-a", sandbox.ID)]
		current.LastActiveAt = time.Time{}
		current.StandbyEligibleAt = time.Time{}
		state.Sandboxes[store.ScopedKey("project-a", sandbox.ID)] = current
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	restarted, err := service.New(h.state, h.backend, nil, service.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := restarted.GetSandbox(context.Background(), "project-a", sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != domain.SandboxRunning || !after.LastActiveAt.Equal(now) || h.backend.pauseCalls != 0 {
		t.Fatalf("legacy sandbox = %#v, pause calls=%d", after, h.backend.pauseCalls)
	}
}

func TestReconcileCompletesInterruptedAutomaticStandby(t *testing.T) {
	now := time.Date(2026, 9, 14, 16, 0, 0, 0, time.UTC)
	h := newHarnessWithServiceOptions(t, service.WithClock(func() time.Time { return now }))
	defer h.close()
	environment, _, err := h.service.CreateEnvironment("project-a", "restart-standby-environment-0001", service.CreateEnvironmentInput{Name: "restart-standby", Template: "base"})
	if err != nil {
		t.Fatal(err)
	}
	sandbox, _, err := h.service.CreateSandbox(context.Background(), "project-a", "restart-standby-sandbox-0001", service.CreateSandboxInput{
		EnvironmentRevision: environment.RevisionID,
		Lifecycle:           domain.Lifecycle{StandbyAfterSeconds: 30, StandbyGraceSeconds: 15, ExpiresAfterSeconds: 600, StandbyCheckpoint: domain.CheckpointFullState},
		Network:             domain.NetworkPolicy{AllowInternet: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	op := domain.Operation{ID: "op_interrupted_auto_pause", ProjectID: "project-a", Kind: "auto_pause_sandbox:" + sandbox.ID, ResourceID: sandbox.ID, State: domain.OperationRunning, CreatedAt: now, UpdatedAt: now}
	if err := h.state.Update(func(state *store.State) error {
		current := state.Sandboxes[store.ScopedKey("project-a", sandbox.ID)]
		current.State = domain.SandboxPausing
		current.Revision++
		state.Sandboxes[store.ScopedKey("project-a", sandbox.ID)] = current
		state.Operations[store.ScopedKey("project-a", op.ID)] = op
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.backend.mu.Lock()
	remote := h.backend.sandboxes[sandbox.BackendID]
	remote.State = domain.SandboxStandby
	h.backend.sandboxes[sandbox.BackendID] = remote
	h.backend.mu.Unlock()
	restarted, err := service.New(h.state, h.backend, nil, service.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := restarted.GetSandbox(context.Background(), "project-a", sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	reconciledOperation, err := restarted.GetOperation("project-a", op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != domain.SandboxStandby || reconciledOperation.State != domain.OperationSucceeded {
		t.Fatalf("restart recovery sandbox=%s operation=%s", after.State, reconciledOperation.State)
	}
}

func TestIndependentCheckpointsReachBackendConcurrentlyAndFenceGuestIO(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	environment, _, err := h.service.CreateEnvironment("project-a", "parallel-checkpoint-environment-0001", service.CreateEnvironmentInput{
		Name: "parallel-checkpoint", Template: "base",
	})
	if err != nil {
		t.Fatal(err)
	}

	sandboxes := make([]domain.Sandbox, 2)
	for index := range sandboxes {
		sandbox, _, createErr := h.service.CreateSandbox(context.Background(), "project-a", fmt.Sprintf("parallel-checkpoint-create-%04d", index), service.CreateSandboxInput{
			EnvironmentRevision: environment.RevisionID,
			Lifecycle:           domain.Lifecycle{ExpiresAfterSeconds: 600},
			Network:             domain.NetworkPolicy{AllowInternet: false},
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		sandboxes[index] = sandbox
	}

	h.backend.checkpointStarted = make(chan struct{}, len(sandboxes))
	h.backend.releaseCheckpoint = make(chan struct{})
	type result struct {
		checkpoint domain.Checkpoint
		err        error
	}
	results := make(chan result, len(sandboxes))
	for index, sandbox := range sandboxes {
		index, sandbox := index, sandbox
		go func() {
			checkpoint, _, checkpointErr := h.service.Checkpoint(context.Background(), "project-a", sandbox.ID, fmt.Sprintf("parallel-checkpoint-%04d", index), fmt.Sprintf("checkpoint-%04d", index), domain.CheckpointFilesystem)
			results <- result{checkpoint: checkpoint, err: checkpointErr}
		}()
	}

	for range sandboxes {
		select {
		case <-h.backend.checkpointStarted:
		case <-time.After(2 * time.Second):
			t.Fatal("independent checkpoint was serialized behind another backend checkpoint")
		}
	}
	if _, err := h.service.RunCommand(context.Background(), "project-a", sandboxes[0].ID, service.RunCommandInput{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil }); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("guest command during checkpoint error=%v", err)
	}
	if _, _, err := h.service.Pause(context.Background(), "project-a", sandboxes[0].ID, "parallel-checkpoint-pause-0001"); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("pause during checkpoint error=%v", err)
	}
	close(h.backend.releaseCheckpoint)
	for range sandboxes {
		select {
		case got := <-results:
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.checkpoint.ID == "" || got.checkpoint.BackendRef == "" {
				t.Fatalf("checkpoint = %#v", got.checkpoint)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent checkpoint did not finish")
		}
	}
}

func TestLifecycleIdempotencyTenantIsolationAndReceipt(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	envRevision := createEnvironment(t, h, "project-a")
	sandboxID, first := createSandbox(t, h, "project-a", envRevision, "sandbox-create-0001", nil)
	_, second := createSandbox(t, h, "project-a", envRevision, "sandbox-create-0001", nil)
	publicPayload, _ := json.Marshal(first)
	for _, forbidden := range []string{"remote-1", "backend_id", "sandbox-create-0001"} {
		if bytes.Contains(publicPayload, []byte(forbidden)) {
			t.Fatalf("public sandbox response leaked internal value %q", forbidden)
		}
	}
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
	resp, checkpointOut := request(t, h, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/checkpoints", "project-a", "checkpoint-0001", map[string]any{"name": "after-clone", "kind": "filesystem"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("checkpoint returned %d", resp.StatusCode)
	}
	checkpointID := checkpointOut["resource"].(map[string]any)["id"].(string)
	forkBody := map[string]any{
		"checkpoint_id": checkpointID,
		"lifecycle":     map[string]any{"expires_after_seconds": 3600},
		"network":       map[string]any{"allow_internet": false},
	}
	resp, forkOut := request(t, h, http.MethodPost, "/v1/sandboxes", "project-a", "sandbox-fork-0001", forkBody)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("fork returned %d %#v", resp.StatusCode, forkOut)
	}
	fork := forkOut["resource"].(map[string]any)
	if fork["source_checkpoint_id"] != checkpointID {
		t.Fatalf("fork lineage = %#v", fork["source_checkpoint_id"])
	}
	h.backend.mu.Lock()
	restoreTemplate := h.backend.lastCreate.TemplateID
	h.backend.mu.Unlock()
	if restoreTemplate != "snapshot-after-clone" {
		t.Fatalf("fork template = %q", restoreTemplate)
	}

	resp, listOut := request(t, h, http.MethodGet, "/v1/sandboxes", "project-a", "", nil)
	if resp.StatusCode != http.StatusOK || len(listOut["sandboxes"].([]any)) != 2 {
		t.Fatalf("project list = %d %#v", resp.StatusCode, listOut)
	}
	resp, listOut = request(t, h, http.MethodGet, "/v1/sandboxes", "project-b", "", nil)
	if resp.StatusCode != http.StatusOK || len(listOut["sandboxes"].([]any)) != 0 {
		t.Fatalf("cross-project list = %d %#v", resp.StatusCode, listOut)
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

func TestAuthenticatedPortLeaseProxiesWithoutServiceCredential(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	environment := createEnvironment(t, h, "project-a")
	sandboxID, _ := createSandbox(t, h, "project-a", environment, "sandbox-port-0001", nil)
	response, payload := request(t, h, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/ports/8080/leases", "project-a", "", map[string]any{"ttl_seconds": 60})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create lease: %d %#v", response.StatusCode, payload)
	}
	leasePath := payload["path"].(string)
	previewRequest, _ := http.NewRequest(http.MethodGet, h.server.URL+leasePath+"health?ready=1", nil)
	previewResponse, err := h.server.Client().Do(previewRequest)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(previewResponse.Body)
	previewResponse.Body.Close()
	if previewResponse.StatusCode != http.StatusOK || string(body) != "preview-ok" {
		t.Fatalf("preview = %d %q", previewResponse.StatusCode, body)
	}
	if previewResponse.Header.Get("X-Access-Token") != "" {
		t.Fatal("internal access token leaked through preview response")
	}
	if h.backend.lastPort != 8080 || h.backend.lastPortPath != "/health?ready=1" {
		t.Fatalf("port request = %d %q", h.backend.lastPort, h.backend.lastPortPath)
	}

	websocketRequest, _ := http.NewRequest(http.MethodGet, h.server.URL+leasePath, nil)
	websocketRequest.Header.Set("Upgrade", "websocket")
	websocketResponse, err := h.server.Client().Do(websocketRequest)
	if err != nil {
		t.Fatal(err)
	}
	websocketResponse.Body.Close()
	if websocketResponse.StatusCode != http.StatusNotImplemented {
		t.Fatalf("WebSocket preview returned %d", websocketResponse.StatusCode)
	}
}

func TestLifecycleMutationRejectsActiveGuestOperation(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	environment := createEnvironment(t, h, "project-a")
	sandboxID, _ := createSandbox(t, h, "project-a", environment, "sandbox-active-0001", nil)
	h.backend.runStarted = make(chan struct{})
	h.backend.releaseRun = make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := h.service.RunCommand(context.Background(), "project-a", sandboxID, service.RunCommandInput{Argv: []string{"sleep", "1"}}, func(backend.CommandEvent) error { return nil })
		finished <- err
	}()
	select {
	case <-h.backend.runStarted:
	case <-time.After(time.Second):
		t.Fatal("command did not start")
	}
	if _, _, err := h.service.Pause(context.Background(), "project-a", sandboxID, "pause-active-0001"); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("pause during active command = %v", err)
	}
	close(h.backend.releaseRun)
	if err := <-finished; err != nil {
		t.Fatal(err)
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
	lease := h.backend.lastCreate.Environment["BREZEL_CONNECTOR_LEASE"]
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

func TestConfirmedBackendCapacityFailureIsNotRecordedAsUnknown(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	h.backend.createError = backend.ErrCapacityUnavailable
	envRevision := createEnvironment(t, h, "project-a")
	body := map[string]any{"environment_revision": envRevision, "lifecycle": map[string]any{"expires_after_seconds": 3600}, "network": map[string]any{"allow_internet": false}}
	resp, out := request(t, h, http.MethodPost, "/v1/sandboxes", "project-a", "capacity-failure-0001", body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("capacity response = %d %#v", resp.StatusCode, out)
	}
	var persisted domain.Sandbox
	if err := h.state.View(func(state store.State) error {
		for _, candidate := range state.Sandboxes {
			persisted = candidate
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if persisted.State != domain.SandboxFailed || persisted.Failure == nil || persisted.Failure.Code != "backend_capacity_unavailable" {
		t.Fatalf("persisted capacity failure = %#v", persisted)
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
	resp, _ = request(t, h, http.MethodPost, "/v1/environments", "project-a", "environment-0002", map[string]any{"name": "x", "template": "x", "secret": "should-not-be-accepted"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field status %d", resp.StatusCode)
	}
	environment := createEnvironment(t, h, "project-a")
	body := map[string]any{
		"environment_revision": environment,
		"checkpoint_id":        "chk_not-allowed-together",
		"lifecycle":            map[string]any{"expires_after_seconds": 3600},
		"network":              map[string]any{"allow_internet": false},
	}
	resp, _ = request(t, h, http.MethodPost, "/v1/sandboxes", "project-a", "sandbox-invalid-source", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("ambiguous sandbox source status %d", resp.StatusCode)
	}
}

func TestProjectBoundCredentialCannotSelectAnotherTenant(t *testing.T) {
	h := newHarness(t)
	h.server.Close()
	api, err := New(h.service, "", WithAuthorizer(projectAuthorizer{token: testToken, project: "project-a"}))
	if err != nil {
		t.Fatal(err)
	}
	h.server = httptest.NewServer(api.Handler())
	defer h.close()

	resp, _ := request(t, h, http.MethodGet, "/v1/capabilities", "project-a", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bound project returned %d", resp.StatusCode)
	}
	resp, body := request(t, h, http.MethodGet, "/v1/capabilities", "project-b", "", nil)
	if resp.StatusCode != http.StatusForbidden || errorCode(body) != "forbidden" {
		t.Fatalf("unbound project returned %d %#v", resp.StatusCode, body)
	}
}

func TestReadinessFailsWhenBackendIsUnavailable(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	h.backend.readyErr = errors.New("engine unavailable")
	response, err := h.server.Client().Get(h.server.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readiness status = %d", response.StatusCode)
	}
}

type readinessRouteAdmin struct {
	service.NodeRouteAdministrator
	err error
}

func (a readinessRouteAdmin) Ready(context.Context) error { return a.err }

func TestReadinessFailsWhenNodeControlListenerIsUnavailable(t *testing.T) {
	h := newHarnessWithServiceOptions(t, service.WithNodeRouteAdministrator(readinessRouteAdmin{err: errors.New("control listener unavailable")}))
	defer h.close()
	response, err := h.server.Client().Get(h.server.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readiness status = %d", response.StatusCode)
	}
}

func TestProjectResourceQuotasFailBeforeBackendMutation(t *testing.T) {
	h := newHarnessWithServiceOptions(t, service.WithLimits(service.Limits{
		MaxActiveSandboxesPerProject:    1,
		MaxActiveSandboxesTotal:         4,
		MaxWorkspacesPerProject:         1,
		MaxConcurrentGuestOpsPerProject: 1,
		MaxEnvironmentsPerProject:       4,
		MaxConnectorsPerProject:         4,
	}))
	defer h.close()
	environment := createEnvironment(t, h, "project-a")
	createSandbox(t, h, "project-a", environment, "quota-sandbox-one", nil)
	before := h.backend.createCalls
	body := map[string]any{
		"environment_revision": environment,
		"lifecycle":            map[string]any{"expires_after_seconds": 3600},
		"network":              map[string]any{"allow_internet": false},
	}
	response, result := request(t, h, http.MethodPost, "/v1/sandboxes", "project-a", "quota-sandbox-two", body)
	if response.StatusCode != http.StatusTooManyRequests || errorCode(result) != "quota_exceeded" {
		t.Fatalf("sandbox quota returned %d %#v", response.StatusCode, result)
	}
	if h.backend.createCalls != before {
		t.Fatal("sandbox backend was called after quota rejection")
	}
	response, _ = request(t, h, http.MethodPost, "/v1/workspaces", "project-a", "quota-workspace-one", map[string]any{"name": "one"})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("first workspace returned %d", response.StatusCode)
	}
	beforeWorkspaces := h.backend.workspaceCreates
	response, result = request(t, h, http.MethodPost, "/v1/workspaces", "project-a", "quota-workspace-two", map[string]any{"name": "two"})
	if response.StatusCode != http.StatusTooManyRequests || errorCode(result) != "quota_exceeded" {
		t.Fatalf("workspace quota returned %d %#v", response.StatusCode, result)
	}
	if h.backend.workspaceCreates != beforeWorkspaces {
		t.Fatal("workspace backend was called after quota rejection")
	}
}

func TestGlobalSandboxCapacityFailsBeforeBackendMutationAcrossProjects(t *testing.T) {
	h := newHarnessWithServiceOptions(t, service.WithLimits(service.Limits{
		MaxActiveSandboxesPerProject:    4,
		MaxActiveSandboxesTotal:         1,
		MaxWorkspacesPerProject:         4,
		MaxConcurrentGuestOpsPerProject: 4,
		MaxEnvironmentsPerProject:       4,
		MaxConnectorsPerProject:         4,
	}))
	defer h.close()
	firstEnvironment := createEnvironment(t, h, "project-a")
	createSandbox(t, h, "project-a", firstEnvironment, "capacity-sandbox-one", nil)
	secondEnvironment := createEnvironment(t, h, "project-b")
	before := h.backend.createCalls
	body := map[string]any{
		"environment_revision": secondEnvironment,
		"lifecycle":            map[string]any{"expires_after_seconds": 3600},
		"network":              map[string]any{"allow_internet": false},
	}
	response, result := request(t, h, http.MethodPost, "/v1/sandboxes", "project-b", "capacity-sandbox-two", body)
	if response.StatusCode != http.StatusTooManyRequests || errorCode(result) != "capacity_exhausted" {
		t.Fatalf("global capacity returned %d %#v", response.StatusCode, result)
	}
	if response.Header.Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q", response.Header.Get("Retry-After"))
	}
	if h.backend.createCalls != before {
		t.Fatal("sandbox backend was called after global capacity rejection")
	}
}

func TestGuestOperationQuotaReleasesProjectSlot(t *testing.T) {
	h := newHarnessWithServiceOptions(t, service.WithLimits(service.Limits{
		MaxActiveSandboxesPerProject:    4,
		MaxActiveSandboxesTotal:         8,
		MaxWorkspacesPerProject:         4,
		MaxConcurrentGuestOpsPerProject: 1,
		MaxEnvironmentsPerProject:       4,
		MaxConnectorsPerProject:         4,
	}))
	defer h.close()
	environment := createEnvironment(t, h, "project-a")
	firstSandbox, _ := createSandbox(t, h, "project-a", environment, "guest-quota-first", nil)
	secondSandbox, _ := createSandbox(t, h, "project-a", environment, "guest-quota-second", nil)
	h.backend.runStarted = make(chan struct{})
	h.backend.releaseRun = make(chan struct{})

	commandBody, err := json.Marshal(service.RunCommandInput{Argv: []string{"true"}, TimeoutSeconds: 30})
	if err != nil {
		t.Fatal(err)
	}
	run := func(sandboxID string) (*http.Response, error) {
		request, requestErr := http.NewRequest(http.MethodPost, h.server.URL+"/v1/sandboxes/"+sandboxID+"/commands", bytes.NewReader(commandBody))
		if requestErr != nil {
			return nil, requestErr
		}
		request.Header.Set("Authorization", "Bearer "+testToken)
		request.Header.Set("X-Project-ID", "project-a")
		request.Header.Set("Content-Type", "application/json")
		return h.server.Client().Do(request)
	}

	firstResult := make(chan error, 1)
	go func() {
		response, runErr := run(firstSandbox)
		if runErr == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				runErr = fmt.Errorf("first command returned %d", response.StatusCode)
			}
		}
		firstResult <- runErr
	}()

	select {
	case <-h.backend.runStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first guest operation did not reach the backend")
	}
	response, err := run(secondSandbox)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("concurrent guest operation returned %d", response.StatusCode)
	}

	close(h.backend.releaseRun)
	select {
	case err := <-firstResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first guest operation did not finish")
	}

	response, err = run(secondSandbox)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("guest operation after release returned %d", response.StatusCode)
	}
}

func TestCommandStreamMakesSuccessfulExitCodeExplicit(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	environment := createEnvironment(t, h, "project-a")
	sandboxID, _ := createSandbox(t, h, "project-a", environment, "command-exit-code-0001", nil)
	body := strings.NewReader(`{"argv":["/bin/true"],"timeout_seconds":30}`)
	request, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/sandboxes/"+sandboxID+"/commands", body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("X-Project-ID", "project-a")
	request.Header.Set("Content-Type", "application/json")
	response, err := h.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("command returned %d", response.StatusCode)
	}
	decoder := json.NewDecoder(response.Body)
	foundExit := false
	for decoder.More() {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["type"] != string(backend.CommandExited) {
			if _, exists := event["exit_code"]; exists {
				t.Fatalf("non-terminal command event included exit_code: %#v", event)
			}
			continue
		}
		foundExit = true
		value, exists := event["exit_code"]
		if !exists || value != float64(0) {
			t.Fatalf("successful terminal event did not include exit_code 0: %#v", event)
		}
	}
	if !foundExit {
		t.Fatal("command stream did not include a terminal event")
	}
}

func TestCommandDiagnosticsCaptureOnlyPublicStreamDurationsAndCounts(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	environment := createEnvironment(t, h, "project-private-diagnostic")
	sandboxID, _ := createSandbox(t, h, "project-private-diagnostic", environment, "command-diagnostic-0001", nil)
	collector := &apiCommandDiagnosticCollector{}
	api, err := New(h.service, testToken, WithCommandDiagnostics(collector))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.Handler())
	defer server.Close()

	body := strings.NewReader(`{"argv":["/bin/sh","-c","printf private-command"],"timeout_seconds":30}`)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/sandboxes/"+sandboxID+"/commands", body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("X-Project-ID", "project-private-diagnostic")
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("command status=%d read error=%v", response.StatusCode, readErr)
	}
	if len(collector.samples) != 1 {
		t.Fatalf("diagnostic samples=%#v", collector.samples)
	}
	sample := collector.samples[0]
	if sample.Component != telemetry.CommandDiagnosticPublicHandler || sample.Outcome != telemetry.OutcomeSuccess || sample.StreamEvents != 3 || sample.StreamBytes != 6 || sample.FirstEventCount != 1 || sample.TerminalEventCount != 1 {
		t.Fatalf("public diagnostic=%#v", sample)
	}
	if sample.AcceptedToFirstEvent < 0 || sample.AcceptedToTerminal < sample.AcceptedToFirstEvent || sample.AcceptedToEOFReady < sample.AcceptedToTerminal {
		t.Fatalf("public diagnostic is not monotonic: %#v", sample)
	}
}

type apiCommandDiagnosticCollector struct {
	samples []telemetry.CommandDiagnostic
}

func (c *apiCommandDiagnosticCollector) ObserveCommandDiagnostic(sample telemetry.CommandDiagnostic) {
	c.samples = append(c.samples, sample)
}

func errorCode(body map[string]any) string {
	value, _ := body["error"].(map[string]any)
	code, _ := value["code"].(string)
	return code
}

func TestCommandAndFileDataPathIsTenantScopedAndContentEphemeral(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	environment := createEnvironment(t, h, "project-a")
	sandboxID, _ := createSandbox(t, h, "project-a", environment, "sandbox-data-path-0001", nil)

	commandBody, _ := json.Marshal(service.RunCommandInput{Argv: []string{"printf", "secret-command"}, Cwd: "/workspace", TimeoutSeconds: 30})
	commandRequest, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/sandboxes/"+sandboxID+"/commands", bytes.NewReader(commandBody))
	commandRequest.Header.Set("Authorization", "Bearer "+testToken)
	commandRequest.Header.Set("X-Project-ID", "project-a")
	commandRequest.Header.Set("Content-Type", "application/json")
	commandResponse, err := h.server.Client().Do(commandRequest)
	if err != nil {
		t.Fatal(err)
	}
	stream, _ := io.ReadAll(commandResponse.Body)
	commandResponse.Body.Close()
	if commandResponse.StatusCode != http.StatusOK || !bytes.Contains(stream, []byte(`"type":"started"`)) || !bytes.Contains(stream, []byte(`"data":"aGVsbG8K"`)) || !bytes.Contains(stream, []byte(`"type":"exited"`)) {
		t.Fatalf("command response = %d %s", commandResponse.StatusCode, stream)
	}
	if h.backend.lastCommand.Argv[1] != "secret-command" {
		t.Fatalf("backend command = %#v", h.backend.lastCommand)
	}

	crossTenantRequest, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/sandboxes/"+sandboxID+"/commands", bytes.NewReader(commandBody))
	crossTenantRequest.Header.Set("Authorization", "Bearer "+testToken)
	crossTenantRequest.Header.Set("X-Project-ID", "project-b")
	crossTenantRequest.Header.Set("Content-Type", "application/json")
	crossTenantResponse, err := h.server.Client().Do(crossTenantRequest)
	if err != nil {
		t.Fatal(err)
	}
	crossTenantResponse.Body.Close()
	if crossTenantResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant command returned %d", crossTenantResponse.StatusCode)
	}

	filePath := "/workspace/private.txt"
	fileRequest, _ := http.NewRequest(http.MethodPut, h.server.URL+"/v1/sandboxes/"+sandboxID+"/files?path="+filePath, strings.NewReader("private-file-content"))
	fileRequest.Header.Set("Authorization", "Bearer "+testToken)
	fileRequest.Header.Set("X-Project-ID", "project-a")
	fileResponse, err := h.server.Client().Do(fileRequest)
	if err != nil {
		t.Fatal(err)
	}
	fileResponse.Body.Close()
	if fileResponse.StatusCode != http.StatusOK {
		t.Fatalf("file upload returned %d", fileResponse.StatusCode)
	}

	readRequest, _ := http.NewRequest(http.MethodGet, h.server.URL+"/v1/sandboxes/"+sandboxID+"/files?path="+filePath, nil)
	readRequest.Header.Set("Authorization", "Bearer "+testToken)
	readRequest.Header.Set("X-Project-ID", "project-a")
	readResponse, err := h.server.Client().Do(readRequest)
	if err != nil {
		t.Fatal(err)
	}
	readData, _ := io.ReadAll(readResponse.Body)
	readResponse.Body.Close()
	if readResponse.StatusCode != http.StatusOK || string(readData) != "private-file-content" {
		t.Fatalf("file read = %d %q", readResponse.StatusCode, readData)
	}

	durable, err := os.ReadFile(h.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(durable, []byte("secret-command")) || bytes.Contains(durable, []byte("private-file-content")) || bytes.Contains(durable, []byte(filePath)) {
		t.Fatal("command or file content leaked into durable control state")
	}
}

func TestWorkspaceLifecycleAttachmentAndTenantIsolation(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	resp, first := request(t, h, http.MethodPost, "/v1/workspaces", "project-a", "workspace-create-0001", map[string]any{"name": "agent-state"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create workspace: %d %#v", resp.StatusCode, first)
	}
	workspace := first["resource"].(map[string]any)
	workspaceID := workspace["id"].(string)
	if workspace["state"] != "ready" {
		t.Fatalf("workspace state = %#v", workspace["state"])
	}
	publicPayload, _ := json.Marshal(first)
	for _, forbidden := range []string{"remote-workspace", "backend_id", "backend_name", "token"} {
		if bytes.Contains(publicPayload, []byte(forbidden)) {
			t.Fatalf("public workspace response leaked %q", forbidden)
		}
	}
	resp, repeated := request(t, h, http.MethodPost, "/v1/workspaces", "project-a", "workspace-create-0001", map[string]any{"name": "agent-state"})
	if resp.StatusCode != http.StatusAccepted || repeated["resource"].(map[string]any)["id"] != workspaceID || h.backend.workspaceCreates != 1 {
		t.Fatalf("idempotent workspace create = %d %#v creates=%d", resp.StatusCode, repeated, h.backend.workspaceCreates)
	}
	resp, mismatch := request(t, h, http.MethodPost, "/v1/workspaces", "project-a", "workspace-create-0001", map[string]any{"name": "different-request"})
	if resp.StatusCode != http.StatusConflict || errorCode(mismatch) != "conflict" || h.backend.workspaceCreates != 1 {
		t.Fatalf("idempotency mismatch = %d %#v creates=%d", resp.StatusCode, mismatch, h.backend.workspaceCreates)
	}
	resp, _ = request(t, h, http.MethodGet, "/v1/workspaces/"+workspaceID, "project-b", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-project workspace read = %d", resp.StatusCode)
	}
	resp, listed := request(t, h, http.MethodGet, "/v1/workspaces", "project-b", "", nil)
	if resp.StatusCode != http.StatusOK || len(listed["workspaces"].([]any)) != 0 {
		t.Fatalf("cross-project workspace list = %d %#v", resp.StatusCode, listed)
	}

	environment := createEnvironment(t, h, "project-a")
	body := map[string]any{
		"environment_revision": environment,
		"lifecycle":            map[string]any{"expires_after_seconds": 3600},
		"network":              map[string]any{"allow_internet": false},
		"workspace_mounts":     []map[string]any{{"workspace_id": workspaceID, "path": "/workspace"}},
	}
	resp, sandboxOut := request(t, h, http.MethodPost, "/v1/sandboxes", "project-a", "sandbox-workspace-0001", body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("attach workspace: %d %#v", resp.StatusCode, sandboxOut)
	}
	sandboxID := sandboxOut["resource"].(map[string]any)["id"].(string)
	h.backend.mu.Lock()
	mounts := append([]backend.WorkspaceMount(nil), h.backend.lastCreate.WorkspaceMounts...)
	h.backend.mu.Unlock()
	if len(mounts) != 1 || mounts[0].Name != workspaceID || mounts[0].Path != "/workspace" {
		t.Fatalf("engine mounts = %#v", mounts)
	}
	createCalls := h.backend.createCalls
	resp, _ = request(t, h, http.MethodPost, "/v1/sandboxes", "project-a", "sandbox-workspace-writer-0002", body)
	if resp.StatusCode != http.StatusConflict || h.backend.createCalls != createCalls {
		t.Fatalf("second workspace writer reached engine: status=%d calls=%d", resp.StatusCode, h.backend.createCalls)
	}
	resp, blocked := request(t, h, http.MethodDelete, "/v1/workspaces/"+workspaceID, "project-a", "workspace-delete-0001", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete attached workspace = %d %#v", resp.StatusCode, blocked)
	}
	resp, _ = request(t, h, http.MethodDelete, "/v1/sandboxes/"+sandboxID, "project-a", "sandbox-delete-workspace-0001", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete sandbox = %d", resp.StatusCode)
	}
	resp, deleted := request(t, h, http.MethodDelete, "/v1/workspaces/"+workspaceID, "project-a", "workspace-delete-0002", nil)
	if resp.StatusCode != http.StatusAccepted || deleted["resource"].(map[string]any)["state"] != "deleted" {
		t.Fatalf("delete workspace = %d %#v", resp.StatusCode, deleted)
	}
	resp, listed = request(t, h, http.MethodGet, "/v1/workspaces", "project-a", "", nil)
	if resp.StatusCode != http.StatusOK || len(listed["workspaces"].([]any)) != 0 {
		t.Fatalf("default workspace list = %d %#v", resp.StatusCode, listed)
	}
	resp, listed = request(t, h, http.MethodGet, "/v1/workspaces?include_terminal=true", "project-a", "", nil)
	if resp.StatusCode != http.StatusOK || len(listed["workspaces"].([]any)) != 1 {
		t.Fatalf("complete workspace list = %d %#v", resp.StatusCode, listed)
	}
}

func TestWorkspaceValidationAndRecoveryFailClosed(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	environment := createEnvironment(t, h, "project-a")

	h.backend.workspaceCreateUnconfirmed = true
	resp, uncertain := request(t, h, http.MethodPost, "/v1/workspaces", "project-a", "workspace-uncertain-0001", map[string]any{"name": "recovery"})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unconfirmed workspace create = %d %#v", resp.StatusCode, uncertain)
	}
	h.backend.workspaceCreateUnconfirmed = false
	if err := h.service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	resp, recovered := request(t, h, http.MethodPost, "/v1/workspaces", "project-a", "workspace-uncertain-0001", map[string]any{"name": "recovery"})
	if resp.StatusCode != http.StatusAccepted || recovered["resource"].(map[string]any)["state"] != "ready" {
		t.Fatalf("recovered workspace = %d %#v", resp.StatusCode, recovered)
	}
	recoveredID := recovered["resource"].(map[string]any)["id"].(string)

	before := h.backend.createCalls
	invalidBody := map[string]any{
		"environment_revision": environment,
		"lifecycle":            map[string]any{"expires_after_seconds": 3600},
		"network":              map[string]any{"allow_internet": false},
		"workspace_mounts":     []map[string]any{{"workspace_id": recoveredID, "path": "/"}},
	}
	resp, _ = request(t, h, http.MethodPost, "/v1/sandboxes", "project-a", "sandbox-invalid-mount-0001", invalidBody)
	if resp.StatusCode != http.StatusBadRequest || h.backend.createCalls != before {
		t.Fatalf("invalid mount reached engine: status=%d calls=%d", resp.StatusCode, h.backend.createCalls)
	}
	invalidBody["workspace_mounts"] = []map[string]any{{"workspace_id": recoveredID, "path": "/workspace"}}
	resp, _ = request(t, h, http.MethodPost, "/v1/sandboxes", "project-b", "sandbox-cross-workspace-0001", invalidBody)
	if resp.StatusCode != http.StatusNotFound || h.backend.createCalls != before {
		t.Fatalf("cross-project mount reached engine: status=%d calls=%d", resp.StatusCode, h.backend.createCalls)
	}

	h.backend.workspaceDeleteFailures = 1
	resp, _ = request(t, h, http.MethodDelete, "/v1/workspaces/"+recoveredID, "project-a", "workspace-delete-uncertain-0001", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unconfirmed workspace delete = %d", resp.StatusCode)
	}
	if err := h.service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	resp, reconciled := request(t, h, http.MethodGet, "/v1/workspaces/"+recoveredID, "project-a", "", nil)
	if resp.StatusCode != http.StatusOK || reconciled["state"] != "deleted" {
		t.Fatalf("workspace cleanup not reconciled: %d %#v", resp.StatusCode, reconciled)
	}
}

func TestCheckpointIdempotencyKeyIsBoundToNameAndKind(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	environment := createEnvironment(t, h, "project-a")
	createBody := map[string]any{
		"environment_revision": environment,
		"lifecycle":            map[string]any{"expires_after_seconds": 3600},
		"network":              map[string]any{"allow_internet": false},
	}
	response, created := request(t, h, http.MethodPost, "/v1/sandboxes", "project-a", "sandbox-checkpoint-0001", createBody)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("create sandbox = %d %#v", response.StatusCode, created)
	}
	sandboxID := created["resource"].(map[string]any)["id"].(string)
	path := "/v1/sandboxes/" + sandboxID + "/checkpoints"
	response, first := request(t, h, http.MethodPost, path, "project-a", "checkpoint-request-0001", map[string]any{"name": "clean", "kind": "filesystem"})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("create checkpoint = %d %#v", response.StatusCode, first)
	}
	checkpointID := first["resource"].(map[string]any)["id"]
	response, replay := request(t, h, http.MethodPost, path, "project-a", "checkpoint-request-0001", map[string]any{"name": "clean", "kind": "filesystem"})
	if response.StatusCode != http.StatusAccepted || replay["resource"].(map[string]any)["id"] != checkpointID {
		t.Fatalf("checkpoint replay = %d %#v", response.StatusCode, replay)
	}
	response, mismatch := request(t, h, http.MethodPost, path, "project-a", "checkpoint-request-0001", map[string]any{"name": "changed", "kind": "filesystem"})
	if response.StatusCode != http.StatusConflict || errorCode(mismatch) != "conflict" {
		t.Fatalf("checkpoint payload mismatch = %d %#v", response.StatusCode, mismatch)
	}
}

func TestCheckpointDeletionIsTenantBoundInUseSafeAndIdempotent(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	environment := createEnvironment(t, h, "project-a")
	createBody := map[string]any{
		"environment_revision": environment,
		"lifecycle":            map[string]any{"expires_after_seconds": 3600},
		"network":              map[string]any{"allow_internet": false},
	}
	response, created := request(t, h, http.MethodPost, "/v1/sandboxes", "project-a", "sandbox-checkpoint-delete-0001", createBody)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("create sandbox = %d %#v", response.StatusCode, created)
	}
	sandboxID := created["resource"].(map[string]any)["id"].(string)
	response, checkpointOut := request(t, h, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/checkpoints", "project-a", "checkpoint-delete-create-0001", map[string]any{"name": "delete-me", "kind": "filesystem"})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("create checkpoint = %d %#v", response.StatusCode, checkpointOut)
	}
	checkpointID := checkpointOut["resource"].(map[string]any)["id"].(string)
	deletePath := "/v1/checkpoints/" + checkpointID

	response, denied := request(t, h, http.MethodDelete, deletePath, "project-b", "checkpoint-delete-cross-0001", nil)
	if response.StatusCode != http.StatusNotFound || errorCode(denied) != "not_found" {
		t.Fatalf("cross-project delete = %d %#v", response.StatusCode, denied)
	}
	response, sourceInUse := request(t, h, http.MethodDelete, deletePath, "project-a", "checkpoint-delete-source-in-use-0001", nil)
	if response.StatusCode != http.StatusConflict || errorCode(sourceInUse) != "conflict" {
		t.Fatalf("source-active checkpoint delete = %d %#v", response.StatusCode, sourceInUse)
	}

	forkBody := map[string]any{
		"checkpoint_id": checkpointID,
		"lifecycle":     map[string]any{"expires_after_seconds": 3600},
		"network":       map[string]any{"allow_internet": false},
	}
	response, forked := request(t, h, http.MethodPost, "/v1/sandboxes", "project-a", "sandbox-from-checkpoint-0001", forkBody)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("create from checkpoint = %d %#v", response.StatusCode, forked)
	}
	forkID := forked["resource"].(map[string]any)["id"].(string)
	response, _ = request(t, h, http.MethodDelete, "/v1/sandboxes/"+sandboxID, "project-a", "sandbox-checkpoint-source-delete-0001", nil)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("delete checkpoint source sandbox = %d", response.StatusCode)
	}
	response, inUse := request(t, h, http.MethodDelete, deletePath, "project-a", "checkpoint-delete-in-use-0001", nil)
	if response.StatusCode != http.StatusConflict || errorCode(inUse) != "conflict" {
		t.Fatalf("in-use checkpoint delete = %d %#v", response.StatusCode, inUse)
	}
	response, _ = request(t, h, http.MethodDelete, "/v1/sandboxes/"+forkID, "project-a", "sandbox-from-checkpoint-delete-0001", nil)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("delete checkpoint-derived sandbox = %d", response.StatusCode)
	}
	response, deleted := request(t, h, http.MethodDelete, deletePath, "project-a", "checkpoint-delete-0001", nil)
	if response.StatusCode != http.StatusAccepted || deleted["operation"].(map[string]any)["state"] != "succeeded" {
		t.Fatalf("delete checkpoint = %d %#v", response.StatusCode, deleted)
	}
	response, replay := request(t, h, http.MethodDelete, deletePath, "project-a", "checkpoint-delete-0001", nil)
	if response.StatusCode != http.StatusAccepted || replay["operation"].(map[string]any)["state"] != "succeeded" {
		t.Fatalf("delete checkpoint replay = %d %#v", response.StatusCode, replay)
	}
}

func TestCheckpointDeletionFailureDoesNotForgetCheckpoint(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	environment := createEnvironment(t, h, "project-a")
	response, created := request(t, h, http.MethodPost, "/v1/sandboxes", "project-a", "sandbox-checkpoint-failure-0001", map[string]any{
		"environment_revision": environment,
		"lifecycle":            map[string]any{"expires_after_seconds": 3600},
		"network":              map[string]any{"allow_internet": false},
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("create sandbox = %d %#v", response.StatusCode, created)
	}
	sandboxID := created["resource"].(map[string]any)["id"].(string)
	response, checkpointOut := request(t, h, http.MethodPost, "/v1/sandboxes/"+sandboxID+"/checkpoints", "project-a", "checkpoint-failure-create-0001", map[string]any{"name": "retry", "kind": "filesystem"})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("create checkpoint = %d %#v", response.StatusCode, checkpointOut)
	}
	checkpointID := checkpointOut["resource"].(map[string]any)["id"].(string)
	response, _ = request(t, h, http.MethodDelete, "/v1/sandboxes/"+sandboxID, "project-a", "sandbox-checkpoint-failure-source-delete-0001", nil)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("delete checkpoint source sandbox = %d", response.StatusCode)
	}
	h.backend.checkpointDeleteFailures = 1
	response, failed := request(t, h, http.MethodDelete, "/v1/checkpoints/"+checkpointID, "project-a", "checkpoint-delete-failure-0001", nil)
	if response.StatusCode != http.StatusBadGateway || errorCode(failed) != "backend_failure" {
		t.Fatalf("unconfirmed checkpoint delete = %d %#v", response.StatusCode, failed)
	}
	response, forked := request(t, h, http.MethodPost, "/v1/sandboxes", "project-a", "checkpoint-still-usable-0001", map[string]any{
		"checkpoint_id": checkpointID,
		"lifecycle":     map[string]any{"expires_after_seconds": 3600},
		"network":       map[string]any{"allow_internet": false},
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("checkpoint was forgotten after failed delete = %d %#v", response.StatusCode, forked)
	}
}

func TestAdmissionLimitPreservesHealthAndExportsContentFreeMetrics(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	environment := createEnvironment(t, h, "project-a")
	response, created := request(t, h, http.MethodPost, "/v1/sandboxes", "project-a", "sandbox-admission-0001", map[string]any{
		"environment_revision": environment,
		"lifecycle":            map[string]any{"expires_after_seconds": 3600},
		"network":              map[string]any{"allow_internet": false},
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("create sandbox = %d %#v", response.StatusCode, created)
	}
	sandboxID := created["resource"].(map[string]any)["id"].(string)
	h.backend.runStarted = make(chan struct{})
	h.backend.releaseRun = make(chan struct{})
	api, err := New(h.service, testToken, WithMaxInFlightRequests(1))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.Handler())
	defer server.Close()

	commandDone := make(chan error, 1)
	go func() {
		body := strings.NewReader(`{"argv":["/bin/true"],"timeout_seconds":30}`)
		req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/sandboxes/"+sandboxID+"/commands", body)
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("X-Project-ID", "project-a")
		resp, err := server.Client().Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		commandDone <- err
	}()
	select {
	case <-h.backend.runStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("command did not occupy the request admission slot")
	}

	list, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/sandboxes", nil)
	list.Header.Set("Authorization", "Bearer "+testToken)
	list.Header.Set("X-Project-ID", "project-a")
	listResponse, err := server.Client().Do(list)
	if err != nil {
		t.Fatal(err)
	}
	listResponse.Body.Close()
	if listResponse.StatusCode != http.StatusServiceUnavailable || listResponse.Header.Get("Retry-After") != "1" || listResponse.Header.Get("X-Request-ID") == "" {
		t.Fatalf("overload response status=%d retry=%q request-id=%q", listResponse.StatusCode, listResponse.Header.Get("Retry-After"), listResponse.Header.Get("X-Request-ID"))
	}

	healthResponse, err := server.Client().Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	healthResponse.Body.Close()
	if healthResponse.StatusCode != http.StatusOK {
		t.Fatalf("health was blocked by admission: %d", healthResponse.StatusCode)
	}
	metricsResponse, err := server.Client().Get(server.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metricsBody, _ := io.ReadAll(metricsResponse.Body)
	metricsResponse.Body.Close()
	if metricsResponse.StatusCode != http.StatusOK || !bytes.Contains(metricsBody, []byte("runtime_http_admission_rejections_total 1")) || bytes.Contains(metricsBody, []byte("project-a")) || bytes.Contains(metricsBody, []byte(sandboxID)) {
		t.Fatalf("metrics status=%d body=%s", metricsResponse.StatusCode, metricsBody)
	}

	close(h.backend.releaseRun)
	if err := <-commandDone; err != nil {
		t.Fatal(err)
	}
	if _, err := New(h.service, testToken, WithMaxInFlightRequests(0)); err == nil {
		t.Fatal("zero request admission limit was accepted")
	}
}

func TestMetricsExposeContentFreeLifecyclePhases(t *testing.T) {
	registry := telemetry.NewRegistry()
	h := newHarnessWithServiceOptions(t, service.WithPhaseObserver(registry))
	defer h.close()
	environment := createEnvironment(t, h, "project-phase-secret")
	response, created := request(t, h, http.MethodPost, "/v1/sandboxes", "project-phase-secret", "sandbox-phase-metrics-0001", map[string]any{
		"environment_revision": environment,
		"lifecycle":            map[string]any{"expires_after_seconds": 3600},
		"network":              map[string]any{"allow_internet": false},
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("create sandbox = %d %#v", response.StatusCode, created)
	}
	sandboxID := created["resource"].(map[string]any)["id"].(string)

	api, err := New(h.service, testToken, WithPhaseMetrics(registry))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.Handler())
	defer server.Close()
	metricsResponse, err := server.Client().Get(server.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metricsBody, _ := io.ReadAll(metricsResponse.Body)
	metricsResponse.Body.Close()
	for _, phase := range []string{"persist_intent", "backend_call", "persist_result"} {
		expected := `operation="sandbox_create",phase="` + phase + `",outcome="success"`
		if !bytes.Contains(metricsBody, []byte(expected)) {
			t.Fatalf("metrics omitted %q: %s", expected, metricsBody)
		}
	}
	if bytes.Contains(metricsBody, []byte("project-phase-secret")) || bytes.Contains(metricsBody, []byte(sandboxID)) || bytes.Contains(metricsBody, []byte(environment)) {
		t.Fatalf("resource identity reached phase metrics: %s", metricsBody)
	}
}

func TestRequestLogUsesRoutePatternInsteadOfPreviewCapability(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	var logs bytes.Buffer
	api, err := New(h.service, testToken, WithRequestLogger(log.New(&logs, "", 0)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.Handler())
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/p/private-capability-value/private/file")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("preview response=%d", response.StatusCode)
	}
	if strings.Contains(logs.String(), "private-capability-value") || strings.Contains(logs.String(), "/private/file") {
		t.Fatalf("request log leaked preview path: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `route="/p/{token}/{path...}"`) {
		t.Fatalf("request log omitted matched route: %s", logs.String())
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
	if report.Qualification != "sandbox_runtime_conformant" {
		t.Fatalf("qualification = %q", report.Qualification)
	}
}

var _ backend.Backend = (*testBackend)(nil)
var _ backend.GuestRuntime = (*testBackend)(nil)
var _ backend.PortRuntime = (*testBackend)(nil)
var _ backend.WorkspaceRuntime = (*testBackend)(nil)
var _ = errors.Is
