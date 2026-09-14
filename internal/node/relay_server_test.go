package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/brezel/internal/backend"
	"github.com/infercrane/brezel/internal/domain"
	"github.com/infercrane/brezel/internal/nodeledger"
)

const (
	relayServerIssuer   = "brezel-api"
	relayServerKeyID    = "api-key-a"
	relayServerAudience = "brezel-node"
	relayServerNodeID   = "node-a"
	relayServerRouteID  = "route-a"
	relayServerProject  = "project-a"
	relayServerSandbox  = "sandbox-a"
	relayServerEngineID = "private-engine-credential-a"
)

var relayServerTime = time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

type relayServerHarness struct {
	t       *testing.T
	ledger  *nodeledger.Ledger
	signer  *CapabilitySigner
	server  *RelayServer
	engine  *relayServerEngine
	now     time.Time
	route   nodeledger.Route
	private string
}

func newRelayServerHarness(t *testing.T) *relayServerHarness {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewCapabilitySigner(relayServerIssuer, relayServerKeyID, private)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewCapabilityVerifier(relayServerIssuer, map[string]ed25519.PublicKey{relayServerKeyID: public})
	if err != nil {
		t.Fatal(err)
	}
	now := relayServerTime
	signer.now = func() time.Time { return now }
	verifier.now = func() time.Time { return now }
	replay, err := NewReplayCache(128)
	if err != nil {
		t.Fatal(err)
	}
	replay.now = func() time.Time { return now }
	ledgerDirectory := t.TempDir() + "/node"
	if err := os.Mkdir(ledgerDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	ledger, err := nodeledger.Open(ledgerDirectory + "/ledger.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	binding, err := ledger.Bind(relayServerRouteID, relayServerProject, relayServerSandbox, relayServerEngineID, now)
	if err != nil {
		t.Fatal(err)
	}
	binding, err = ledger.Transition(relayServerRouteID, binding.Public().Generation, nodeledger.StateAttaching, nodeledger.StateReady)
	if err != nil {
		t.Fatal(err)
	}
	engine := &relayServerEngine{
		fileData:   []byte("file-data"),
		fileType:   "text/plain; charset=utf-8",
		portStatus: http.StatusCreated,
		portHeader: http.Header{
			"Content-Type":             []string{"application/json"},
			"X-App-Response":           []string{"yes"},
			"X-Access-Token":           []string{"must-not-leak"},
			"E2b-Traffic-Access-Token": []string{"must-not-leak"},
			"Set-Cookie":               []string{"secret=value"},
			"Connection":               []string{"keep-alive"},
			RelayRouteHeader:           []string{"must-not-leak"},
		},
		portBody: []byte(`{"ok":true}`),
	}
	server, err := NewRelayServer(RelayServerConfig{
		NodeID: relayServerNodeID, Audience: relayServerAudience,
		Ledger: ledger, Verifier: verifier, Replay: replay, Engine: engine,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return &relayServerHarness{t: t, ledger: ledger, signer: signer, server: server, engine: engine, now: now, route: binding.Public(), private: relayServerEngineID}
}

func (h *relayServerHarness) issue(operation CapabilityOperation, canonical []byte, bounds CapabilityBounds) string {
	h.t.Helper()
	digest, err := RequestDigest(operation, canonical)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.issueFor(h.route, operation, digest, bounds)
}

func (h *relayServerHarness) issueFor(route nodeledger.Route, operation CapabilityOperation, digest string, bounds CapabilityBounds) string {
	h.t.Helper()
	token, _, err := h.signer.Issue(CapabilityIssue{
		Audience: relayServerAudience, NodeID: relayServerNodeID, BootEpoch: h.server.bootEpoch,
		RouteID: route.RouteID, Generation: route.Generation, ProjectID: route.ProjectID, SandboxID: route.SandboxID,
		Operation: operation, RequestDigest: digest, Bounds: bounds, TTL: 10 * time.Second,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return token
}

func (h *relayServerHarness) authorize(request *http.Request, token string) {
	h.t.Helper()
	request.Header.Set(RelayRouteHeader, relayServerRouteID)
	request.Header.Set("Authorization", RelayCapabilityScheme+" "+token)
}

func (h *relayServerHarness) execute(request *http.Request) *httptest.ResponseRecorder {
	h.t.Helper()
	recorder := httptest.NewRecorder()
	h.server.ServeHTTP(recorder, request)
	return recorder
}

func TestRelayServerHealthReadinessAndRouteResolution(t *testing.T) {
	h := newRelayServerHarness(t)
	for _, endpoint := range []string{"/healthz", "/readyz"} {
		response := h.execute(httptest.NewRequest(http.MethodGet, endpoint, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", endpoint, response.Code, response.Body)
		}
	}
	response := h.execute(httptest.NewRequest(http.MethodGet, "/v1/routes/"+relayServerRouteID, nil))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), relayServerEngineID) {
		t.Fatalf("route response status=%d body=%q", response.Code, response.Body.String())
	}
	var route relayRouteResponse
	if err := json.Unmarshal(response.Body.Bytes(), &route); err != nil || route.Route != h.route {
		t.Fatalf("route response=%#v error=%v", route, err)
	}
	response = h.execute(httptest.NewRequest(http.MethodGet, "/v1/routes/missing", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing route status=%d", response.Code)
	}
	response = h.execute(httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unexpected health method status=%d", response.Code)
	}
	h.engine.readyErr = errors.New("engine unavailable")
	response = h.execute(httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), h.engine.readyErr.Error()) {
		t.Fatalf("unready response status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestRelayServerDrainRejectsNewOperationsWithoutReadingBodies(t *testing.T) {
	h := newRelayServerHarness(t)
	h.server.BeginDrain()
	h.server.BeginDrain()
	if !h.server.Draining() {
		t.Fatal("relay did not enter draining state")
	}
	if response := h.execute(httptest.NewRequest(http.MethodGet, "/healthz", nil)); response.Code != http.StatusOK {
		t.Fatalf("health status=%d", response.Code)
	}
	if response := h.execute(httptest.NewRequest(http.MethodGet, "/readyz", nil)); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("readiness status=%d", response.Code)
	}
	body := &failOnReadBody{}
	request := httptest.NewRequest(http.MethodPost, "/v1/commands", body)
	request.Header.Set("Content-Type", "application/json")
	request.ContentLength = 1
	if response := h.execute(request); response.Code != http.StatusServiceUnavailable || body.read || h.engine.commandCalls != 0 {
		t.Fatalf("operation status=%d bodyRead=%v calls=%d", response.Code, body.read, h.engine.commandCalls)
	}
}

func TestRelayServerAdmissionRejectsOverloadBeforeReadingBody(t *testing.T) {
	h := newRelayServerHarness(t)
	h.server.admission = make(chan struct{}, 1)
	started := make(chan struct{})
	release := make(chan struct{})
	h.engine.commandStarted = started
	h.engine.commandRelease = release
	wire := relayCommandRequest{Argv: []string{"sleep", "1"}}
	canonical, _ := canonicalRelayJSON(wire)
	first := httptest.NewRequest(http.MethodPost, "/v1/commands", bytes.NewReader(canonical))
	first.Header.Set("Content-Type", "application/json")
	h.authorize(first, h.issue(CapabilityRunCommand, canonical, CapabilityBounds{MaxDurationMillis: 5_000, MaxResponseBytes: 1 << 20}))
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- h.execute(first) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first operation did not enter the engine")
	}

	body := &failOnReadBody{}
	second := httptest.NewRequest(http.MethodPut, "/v1/files?path=%2Fworkspace%2Fa", body)
	second.ContentLength = 1
	if response := h.execute(second); response.Code != http.StatusServiceUnavailable || body.read || h.engine.writeCalls != 0 {
		t.Fatalf("overload status=%d bodyRead=%v writes=%d", response.Code, body.read, h.engine.writeCalls)
	}
	close(release)
	select {
	case response := <-done:
		if response.Code != http.StatusOK {
			t.Fatalf("first operation status=%d", response.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first operation did not finish")
	}
}

func TestRelayServerRejectsUnsafeAdmissionCapacity(t *testing.T) {
	h := newRelayServerHarness(t)
	for _, value := range []int{-1, relayMaxInFlight + 1} {
		if _, err := NewRelayServer(RelayServerConfig{
			NodeID: relayServerNodeID, Audience: relayServerAudience, Ledger: h.ledger,
			Verifier: h.server.verifier, Replay: h.server.replay, Engine: h.engine,
			MaxInFlight: value,
		}); err == nil {
			t.Fatalf("max in-flight %d was accepted", value)
		}
	}
}

func TestRelayServerRotatesBootEpochAndRejectsPreRestartCapability(t *testing.T) {
	h := newRelayServerHarness(t)
	wire := relayCommandRequest{Argv: []string{"true"}}
	canonical, _ := canonicalRelayJSON(wire)
	token := h.issue(CapabilityRunCommand, canonical, CapabilityBounds{MaxDurationMillis: 1_000, MaxResponseBytes: 1 << 20})
	replay, err := NewReplayCache(16)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := NewRelayServer(RelayServerConfig{
		NodeID: relayServerNodeID, Audience: relayServerAudience, Ledger: h.ledger,
		Verifier: h.server.verifier, Replay: replay, Engine: h.engine, Now: func() time.Time { return h.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.bootEpoch == h.server.bootEpoch {
		t.Fatal("relay restart reused its boot epoch")
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/commands", bytes.NewReader(canonical))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(RelayRouteHeader, relayServerRouteID)
	request.Header.Set("Authorization", RelayCapabilityScheme+" "+token)
	response := httptest.NewRecorder()
	replacement.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || h.engine.commandCalls != 0 {
		t.Fatalf("restart replay status=%d calls=%d", response.Code, h.engine.commandCalls)
	}
}

func TestRelayServerOperationLeaseBlocksReplacementUntilCommandFinishes(t *testing.T) {
	h := newRelayServerHarness(t)
	started := make(chan struct{})
	release := make(chan struct{})
	h.engine.commandStarted = started
	h.engine.commandRelease = release
	wire := relayCommandRequest{Argv: []string{"sleep", "1"}}
	canonical, _ := canonicalRelayJSON(wire)
	request := httptest.NewRequest(http.MethodPost, "/v1/commands", bytes.NewReader(canonical))
	request.Header.Set("Content-Type", "application/json")
	h.authorize(request, h.issue(CapabilityRunCommand, canonical, CapabilityBounds{MaxDurationMillis: 5_000, MaxResponseBytes: 1 << 20}))
	responseReady := make(chan *httptest.ResponseRecorder, 1)
	go func() { responseReady <- h.execute(request) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("command did not enter the engine")
	}
	draining, err := h.ledger.Transition(relayServerRouteID, h.route.Generation, nodeledger.StateReady, nodeledger.StateDraining)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.ledger.Transition(relayServerRouteID, draining.Public().Generation, nodeledger.StateDraining, nodeledger.StateStandby); !errors.Is(err, nodeledger.ErrActiveOperations) {
		t.Fatalf("standby with active command error=%v", err)
	}
	close(release)
	select {
	case response := <-responseReady:
		if response.Code != http.StatusOK {
			t.Fatalf("command response status=%d body=%q", response.Code, response.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("command did not finish")
	}
	if _, err := h.ledger.Transition(relayServerRouteID, draining.Public().Generation, nodeledger.StateDraining, nodeledger.StateStandby); err != nil {
		t.Fatalf("standby after command: %v", err)
	}
}

func TestRelayServerRunsCommandWithPrivateBindingAndNDJSON(t *testing.T) {
	h := newRelayServerHarness(t)
	wire := relayCommandRequest{Argv: []string{"sh", "-lc", "printf ok"}, Cwd: "/workspace", Env: map[string]string{"MODE": "test"}}
	canonical, err := canonicalRelayJSON(wire)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/commands", bytes.NewReader(canonical))
	request.Header.Set("Content-Type", "application/json")
	h.authorize(request, h.issue(CapabilityRunCommand, canonical, CapabilityBounds{MaxDurationMillis: 5_000, MaxResponseBytes: 1 << 20}))
	response := h.execute(request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("command response status=%d content-type=%q body=%q", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	decoder := json.NewDecoder(response.Body)
	var events []backend.CommandEvent
	for {
		var event backend.CommandEvent
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if len(events) != 3 || events[0].Type != backend.CommandStarted || string(events[1].Data) != "ok\n" || events[2].Type != backend.CommandExited {
		t.Fatalf("command events=%#v", events)
	}
	if h.engine.commandCalls != 1 || h.engine.lastEngineID != h.private || h.engine.lastCommand.Cwd != wire.Cwd || h.engine.lastCommand.Env["MODE"] != "test" {
		t.Fatalf("engine command call=%d engine=%q request=%#v", h.engine.commandCalls, h.engine.lastEngineID, h.engine.lastCommand)
	}
}

func TestRelayServerFileWriteAndRead(t *testing.T) {
	h := newRelayServerHarness(t)
	data := []byte("uploaded-data")
	descriptor := fileWriteDescriptor("/workspace/data.txt", data)
	canonical, _ := canonicalRelayJSON(descriptor)
	writeRequest := httptest.NewRequest(http.MethodPut, "/v1/files?path=%2Fworkspace%2Fdata.txt", bytes.NewReader(data))
	writeRequest.Header.Set(RelayContentSHAHeader, descriptor.SHA256)
	h.authorize(writeRequest, h.issue(CapabilityWriteFile, canonical, CapabilityBounds{MaxRequestBytes: int64(len(data))}))
	writeResponse := h.execute(writeRequest)
	if writeResponse.Code != http.StatusOK {
		t.Fatalf("write status=%d body=%q", writeResponse.Code, writeResponse.Body.String())
	}
	var info backend.FileInfo
	if err := json.Unmarshal(writeResponse.Body.Bytes(), &info); err != nil || info.Path != descriptor.Path || info.Size != int64(len(data)) {
		t.Fatalf("write info=%#v error=%v", info, err)
	}
	if h.engine.writeCalls != 1 || h.engine.lastEngineID != h.private || h.engine.lastPath != descriptor.Path || !bytes.Equal(h.engine.writtenData, data) {
		t.Fatalf("write engine=%q path=%q data=%q calls=%d", h.engine.lastEngineID, h.engine.lastPath, h.engine.writtenData, h.engine.writeCalls)
	}

	readDescriptor := relayFileDescriptor{Path: descriptor.Path}
	readCanonical, _ := canonicalRelayJSON(readDescriptor)
	readRequest := httptest.NewRequest(http.MethodGet, "/v1/files?path=%2Fworkspace%2Fdata.txt", nil)
	h.authorize(readRequest, h.issue(CapabilityReadFile, readCanonical, CapabilityBounds{MaxResponseBytes: 1 << 20}))
	readResponse := h.execute(readRequest)
	if readResponse.Code != http.StatusOK || !bytes.Equal(readResponse.Body.Bytes(), h.engine.fileData) {
		t.Fatalf("read status=%d body=%q", readResponse.Code, readResponse.Body.Bytes())
	}
	expectedDigest := sha256.Sum256(h.engine.fileData)
	if readResponse.Header().Get(RelayFilePathHeader) != descriptor.Path || readResponse.Header().Get(RelayFileSizeHeader) != "9" ||
		readResponse.Header().Get(RelayContentSHAHeader) != base64.RawURLEncoding.EncodeToString(expectedDigest[:]) ||
		readResponse.Header().Get("Content-Type") != h.engine.fileType {
		t.Fatalf("read headers=%v", readResponse.Header())
	}
	if h.engine.readCalls != 1 || h.engine.lastEngineID != h.private || h.engine.lastPath != descriptor.Path {
		t.Fatalf("read engine=%q path=%q calls=%d", h.engine.lastEngineID, h.engine.lastPath, h.engine.readCalls)
	}
}

func TestRelayServerProxiesRawPortRequestAndSanitizesResponse(t *testing.T) {
	h := newRelayServerHarness(t)
	body := []byte(`{"request":true}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/ports/3000/api%2Fitems?q=hello%20world", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-App-Request", "yes")
	request.Header.Set("User-Agent", "brezel-test")
	request.Header.Set("Accept-Encoding", "identity")
	protocol := portProtocolRequest(request.Method, "/api%2Fitems", request.URL.RawQuery, request.Header, body)
	canonical, _ := canonicalRelayJSON(protocol)
	h.authorize(request, h.issue(CapabilityProxyPort, canonical, CapabilityBounds{
		Port: 3000, MaxDurationMillis: 5_000, MaxRequestBytes: int64(len(body)), MaxResponseBytes: 1 << 20,
	}))
	response := h.execute(request)
	if response.Code != http.StatusCreated || response.Body.String() != string(h.engine.portBody) || response.Header().Get("X-App-Response") != "yes" {
		t.Fatalf("proxy status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
	for _, forbidden := range []string{"X-Access-Token", "E2b-Traffic-Access-Token", "Set-Cookie", "Connection", RelayRouteHeader} {
		if value := response.Header().Get(forbidden); value != "" {
			t.Fatalf("response leaked %s=%q", forbidden, value)
		}
	}
	if h.engine.portCalls != 1 || h.engine.lastEngineID != h.private || h.engine.lastPort != 3000 || h.engine.lastPortRequest.Method != http.MethodPost ||
		h.engine.lastPortRequest.URL.EscapedPath() != "/api%2Fitems" || h.engine.lastPortRequest.URL.RawQuery != "q=hello%20world" ||
		!bytes.Equal(h.engine.portRequestBody, body) || h.engine.lastPortRequest.Header.Get("X-App-Request") != "yes" ||
		h.engine.lastPortRequest.Header.Get("Authorization") != "" || h.engine.lastPortRequest.Header.Get(RelayRouteHeader) != "" {
		t.Fatalf("proxied engine=%q port=%d request=%#v body=%q", h.engine.lastEngineID, h.engine.lastPort, h.engine.lastPortRequest, h.engine.portRequestBody)
	}
}

func TestRelayServerRejectsInvalidAuthorizationTamperAndReplay(t *testing.T) {
	command := relayCommandRequest{Argv: []string{"true"}}
	canonical, _ := canonicalRelayJSON(command)
	bounds := CapabilityBounds{MaxDurationMillis: 1_000, MaxResponseBytes: 1 << 20}

	t.Run("missing authorization", func(t *testing.T) {
		h := newRelayServerHarness(t)
		request := httptest.NewRequest(http.MethodPost, "/v1/commands", bytes.NewReader(canonical))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(RelayRouteHeader, relayServerRouteID)
		if response := h.execute(request); response.Code != http.StatusUnauthorized || h.engine.commandCalls != 0 {
			t.Fatalf("status=%d calls=%d", response.Code, h.engine.commandCalls)
		}
	})

	t.Run("ambiguous authorization", func(t *testing.T) {
		h := newRelayServerHarness(t)
		token := h.issue(CapabilityRunCommand, canonical, bounds)
		request := httptest.NewRequest(http.MethodPost, "/v1/commands", bytes.NewReader(canonical))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(RelayRouteHeader, relayServerRouteID)
		request.Header.Add("Authorization", RelayCapabilityScheme+" "+token)
		request.Header.Add("Authorization", RelayCapabilityScheme+" "+token)
		if response := h.execute(request); response.Code != http.StatusUnauthorized || h.engine.commandCalls != 0 {
			t.Fatalf("status=%d calls=%d", response.Code, h.engine.commandCalls)
		}
	})

	t.Run("missing route", func(t *testing.T) {
		h := newRelayServerHarness(t)
		request := httptest.NewRequest(http.MethodPost, "/v1/commands", bytes.NewReader(canonical))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", RelayCapabilityScheme+" "+h.issue(CapabilityRunCommand, canonical, bounds))
		if response := h.execute(request); response.Code != http.StatusUnauthorized || h.engine.commandCalls != 0 {
			t.Fatalf("status=%d calls=%d", response.Code, h.engine.commandCalls)
		}
	})

	t.Run("unknown route", func(t *testing.T) {
		h := newRelayServerHarness(t)
		request := httptest.NewRequest(http.MethodPost, "/v1/commands", bytes.NewReader(canonical))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(RelayRouteHeader, "route-missing")
		request.Header.Set("Authorization", RelayCapabilityScheme+" unused")
		if response := h.execute(request); response.Code != http.StatusForbidden || h.engine.commandCalls != 0 {
			t.Fatalf("status=%d calls=%d", response.Code, h.engine.commandCalls)
		}
	})

	t.Run("tampered token", func(t *testing.T) {
		h := newRelayServerHarness(t)
		token := h.issue(CapabilityRunCommand, canonical, bounds)
		last := byte('A')
		if token[len(token)-1] == last {
			last = 'B'
		}
		token = token[:len(token)-1] + string(last)
		request := httptest.NewRequest(http.MethodPost, "/v1/commands", bytes.NewReader(canonical))
		request.Header.Set("Content-Type", "application/json")
		h.authorize(request, token)
		response := h.execute(request)
		if response.Code != http.StatusForbidden || h.engine.commandCalls != 0 || strings.Contains(response.Body.String(), token) || strings.Contains(response.Body.String(), relayServerEngineID) {
			t.Fatalf("status=%d calls=%d body=%q", response.Code, h.engine.commandCalls, response.Body.String())
		}
	})

	t.Run("request digest mismatch", func(t *testing.T) {
		h := newRelayServerHarness(t)
		request := httptest.NewRequest(http.MethodPost, "/v1/commands", strings.NewReader(`{"argv":["false"]}`))
		request.Header.Set("Content-Type", "application/json")
		h.authorize(request, h.issue(CapabilityRunCommand, canonical, bounds))
		if response := h.execute(request); response.Code != http.StatusForbidden || h.engine.commandCalls != 0 {
			t.Fatalf("status=%d calls=%d", response.Code, h.engine.commandCalls)
		}
	})

	t.Run("wrong operation", func(t *testing.T) {
		h := newRelayServerHarness(t)
		request := httptest.NewRequest(http.MethodPost, "/v1/commands", bytes.NewReader(canonical))
		request.Header.Set("Content-Type", "application/json")
		h.authorize(request, h.issue(CapabilityReadFile, canonical, CapabilityBounds{MaxResponseBytes: 1 << 20}))
		if response := h.execute(request); response.Code != http.StatusForbidden || h.engine.commandCalls != 0 {
			t.Fatalf("status=%d calls=%d", response.Code, h.engine.commandCalls)
		}
	})

	t.Run("wrong project", func(t *testing.T) {
		h := newRelayServerHarness(t)
		digest, _ := RequestDigest(CapabilityRunCommand, canonical)
		wrong := h.route
		wrong.ProjectID = "project-b"
		request := httptest.NewRequest(http.MethodPost, "/v1/commands", bytes.NewReader(canonical))
		request.Header.Set("Content-Type", "application/json")
		h.authorize(request, h.issueFor(wrong, CapabilityRunCommand, digest, bounds))
		if response := h.execute(request); response.Code != http.StatusNotFound || h.engine.commandCalls != 0 {
			t.Fatalf("status=%d calls=%d", response.Code, h.engine.commandCalls)
		}
	})

	t.Run("replay", func(t *testing.T) {
		h := newRelayServerHarness(t)
		token := h.issue(CapabilityRunCommand, canonical, bounds)
		for index, wantStatus := range []int{http.StatusOK, http.StatusForbidden} {
			request := httptest.NewRequest(http.MethodPost, "/v1/commands", bytes.NewReader(canonical))
			request.Header.Set("Content-Type", "application/json")
			h.authorize(request, token)
			if response := h.execute(request); response.Code != wantStatus {
				t.Fatalf("request %d status=%d body=%q", index, response.Code, response.Body.String())
			}
		}
		if h.engine.commandCalls != 1 {
			t.Fatalf("engine calls=%d", h.engine.commandCalls)
		}
	})
}

func TestRelayServerRejectsStaleGenerationAndNonReadyRoute(t *testing.T) {
	command := relayCommandRequest{Argv: []string{"true"}}
	canonical, _ := canonicalRelayJSON(command)
	digest, _ := RequestDigest(CapabilityRunCommand, canonical)
	bounds := CapabilityBounds{MaxDurationMillis: 1_000, MaxResponseBytes: 1 << 20}

	t.Run("stale generation", func(t *testing.T) {
		h := newRelayServerHarness(t)
		oldToken := h.issueFor(h.route, CapabilityRunCommand, digest, bounds)
		binding, err := h.ledger.Transition(relayServerRouteID, h.route.Generation, nodeledger.StateReady, nodeledger.StateDraining)
		if err != nil {
			t.Fatal(err)
		}
		binding, err = h.ledger.Transition(relayServerRouteID, binding.Public().Generation, nodeledger.StateDraining, nodeledger.StateStandby)
		if err != nil {
			t.Fatal(err)
		}
		binding, err = h.ledger.Rebind(relayServerRouteID, binding.Public().Generation, nodeledger.StateStandby, "private-engine-credential-b", h.now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.ledger.Transition(relayServerRouteID, binding.Public().Generation, nodeledger.StateAttaching, nodeledger.StateReady); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/commands", bytes.NewReader(canonical))
		request.Header.Set("Content-Type", "application/json")
		h.authorize(request, oldToken)
		if response := h.execute(request); response.Code != http.StatusConflict || h.engine.commandCalls != 0 {
			t.Fatalf("status=%d calls=%d body=%q", response.Code, h.engine.commandCalls, response.Body.String())
		}
	})

	t.Run("non-ready state", func(t *testing.T) {
		h := newRelayServerHarness(t)
		if _, err := h.ledger.Transition(relayServerRouteID, h.route.Generation, nodeledger.StateReady, nodeledger.StateDraining); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/commands", bytes.NewReader(canonical))
		request.Header.Set("Content-Type", "application/json")
		h.authorize(request, h.issueFor(h.route, CapabilityRunCommand, digest, bounds))
		if response := h.execute(request); response.Code != http.StatusConflict || h.engine.commandCalls != 0 {
			t.Fatalf("status=%d calls=%d", response.Code, h.engine.commandCalls)
		}
	})
}

func TestRelayServerEnforcesOperationAndServerBounds(t *testing.T) {
	t.Run("file request exceeds capability", func(t *testing.T) {
		h := newRelayServerHarness(t)
		data := []byte("two")
		canonical, _ := canonicalRelayJSON(fileWriteDescriptor("/workspace/a", data))
		request := httptest.NewRequest(http.MethodPut, "/v1/files?path=%2Fworkspace%2Fa", bytes.NewReader(data))
		h.authorize(request, h.issue(CapabilityWriteFile, canonical, CapabilityBounds{MaxRequestBytes: 1}))
		if response := h.execute(request); response.Code != http.StatusRequestEntityTooLarge || h.engine.writeCalls != 0 {
			t.Fatalf("status=%d calls=%d", response.Code, h.engine.writeCalls)
		}
	})

	t.Run("file response exceeds capability", func(t *testing.T) {
		h := newRelayServerHarness(t)
		h.engine.fileData = []byte("two")
		canonical, _ := canonicalRelayJSON(relayFileDescriptor{Path: "/workspace/a"})
		request := httptest.NewRequest(http.MethodGet, "/v1/files?path=%2Fworkspace%2Fa", nil)
		h.authorize(request, h.issue(CapabilityReadFile, canonical, CapabilityBounds{MaxResponseBytes: 1}))
		if response := h.execute(request); response.Code != http.StatusBadGateway || response.Body.String() == "two" {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}
	})

	t.Run("file server response bound", func(t *testing.T) {
		h := newRelayServerHarness(t)
		canonical, _ := canonicalRelayJSON(relayFileDescriptor{Path: "/workspace/a"})
		request := httptest.NewRequest(http.MethodGet, "/v1/files?path=%2Fworkspace%2Fa", nil)
		h.authorize(request, h.issue(CapabilityReadFile, canonical, CapabilityBounds{MaxResponseBytes: relayMaxFileDownload + 1}))
		if response := h.execute(request); response.Code != http.StatusForbidden || h.engine.readCalls != 0 {
			t.Fatalf("status=%d calls=%d", response.Code, h.engine.readCalls)
		}
	})

	t.Run("proxy port bound", func(t *testing.T) {
		h := newRelayServerHarness(t)
		request := httptest.NewRequest(http.MethodGet, "/v1/ports/3000/", nil)
		protocol := portProtocolRequest(request.Method, "/", request.URL.RawQuery, request.Header, nil)
		canonical, _ := canonicalRelayJSON(protocol)
		h.authorize(request, h.issue(CapabilityProxyPort, canonical, CapabilityBounds{Port: 3001, MaxDurationMillis: 1_000, MaxRequestBytes: 1, MaxResponseBytes: 1 << 20}))
		if response := h.execute(request); response.Code != http.StatusForbidden || h.engine.portCalls != 0 {
			t.Fatalf("status=%d calls=%d", response.Code, h.engine.portCalls)
		}
	})

	t.Run("proxy response exceeds capability", func(t *testing.T) {
		h := newRelayServerHarness(t)
		h.engine.portBody = []byte("two")
		request := httptest.NewRequest(http.MethodGet, "/v1/ports/3000/", nil)
		protocol := portProtocolRequest(request.Method, "/", request.URL.RawQuery, request.Header, nil)
		canonical, _ := canonicalRelayJSON(protocol)
		h.authorize(request, h.issue(CapabilityProxyPort, canonical, CapabilityBounds{Port: 3000, MaxDurationMillis: 1_000, MaxRequestBytes: 1, MaxResponseBytes: 1}))
		if response := h.execute(request); response.Code != http.StatusBadGateway || response.Body.String() == "two" {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}
	})

	t.Run("command server duration bound", func(t *testing.T) {
		h := newRelayServerHarness(t)
		wire := relayCommandRequest{Argv: []string{"true"}}
		canonical, _ := canonicalRelayJSON(wire)
		request := httptest.NewRequest(http.MethodPost, "/v1/commands", bytes.NewReader(canonical))
		request.Header.Set("Content-Type", "application/json")
		h.authorize(request, h.issue(CapabilityRunCommand, canonical, CapabilityBounds{MaxDurationMillis: relayMaxCommandDuration.Milliseconds() + 1, MaxResponseBytes: 1 << 20}))
		if response := h.execute(request); response.Code != http.StatusForbidden || h.engine.commandCalls != 0 {
			t.Fatalf("status=%d calls=%d", response.Code, h.engine.commandCalls)
		}
	})

	t.Run("command output bound", func(t *testing.T) {
		h := newRelayServerHarness(t)
		h.engine.commandEvents = []backend.CommandEvent{
			{ExecutionID: "execution-a", Type: backend.CommandStarted, PID: 42},
			{ExecutionID: "execution-a", Type: backend.CommandStdout, Data: []byte("two")},
		}
		wire := relayCommandRequest{Argv: []string{"true"}}
		canonical, _ := canonicalRelayJSON(wire)
		request := httptest.NewRequest(http.MethodPost, "/v1/commands", bytes.NewReader(canonical))
		request.Header.Set("Content-Type", "application/json")
		h.authorize(request, h.issue(CapabilityRunCommand, canonical, CapabilityBounds{MaxDurationMillis: 1_000, MaxResponseBytes: 1}))
		response := h.execute(request)
		if response.Code != http.StatusOK || response.Header().Get("X-Brezel-Stream-Error") != "execution_failed" || strings.Contains(response.Body.String(), "two") {
			t.Fatalf("status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
		}
	})

	t.Run("declared body exceeds server request bound", func(t *testing.T) {
		h := newRelayServerHarness(t)
		request := httptest.NewRequest(http.MethodPut, "/v1/files?path=%2Fworkspace%2Fa", strings.NewReader("x"))
		request.ContentLength = relayMaxFileUpload + 1
		if response := h.execute(request); response.Code != http.StatusRequestEntityTooLarge || h.engine.writeCalls != 0 {
			t.Fatalf("status=%d calls=%d", response.Code, h.engine.writeCalls)
		}
	})
}

func TestRelayServerRejectsUnsafeRequestsBeforeEngine(t *testing.T) {
	tests := []struct {
		name    string
		request func() *http.Request
	}{
		{name: "command content type", request: func() *http.Request {
			return httptest.NewRequest(http.MethodPost, "/v1/commands", strings.NewReader(`{"argv":["true"]}`))
		}},
		{name: "file traversal", request: func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "/v1/files?path=%2Fworkspace%2F..%2Fetc", nil)
		}},
		{name: "proxy connect", request: func() *http.Request { return httptest.NewRequest(http.MethodConnect, "/v1/ports/3000/", nil) }},
		{name: "proxy credential header", request: func() *http.Request {
			request := httptest.NewRequest(http.MethodGet, "/v1/ports/3000/", nil)
			request.Header.Set("Cookie", "secret=value")
			return request
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newRelayServerHarness(t)
			response := h.execute(test.request())
			if response.Code != http.StatusBadRequest || h.engine.totalDataCalls() != 0 {
				t.Fatalf("status=%d calls=%d body=%q", response.Code, h.engine.totalDataCalls(), response.Body.String())
			}
		})
	}
}

func TestRelayServerDoesNotReadUnauthenticatedLargeBody(t *testing.T) {
	h := newRelayServerHarness(t)
	body := &failOnReadBody{}
	request := httptest.NewRequest(http.MethodPut, "/v1/files?path=%2Fworkspace%2Fa", body)
	request.ContentLength = 1024
	request.Header.Set(RelayRouteHeader, relayServerRouteID)
	response := h.execute(request)
	if response.Code != http.StatusUnauthorized || body.read {
		t.Fatalf("status=%d bodyRead=%v", response.Code, body.read)
	}
}

type relayServerEngine struct {
	readyErr        error
	commandErr      error
	commandEvents   []backend.CommandEvent
	fileData        []byte
	fileType        string
	portStatus      int
	portHeader      http.Header
	portBody        []byte
	commandCalls    int
	writeCalls      int
	readCalls       int
	portCalls       int
	lastEngineID    string
	lastCommand     backend.CommandRequest
	lastPath        string
	writtenData     []byte
	lastPort        uint16
	lastPortRequest *http.Request
	portRequestBody []byte
	commandStarted  chan struct{}
	commandRelease  <-chan struct{}
}

type failOnReadBody struct{ read bool }

func (b *failOnReadBody) Read([]byte) (int, error) {
	b.read = true
	return 0, errors.New("body must not be read")
}

func (*failOnReadBody) Close() error { return nil }

func (*relayServerEngine) Name() string { return backend.DefaultName }

func (*relayServerEngine) Capabilities() backend.Capabilities {
	return backend.Capabilities{CommandStreaming: true, FileReadWrite: true, AuthenticatedPorts: true}
}

func (*relayServerEngine) Create(context.Context, backend.CreateRequest) (backend.Sandbox, error) {
	return backend.Sandbox{}, backend.ErrCapabilityUnavailable
}

func (*relayServerEngine) Find(context.Context, string, string) (backend.Sandbox, error) {
	return backend.Sandbox{}, backend.ErrCapabilityUnavailable
}

func (*relayServerEngine) Inspect(context.Context, string) (backend.Sandbox, error) {
	return backend.Sandbox{}, backend.ErrCapabilityUnavailable
}

func (*relayServerEngine) Pause(context.Context, string, domain.CheckpointKind) error {
	return backend.ErrCapabilityUnavailable
}

func (*relayServerEngine) Resume(context.Context, string, domain.CheckpointKind, int64) (backend.Sandbox, error) {
	return backend.Sandbox{}, backend.ErrCapabilityUnavailable
}

func (*relayServerEngine) Delete(context.Context, string) error {
	return backend.ErrCapabilityUnavailable
}

func (*relayServerEngine) Checkpoint(context.Context, string, domain.CheckpointKind, string) (backend.Checkpoint, error) {
	return backend.Checkpoint{}, backend.ErrCapabilityUnavailable
}

func (*relayServerEngine) DeleteCheckpoint(context.Context, string) error {
	return backend.ErrCapabilityUnavailable
}

func (e *relayServerEngine) Ready(context.Context) error { return e.readyErr }

func (e *relayServerEngine) Run(_ context.Context, engineID string, request backend.CommandRequest, emit func(backend.CommandEvent) error) error {
	e.commandCalls++
	e.lastEngineID = engineID
	e.lastCommand = request
	if e.commandStarted != nil {
		close(e.commandStarted)
	}
	if e.commandRelease != nil {
		<-e.commandRelease
	}
	events := e.commandEvents
	if events == nil {
		events = []backend.CommandEvent{
			{ExecutionID: "execution-a", Type: backend.CommandStarted, PID: 42},
			{ExecutionID: "execution-a", Type: backend.CommandStdout, Data: []byte("ok\n")},
			{ExecutionID: "execution-a", Type: backend.CommandExited, Exited: true},
		}
	}
	for _, event := range events {
		if err := emit(event); err != nil {
			return err
		}
	}
	return e.commandErr
}

func (e *relayServerEngine) WriteFile(_ context.Context, engineID, path string, source io.Reader) (backend.FileInfo, error) {
	e.writeCalls++
	e.lastEngineID = engineID
	e.lastPath = path
	data, err := io.ReadAll(source)
	e.writtenData = append([]byte(nil), data...)
	return backend.FileInfo{Path: path, Size: int64(len(data)), ContentType: "application/octet-stream"}, err
}

func (e *relayServerEngine) ReadFile(_ context.Context, engineID, path string, destination io.Writer) (backend.FileInfo, error) {
	e.readCalls++
	e.lastEngineID = engineID
	e.lastPath = path
	written, err := destination.Write(e.fileData)
	return backend.FileInfo{Path: path, Size: int64(written), ContentType: e.fileType}, err
}

func (*relayServerEngine) ValidatePort(port uint16) error {
	if port == 0 {
		return errors.New("invalid port")
	}
	return nil
}

func (e *relayServerEngine) RoundTripPort(_ context.Context, engineID string, port uint16, request *http.Request) (*http.Response, error) {
	e.portCalls++
	e.lastEngineID = engineID
	e.lastPort = port
	e.lastPortRequest = request.Clone(request.Context())
	data, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	e.portRequestBody = append([]byte(nil), data...)
	return &http.Response{StatusCode: e.portStatus, Header: e.portHeader.Clone(), Body: io.NopCloser(bytes.NewReader(e.portBody))}, nil
}

func (e *relayServerEngine) totalDataCalls() int {
	return e.commandCalls + e.writeCalls + e.readCalls + e.portCalls
}

var _ backend.Backend = (*relayServerEngine)(nil)
var _ backend.ReadinessBackend = (*relayServerEngine)(nil)
var _ backend.GuestRuntime = (*relayServerEngine)(nil)
var _ backend.PortRuntime = (*relayServerEngine)(nil)
