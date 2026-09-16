package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/brezel/internal/backend"
	"github.com/infercrane/brezel/internal/nodeledger"
	"github.com/infercrane/brezel/internal/telemetry"
)

const (
	testRelayIssuer   = "brezel-api"
	testRelayKeyID    = "api-key-1"
	testRelayAudience = "brezel-node"
	testRelayNode     = "node-a"
	testRelayEpoch    = "boot-a"
	testRelayRoute    = "route-a"
	testRelayProject  = "project-a"
	testRelaySandbox  = "sandbox-a"
)

func TestRelayDataPlaneEndToEndProtocol(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewCapabilitySigner(testRelayIssuer, testRelayKeyID, private)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewCapabilityVerifier(testRelayIssuer, map[string]ed25519.PublicKey{testRelayKeyID: public})
	if err != nil {
		t.Fatal(err)
	}
	route := nodeledger.Route{RouteID: testRelayRoute, ProjectID: testRelayProject, SandboxID: testRelaySandbox, Generation: 11, State: nodeledger.StateReady, LastActivity: time.Now().UTC()}
	var operations []CapabilityOperation
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/routes/"+testRelayRoute {
			_ = json.NewEncoder(w).Encode(relayRouteResponse{NodeID: testRelayNode, BootEpoch: testRelayEpoch, Route: route})
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), RelayCapabilityScheme+" ")
		if token == r.Header.Get("Authorization") || r.Header.Get(RelayRouteHeader) != route.RouteID {
			http.Error(w, "missing relay authorization", http.StatusUnauthorized)
			return
		}
		var operation CapabilityOperation
		var canonical []byte
		switch {
		case r.URL.Path == "/v1/commands":
			operation = CapabilityRunCommand
			var request relayCommandRequest
			if err := decodeRelayJSON(r.Body, 1<<20, &request); err != nil {
				http.Error(w, "invalid command", http.StatusBadRequest)
				return
			}
			canonical, _ = canonicalRelayJSON(request)
		case r.URL.Path == "/v1/files" && r.Method == http.MethodPut:
			operation = CapabilityWriteFile
			data, _ := io.ReadAll(r.Body)
			descriptor := fileWriteDescriptor(r.URL.Query().Get("path"), data)
			if r.Header.Get(RelayContentSHAHeader) != descriptor.SHA256 {
				http.Error(w, "digest mismatch", http.StatusBadRequest)
				return
			}
			canonical, _ = canonicalRelayJSON(descriptor)
		case r.URL.Path == "/v1/files" && r.Method == http.MethodGet:
			operation = CapabilityReadFile
			canonical, _ = canonicalRelayJSON(relayFileDescriptor{Path: r.URL.Query().Get("path")})
		case strings.HasPrefix(r.URL.Path, "/v1/ports/3000/"):
			operation = CapabilityProxyPort
			data, _ := io.ReadAll(r.Body)
			protocol := portProtocolRequest(r.Method, "/hello", r.URL.RawQuery, r.Header, data)
			canonical, _ = canonicalRelayJSON(protocol)
		default:
			http.NotFound(w, r)
			return
		}
		digest, _ := RequestDigest(operation, canonical)
		claims, err := verifier.Verify(token, CapabilityExpected{
			Audience: testRelayAudience, NodeID: testRelayNode, BootEpoch: testRelayEpoch,
			RouteID: route.RouteID, Generation: route.Generation, ProjectID: route.ProjectID,
			SandboxID: route.SandboxID, Operation: operation, RequestDigest: digest,
		})
		if err != nil {
			http.Error(w, "invalid capability: "+err.Error(), http.StatusUnauthorized)
			return
		}
		operations = append(operations, operation)
		switch operation {
		case CapabilityRunCommand:
			w.Header().Set("Content-Type", "application/x-ndjson")
			_ = json.NewEncoder(w).Encode(backend.CommandEvent{Type: backend.CommandStarted, PID: 7})
			_ = json.NewEncoder(w).Encode(backend.CommandEvent{Type: backend.CommandStdout, Data: []byte("ok\n")})
			_ = json.NewEncoder(w).Encode(backend.CommandEvent{Type: backend.CommandExited, Exited: true})
		case CapabilityWriteFile:
			if claims.Bounds.MaxRequestBytes < 1 {
				http.Error(w, "missing bound", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(backend.FileInfo{Path: "/workspace/a", Size: 4, ContentType: "application/octet-stream"})
		case CapabilityReadFile:
			w.Header().Set(RelayFilePathHeader, "/workspace/a")
			w.Header().Set(RelayFileSizeHeader, "4")
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("data"))
		case CapabilityProxyPort:
			if claims.Bounds.Port != 3000 {
				http.Error(w, "wrong port", http.StatusBadRequest)
				return
			}
			w.Header().Set("X-App", "preview")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("preview"))
		}
	})
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	diagnostics := &relayDiagnosticCollector{}
	client, err := newRelayDataPlane(server.URL, server.Client(), signer, testRelayAudience, testRelayNode, WithRelayCommandDiagnostics(diagnostics))
	if err != nil {
		t.Fatal(err)
	}
	binding := testRelayBinding(t, testRelayRoute, route.Generation)
	var events []backend.CommandEvent
	if err := client.Run(context.Background(), binding, backend.CommandRequest{Argv: []string{"printf", "ok"}}, func(event backend.CommandEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || string(events[1].Data) != "ok\n" {
		t.Fatalf("unexpected command events: %#v", events)
	}
	assertRelayDiagnostic(t, diagnostics.samples, telemetry.CommandDiagnosticRelayClient, 3, 3)
	info, err := client.WriteFile(context.Background(), binding, "/workspace/a", strings.NewReader("data"))
	if err != nil || info.Size != 4 {
		t.Fatalf("WriteFile()=%#v, %v", info, err)
	}
	var file bytes.Buffer
	info, err = client.ReadFile(context.Background(), binding, "/workspace/a", &file)
	if err != nil || file.String() != "data" || info.ContentType != "text/plain" {
		t.Fatalf("ReadFile()=%#v, %q, %v", info, file.String(), err)
	}
	portRequest := httptest.NewRequest(http.MethodPost, "https://example.invalid/hello?q=1", strings.NewReader("body"))
	portRequest.Header.Set("X-App-Request", "yes")
	response, err := client.RoundTripPort(context.Background(), binding, 3000, portRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusCreated || string(data) != "preview" || response.Header.Get("X-App") != "preview" {
		t.Fatalf("port response status=%d body=%q header=%q err=%v", response.StatusCode, data, response.Header.Get("X-App"), err)
	}
	wantOperations := []CapabilityOperation{CapabilityRunCommand, CapabilityWriteFile, CapabilityReadFile, CapabilityProxyPort}
	if len(operations) != len(wantOperations) {
		t.Fatalf("operations=%v", operations)
	}
	for index := range wantOperations {
		if operations[index] != wantOperations[index] {
			t.Fatalf("operations=%v", operations)
		}
	}
}

func TestRelayDataPlaneRejectsStaleRouteBeforeOperation(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := NewCapabilitySigner(testRelayIssuer, testRelayKeyID, private)
	operationCalled := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/routes/") {
			_ = json.NewEncoder(w).Encode(relayRouteResponse{NodeID: testRelayNode, BootEpoch: testRelayEpoch, Route: nodeledger.Route{
				RouteID: testRelayRoute, ProjectID: testRelayProject, SandboxID: testRelaySandbox, Generation: 12, State: nodeledger.StateReady,
			}})
			return
		}
		operationCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := newRelayDataPlane(server.URL, server.Client(), signer, testRelayAudience, testRelayNode)
	if err != nil {
		t.Fatal(err)
	}
	err = client.Run(context.Background(), testRelayBinding(t, testRelayRoute, 11), backend.CommandRequest{Argv: []string{"true"}}, func(backend.CommandEvent) error { return nil })
	if !errors.Is(err, ErrStaleBinding) || operationCalled {
		t.Fatalf("Run() error=%v operationCalled=%v", err, operationCalled)
	}
}

func TestRelayClientAndServerEndToEnd(t *testing.T) {
	harness := newRelayServerHarness(t)
	serverDiagnostics := &relayDiagnosticCollector{}
	harness.server.diagnostics = serverDiagnostics
	server := httptest.NewTLSServer(harness.server)
	defer server.Close()
	clientDiagnostics := &relayDiagnosticCollector{}
	client, err := newRelayDataPlane(server.URL, server.Client(), harness.signer, relayServerAudience, relayServerNodeID, WithRelayCommandDiagnostics(clientDiagnostics))
	if err != nil {
		t.Fatal(err)
	}
	binding, err := BindAuthorizedSandbox(relayServerProject, relayServerSandbox, relayServerRouteID, "operation-a", 1, time.Now().Add(time.Minute), func(projectID, sandboxID, routeID string, revision int64) bool {
		return projectID == relayServerProject && sandboxID == relayServerSandbox && routeID == relayServerRouteID && revision == 1
	}, WithNodeGeneration(harness.route.Generation))
	if err != nil {
		t.Fatal(err)
	}
	var events []backend.CommandEvent
	if err := client.Run(context.Background(), binding, backend.CommandRequest{Argv: []string{"true"}}, func(event backend.CommandEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || harness.engine.lastEngineID != relayServerEngineID {
		t.Fatalf("command events=%#v engine=%q", events, harness.engine.lastEngineID)
	}
	assertRelayDiagnostic(t, clientDiagnostics.samples, telemetry.CommandDiagnosticRelayClient, 3, 3)
	assertRelayDiagnostic(t, serverDiagnostics.samples, telemetry.CommandDiagnosticRelayServer, 3, 3)
	info, err := client.WriteFile(context.Background(), binding, "/workspace/e2e.txt", strings.NewReader("relay"))
	if err != nil || info.Size != 5 || string(harness.engine.writtenData) != "relay" {
		t.Fatalf("WriteFile()=%#v data=%q error=%v", info, harness.engine.writtenData, err)
	}
	var downloaded bytes.Buffer
	info, err = client.ReadFile(context.Background(), binding, "/workspace/e2e.txt", &downloaded)
	if err != nil || downloaded.String() != string(harness.engine.fileData) || info.Size != int64(len(harness.engine.fileData)) {
		t.Fatalf("ReadFile()=%#v data=%q error=%v", info, downloaded.String(), err)
	}
	request := httptest.NewRequest(http.MethodPost, "https://app.invalid/api%2Fagent?q=1", strings.NewReader("input"))
	request.Header.Set("Content-Type", "text/plain")
	response, err := client.RoundTripPort(context.Background(), binding, 3000, request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != harness.engine.portStatus || !bytes.Equal(data, harness.engine.portBody) {
		t.Fatalf("RoundTripPort() status=%d data=%q error=%v", response.StatusCode, data, err)
	}
	if harness.engine.lastPortRequest.URL.EscapedPath() != "/api%2Fagent" || harness.engine.lastPortRequest.URL.RawQuery != "q=1" {
		t.Fatalf("upstream URL=%q", harness.engine.lastPortRequest.URL.String())
	}
}

type relayDiagnosticCollector struct {
	samples []telemetry.CommandDiagnostic
}

func (c *relayDiagnosticCollector) ObserveCommandDiagnostic(sample telemetry.CommandDiagnostic) {
	c.samples = append(c.samples, sample)
}

func assertRelayDiagnostic(t *testing.T, samples []telemetry.CommandDiagnostic, component telemetry.CommandDiagnosticComponent, events, bytes uint64) {
	t.Helper()
	if len(samples) != 1 {
		t.Fatalf("%s diagnostic samples=%#v", component, samples)
	}
	sample := samples[0]
	if sample.Component != component || sample.Outcome != telemetry.OutcomeSuccess || sample.StreamEvents != events || sample.StreamBytes != bytes || sample.FirstEventCount != 1 || sample.TerminalEventCount != 1 {
		t.Fatalf("%s diagnostic=%#v", component, sample)
	}
	if sample.AcceptedToFirstEvent < 0 || sample.AcceptedToTerminal < sample.AcceptedToFirstEvent || sample.AcceptedToEOFReady < sample.AcceptedToTerminal {
		t.Fatalf("%s diagnostic is not monotonic: %#v", component, sample)
	}
}

func TestRelayDataPlaneRejectsInsecureEndpointAndOversizedUpload(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := NewCapabilitySigner(testRelayIssuer, testRelayKeyID, private)
	if _, err := newRelayDataPlane("http://node.invalid", &http.Client{Transport: http.DefaultTransport}, signer, testRelayAudience, testRelayNode); err == nil {
		t.Fatal("insecure relay endpoint was accepted")
	}
	if _, err := NewRelayDataPlane("https://node.invalid", nil, signer, testRelayAudience, testRelayNode); err == nil {
		t.Fatal("production relay client accepted a missing mTLS configuration")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(relayRouteResponse{NodeID: testRelayNode, BootEpoch: testRelayEpoch, Route: nodeledger.Route{RouteID: testRelayRoute, ProjectID: testRelayProject, SandboxID: testRelaySandbox, Generation: 1, State: nodeledger.StateReady}})
	}))
	defer server.Close()
	client, err := newRelayDataPlane(server.URL, server.Client(), signer, testRelayAudience, testRelayNode)
	if err != nil {
		t.Fatal(err)
	}
	large := io.LimitReader(&infiniteAReader{}, relayClientMaxFileUpload+1)
	if _, err := client.WriteFile(context.Background(), testRelayBinding(t, testRelayRoute, 1), "/workspace/a", large); !errors.Is(err, errRelayBodyLimit) {
		t.Fatalf("oversized WriteFile() error=%v", err)
	}
}

func testRelayBinding(t *testing.T, routeID string, generation uint64) SandboxBinding {
	t.Helper()
	binding, err := BindAuthorizedSandbox(testRelayProject, testRelaySandbox, routeID, "operation-a", 1, time.Now().Add(time.Minute), func(projectID, sandboxID, backendID string, revision int64) bool {
		return projectID == testRelayProject && sandboxID == testRelaySandbox && backendID == routeID && revision == 1
	}, WithNodeGeneration(generation))
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

type infiniteAReader struct{}

func (*infiniteAReader) Read(destination []byte) (int, error) {
	for index := range destination {
		destination[index] = 'a'
	}
	return len(destination), nil
}
