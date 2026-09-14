package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/sandbox-runtime-lab/internal/backend"
	"github.com/infercrane/sandbox-runtime-lab/internal/domain"
)

type testEngine struct {
	caps          backend.Capabilities
	lastBackendID string
}

func (e *testEngine) Name() string                       { return backend.DefaultName }
func (e *testEngine) Capabilities() backend.Capabilities { return e.caps }
func (e *testEngine) Create(context.Context, backend.CreateRequest) (backend.Sandbox, error) {
	return backend.Sandbox{}, backend.ErrCapabilityUnavailable
}
func (e *testEngine) Find(context.Context, string, string) (backend.Sandbox, error) {
	return backend.Sandbox{}, backend.ErrCapabilityUnavailable
}
func (e *testEngine) Inspect(context.Context, string) (backend.Sandbox, error) {
	return backend.Sandbox{}, backend.ErrCapabilityUnavailable
}
func (e *testEngine) Pause(context.Context, string, domain.CheckpointKind) error {
	return backend.ErrCapabilityUnavailable
}
func (e *testEngine) Resume(context.Context, string, domain.CheckpointKind, int64) (backend.Sandbox, error) {
	return backend.Sandbox{}, backend.ErrCapabilityUnavailable
}
func (e *testEngine) Delete(context.Context, string) error { return backend.ErrCapabilityUnavailable }
func (e *testEngine) Checkpoint(context.Context, string, domain.CheckpointKind, string) (backend.Checkpoint, error) {
	return backend.Checkpoint{}, backend.ErrCapabilityUnavailable
}
func (e *testEngine) DeleteCheckpoint(context.Context, string) error {
	return backend.ErrCapabilityUnavailable
}
func (e *testEngine) Run(_ context.Context, id string, _ backend.CommandRequest, emit func(backend.CommandEvent) error) error {
	e.lastBackendID = id
	return emit(backend.CommandEvent{Type: backend.CommandExited, Exited: true})
}
func (e *testEngine) WriteFile(_ context.Context, id, path string, source io.Reader) (backend.FileInfo, error) {
	e.lastBackendID = id
	data, err := io.ReadAll(source)
	return backend.FileInfo{Path: path, Size: int64(len(data))}, err
}
func (e *testEngine) ReadFile(_ context.Context, id, path string, destination io.Writer) (backend.FileInfo, error) {
	e.lastBackendID = id
	n, err := destination.Write([]byte("data"))
	return backend.FileInfo{Path: path, Size: int64(n)}, err
}
func (e *testEngine) ValidatePort(port uint16) error {
	if port == 0 {
		return errors.New("invalid port")
	}
	return nil
}
func (e *testEngine) RoundTripPort(_ context.Context, id string, _ uint16, _ *http.Request) (*http.Response, error) {
	e.lastBackendID = id
	return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
}

func TestBindingRejectsMissingOrUnsafeIdentity(t *testing.T) {
	verifier := func(string, string, string, int64) bool { return true }
	for _, values := range [][4]string{
		{"", "sandbox-a", "engine-a", "operation-a"},
		{"project-a", "", "engine-a", "operation-a"},
		{"project-a", "sandbox-a", "", "operation-a"},
		{"project-a", "sandbox-a", "engine-a", ""},
		{"project-a", "sandbox-a", "engine-a\nsecond", "operation-a"},
	} {
		if _, err := BindAuthorizedSandbox(values[0], values[1], values[2], values[3], 1, time.Now().Add(time.Minute), verifier); err == nil {
			t.Fatalf("BindAuthorizedSandbox(%q) succeeded", values)
		}
	}
}

func TestBindingStringDoesNotExposeBackendIdentity(t *testing.T) {
	binding, err := BindAuthorizedSandbox("project-a", "sandbox-a", "upstream-secret-route", "operation-a", 7, time.Now().Add(time.Minute), func(string, string, string, int64) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if got := binding.String(); strings.Contains(got, "upstream-secret-route") {
		t.Fatalf("binding string exposed backend identity: %q", got)
	}
	encoded, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "{}" {
		t.Fatalf("binding serialization exposed internal fields: %s", encoded)
	}
}

func TestBackendDataPlaneRequiresBindingAndForwardsOnlyBackendIdentity(t *testing.T) {
	engine := &testEngine{caps: backend.Capabilities{CommandStreaming: true, FileReadWrite: true, AuthenticatedPorts: true}}
	dataPlane := NewBackendDataPlane(engine)
	currentRevision := int64(7)
	binding, err := BindAuthorizedSandbox("project-a", "sandbox-a", "engine-a", "operation-a", currentRevision, time.Now().Add(time.Minute), func(projectID, sandboxID, backendID string, revision int64) bool {
		return projectID == "project-a" && sandboxID == "sandbox-a" && backendID == "engine-a" && revision == currentRevision
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dataPlane.Run(context.Background(), SandboxBinding{}, backend.CommandRequest{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil }); err == nil {
		t.Fatal("zero binding reached the engine")
	}
	if engine.lastBackendID != "" {
		t.Fatalf("engine called for invalid binding: %q", engine.lastBackendID)
	}
	if err := dataPlane.Run(context.Background(), binding, backend.CommandRequest{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if engine.lastBackendID != "engine-a" {
		t.Fatalf("engine backend id=%q", engine.lastBackendID)
	}
	info, err := dataPlane.WriteFile(context.Background(), binding, "/workspace/a", bytes.NewBufferString("abc"))
	if err != nil || info.Size != 3 {
		t.Fatalf("WriteFile()=%#v, %v", info, err)
	}
	var output bytes.Buffer
	if _, err := dataPlane.ReadFile(context.Background(), binding, "/workspace/a", &output); err != nil || output.String() != "data" {
		t.Fatalf("ReadFile()=%q, %v", output.String(), err)
	}
	response, err := dataPlane.RoundTripPort(context.Background(), binding, 8080, &http.Request{})
	if err != nil || response.StatusCode != http.StatusNoContent {
		t.Fatalf("RoundTripPort()=%#v, %v", response, err)
	}
}

func TestBackendDataPlaneFailsClosedWhenInterfacesAreUnavailable(t *testing.T) {
	engine := &lifecycleOnlyEngine{caps: backend.Capabilities{CommandStreaming: true, FileReadWrite: true, AuthenticatedPorts: true}}
	dataPlane := NewBackendDataPlane(engine)
	if dataPlane.Capabilities() != (Capabilities{}) {
		t.Fatalf("unexpected capabilities: %#v", dataPlane.Capabilities())
	}
	binding, err := BindAuthorizedSandbox("project-a", "sandbox-a", "engine-a", "operation-a", 1, time.Now().Add(time.Minute), func(string, string, string, int64) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if err := dataPlane.Run(context.Background(), binding, backend.CommandRequest{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil }); !errors.Is(err, backend.ErrCapabilityUnavailable) {
		t.Fatalf("Run() error=%v", err)
	}
}

type lifecycleOnlyEngine struct {
	backend.Backend
	caps backend.Capabilities
}

func (e *lifecycleOnlyEngine) Name() string                       { return backend.DefaultName }
func (e *lifecycleOnlyEngine) Capabilities() backend.Capabilities { return e.caps }

func TestStaleAndRevokedBindingsCannotReachEngine(t *testing.T) {
	engine := &testEngine{caps: backend.Capabilities{CommandStreaming: true}}
	dataPlane := NewBackendDataPlane(engine)
	currentRevision := int64(3)
	verifier := func(_, _, _ string, revision int64) bool { return revision == currentRevision }
	binding, err := BindAuthorizedSandbox("project-a", "sandbox-a", "engine-a", "operation-a", currentRevision, time.Now().Add(time.Minute), verifier)
	if err != nil {
		t.Fatal(err)
	}

	currentRevision++
	if err := dataPlane.Run(context.Background(), binding, backend.CommandRequest{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil }); !errors.Is(err, ErrStaleBinding) {
		t.Fatalf("stale binding error=%v", err)
	}
	if engine.lastBackendID != "" {
		t.Fatalf("stale binding reached engine: %q", engine.lastBackendID)
	}

	fresh, err := BindAuthorizedSandbox("project-a", "sandbox-a", "engine-a", "operation-b", currentRevision, time.Now().Add(time.Minute), verifier)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Revoke()
	if err := dataPlane.Run(context.Background(), fresh, backend.CommandRequest{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil }); !errors.Is(err, ErrRevokedBinding) {
		t.Fatalf("revoked binding error=%v", err)
	}
	if engine.lastBackendID != "" {
		t.Fatalf("revoked binding reached engine: %q", engine.lastBackendID)
	}
}

func TestExpiredBindingCannotReachEngine(t *testing.T) {
	engine := &testEngine{caps: backend.Capabilities{FileReadWrite: true}}
	dataPlane := NewBackendDataPlane(engine)
	binding, err := BindAuthorizedSandbox("project-a", "sandbox-a", "engine-a", "operation-a", 1, time.Now().Add(time.Minute), func(string, string, string, int64) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	// Package-level test access avoids a real-time sleep while proving that an
	// expired handoff is rejected before any substrate operation.
	binding.expiresAt = time.Now().Add(-time.Second)
	if _, err := dataPlane.WriteFile(context.Background(), binding, "/workspace/a", strings.NewReader("data")); !errors.Is(err, ErrStaleBinding) {
		t.Fatalf("expired binding error=%v", err)
	}
	if engine.lastBackendID != "" {
		t.Fatalf("expired binding reached engine: %q", engine.lastBackendID)
	}
}

var _ backend.Backend = (*testEngine)(nil)
var _ backend.GuestRuntime = (*testEngine)(nil)
var _ backend.PortRuntime = (*testEngine)(nil)
