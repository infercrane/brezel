package node

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/infercrane/brezel/internal/backend"
	"github.com/infercrane/brezel/internal/nodeledger"
)

const (
	relayMaxCommandRequest  = 128 << 10
	relayMaxCommandOutput   = 64 << 20
	relayMaxCommandDuration = time.Hour
	relayMaxFileUpload      = 32 << 20
	relayMaxFileDownload    = 64 << 20
	relayMaxFileDuration    = 5 * time.Minute
	relayMaxProxyRequest    = 32 << 20
	relayMaxProxyResponse   = 64 << 20
	relayMaxURLBytes        = 16 << 10
	relayReadyTimeout       = 2 * time.Second
)

var relayEnvironmentKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// RelayServerConfig binds one relay process to one node boot. The surrounding
// nodeidentity server is responsible for mTLS and exact peer verification.
type RelayServerConfig struct {
	NodeID   string
	Audience string
	Ledger   *nodeledger.Ledger
	Verifier *CapabilityVerifier
	Replay   *ReplayCache
	Engine   backend.Backend
	Now      func() time.Time
}

// RelayServer is the node-local command, file, and application-port data path.
// It deliberately has no logger: capability tokens, engine identities, and
// customer content must never enter ordinary process logs.
type RelayServer struct {
	nodeID      string
	bootEpoch   string
	audience    string
	ledger      *nodeledger.Ledger
	verifier    *CapabilityVerifier
	replay      *ReplayCache
	dataPlane   *BackendDataPlane
	engineReady backend.ReadinessBackend
	now         func() time.Time
	mux         *http.ServeMux
}

func NewRelayServer(config RelayServerConfig) (*RelayServer, error) {
	for name, value := range map[string]string{"node id": config.NodeID, "audience": config.Audience} {
		if err := validateIdentity(name, value); err != nil {
			return nil, err
		}
	}
	if config.Ledger == nil {
		return nil, errors.New("node generation ledger is required")
	}
	if config.Verifier == nil {
		return nil, errors.New("node capability verifier is required")
	}
	if config.Replay == nil {
		return nil, errors.New("node capability replay cache is required")
	}
	if config.Engine == nil {
		return nil, errors.New("node execution engine is required")
	}
	bootEpoch, err := newRelayBootEpoch()
	if err != nil {
		return nil, err
	}
	now := config.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	server := &RelayServer{
		nodeID:    config.NodeID,
		bootEpoch: bootEpoch,
		audience:  config.Audience,
		ledger:    config.Ledger,
		verifier:  config.Verifier,
		replay:    config.Replay,
		dataPlane: NewBackendDataPlane(config.Engine),
		now:       now,
		mux:       http.NewServeMux(),
	}
	server.engineReady, _ = config.Engine.(backend.ReadinessBackend)
	server.routes()
	return server, nil
}

func (s *RelayServer) routes() {
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("GET /readyz", s.ready)
	s.mux.HandleFunc("GET /v1/routes/{route}", s.route)
	s.mux.HandleFunc("POST /v1/commands", s.command)
	s.mux.HandleFunc("PUT /v1/files", s.writeFile)
	s.mux.HandleFunc("GET /v1/files", s.readFile)
	s.mux.HandleFunc("/v1/ports/{port}", s.proxyPort)
	s.mux.HandleFunc("/v1/ports/{port}/{path...}", s.proxyPort)
}

func (s *RelayServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.mux.ServeHTTP(w, r)
}

func (s *RelayServer) health(w http.ResponseWriter, _ *http.Request) {
	writeRelayJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *RelayServer) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.ledger.Ready(); err != nil {
		writeRelayError(w, http.StatusServiceUnavailable, "not_ready")
		return
	}
	capabilities := s.dataPlane.Capabilities()
	if !capabilities.CommandStreaming || !capabilities.FileReadWrite || !capabilities.AuthenticatedPorts {
		writeRelayError(w, http.StatusServiceUnavailable, "not_ready")
		return
	}
	if s.engineReady != nil {
		ctx, cancel := context.WithTimeout(r.Context(), relayReadyTimeout)
		defer cancel()
		if err := s.engineReady.Ready(ctx); err != nil {
			writeRelayError(w, http.StatusServiceUnavailable, "not_ready")
			return
		}
	}
	writeRelayJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *RelayServer) route(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || requestHasBody(r) {
		writeRelayError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	binding, err := s.ledger.Resolve(r.PathValue("route"))
	if err != nil {
		writeRelayLedgerError(w, err)
		return
	}
	writeRelayJSON(w, http.StatusOK, relayRouteResponse{NodeID: s.nodeID, BootEpoch: s.bootEpoch, Route: binding.Public()})
}

func newRelayBootEpoch() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate node relay boot epoch: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (s *RelayServer) command(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || !relayJSONContentType(r.Header.Get("Content-Type")) {
		writeRelayError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if r.ContentLength > relayMaxCommandRequest {
		writeRelayError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return
	}
	authorization, ok := s.authenticate(w, r, CapabilityRunCommand)
	if !ok {
		return
	}
	if authorization.claims.Bounds.MaxDurationMillis > relayMaxCommandDuration.Milliseconds() || authorization.claims.Bounds.MaxResponseBytes > relayMaxCommandOutput {
		writeRelayError(w, http.StatusForbidden, "bounds_exceeded")
		return
	}
	var wire relayCommandRequest
	if err := decodeRelayJSON(r.Body, relayMaxCommandRequest, &wire); err != nil || validateRelayCommand(wire) != nil {
		writeRelayError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	canonical, err := canonicalRelayJSON(wire)
	if err != nil {
		writeRelayError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	digest, err := RequestDigest(CapabilityRunCommand, canonical)
	if err != nil {
		writeRelayError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	binding, release, ok := s.authorize(w, authorization, digest)
	if !ok {
		return
	}
	defer release()
	defer binding.Revoke()
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(authorization.claims.Bounds.MaxDurationMillis)*time.Millisecond)
	defer cancel()

	wrote := false
	var outputBytes int64
	emit := func(event backend.CommandEvent) error {
		if event.Type == backend.CommandStdout || event.Type == backend.CommandStderr {
			outputBytes += int64(len(event.Data))
			if outputBytes > authorization.claims.Bounds.MaxResponseBytes || outputBytes > relayMaxCommandOutput {
				return errors.New("command output exceeded relay authorization")
			}
		}
		if !wrote {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.Header().Set("Trailer", "X-Brezel-Stream-Error")
			w.WriteHeader(http.StatusOK)
			wrote = true
		}
		if err := json.NewEncoder(w).Encode(event); err != nil {
			return err
		}
		if flusher, supported := w.(http.Flusher); supported {
			flusher.Flush()
		}
		return nil
	}
	err = s.dataPlane.Run(ctx, binding, backend.CommandRequest{Argv: append([]string(nil), wire.Argv...), Cwd: wire.Cwd, Env: cloneStringMap(wire.Env)}, emit)
	if err == nil {
		if !wrote {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
		}
		return
	}
	if !wrote {
		writeRelayError(w, http.StatusBadGateway, "execution_failed")
		return
	}
	w.Header().Set("X-Brezel-Stream-Error", "execution_failed")
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"code": "execution_failed"}})
}

func (s *RelayServer) writeFile(w http.ResponseWriter, r *http.Request) {
	path, ok := relayFilePath(r.URL)
	if !ok {
		writeRelayError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if r.ContentLength > relayMaxFileUpload {
		writeRelayError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return
	}
	authorization, ok := s.authenticate(w, r, CapabilityWriteFile)
	if !ok {
		return
	}
	requestLimit := authorization.claims.Bounds.MaxRequestBytes
	if requestLimit > relayMaxFileUpload {
		writeRelayError(w, http.StatusForbidden, "bounds_exceeded")
		return
	}
	if r.ContentLength > requestLimit {
		writeRelayError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return
	}
	data, err := readRelayBody(r.Body, requestLimit)
	if err != nil {
		writeRelayError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return
	}
	canonical, err := canonicalRelayJSON(fileWriteDescriptor(path, data))
	if err != nil {
		writeRelayError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	digest, _ := RequestDigest(CapabilityWriteFile, canonical)
	binding, release, authorized := s.authorize(w, authorization, digest)
	if !authorized {
		return
	}
	defer release()
	defer binding.Revoke()
	ctx, cancel := context.WithTimeout(r.Context(), relayMaxFileDuration)
	defer cancel()
	info, err := s.dataPlane.WriteFile(ctx, binding, path, bytes.NewReader(data))
	if err != nil {
		writeRelayError(w, http.StatusBadGateway, "execution_failed")
		return
	}
	info.Path = path
	info.Size = int64(len(data))
	writeRelayJSON(w, http.StatusOK, info)
}

func (s *RelayServer) readFile(w http.ResponseWriter, r *http.Request) {
	path, ok := relayFilePath(r.URL)
	if !ok || requestHasBody(r) {
		writeRelayError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	authorization, ok := s.authenticate(w, r, CapabilityReadFile)
	if !ok {
		return
	}
	if authorization.claims.Bounds.MaxResponseBytes > relayMaxFileDownload {
		writeRelayError(w, http.StatusForbidden, "bounds_exceeded")
		return
	}
	canonical, err := canonicalRelayJSON(relayFileDescriptor{Path: path})
	if err != nil {
		writeRelayError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	digest, _ := RequestDigest(CapabilityReadFile, canonical)
	binding, release, authorized := s.authorize(w, authorization, digest)
	if !authorized {
		return
	}
	defer release()
	defer binding.Revoke()
	ctx, cancel := context.WithTimeout(r.Context(), relayMaxFileDuration)
	defer cancel()
	var output bytes.Buffer
	limited := &relayBoundedWriter{destination: &output, remaining: authorization.claims.Bounds.MaxResponseBytes}
	info, err := s.dataPlane.ReadFile(ctx, binding, path, limited)
	if err != nil {
		writeRelayError(w, http.StatusBadGateway, "execution_failed")
		return
	}
	info.Path = path
	info.Size = int64(output.Len())
	w.Header().Set(RelayFilePathHeader, path)
	w.Header().Set(RelayFileSizeHeader, strconv.FormatInt(info.Size, 10))
	digestBytes := sha256.Sum256(output.Bytes())
	w.Header().Set(RelayContentSHAHeader, base64.RawURLEncoding.EncodeToString(digestBytes[:]))
	if validRelayContentType(info.ContentType) {
		w.Header().Set("Content-Type", info.ContentType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Content-Length", strconv.Itoa(output.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(output.Bytes())
}

func (s *RelayServer) proxyPort(w http.ResponseWriter, r *http.Request) {
	portValue, err := strconv.ParseUint(r.PathValue("port"), 10, 16)
	if err != nil || portValue == 0 || len(r.URL.RequestURI()) > relayMaxURLBytes || !validRelayProxyMethod(r.Method) {
		writeRelayError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	port := uint16(portValue)
	if disallowedRelayRequestHeaders(r.Header) {
		writeRelayError(w, http.StatusBadRequest, "disallowed_header")
		return
	}
	if r.ContentLength > relayMaxProxyRequest {
		writeRelayError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return
	}
	authorization, ok := s.authenticate(w, r, CapabilityProxyPort)
	if !ok {
		return
	}
	if authorization.claims.Bounds.Port != uint32(port) ||
		authorization.claims.Bounds.MaxDurationMillis > relayMaxCommandDuration.Milliseconds() ||
		authorization.claims.Bounds.MaxRequestBytes > relayMaxProxyRequest ||
		authorization.claims.Bounds.MaxResponseBytes > relayMaxProxyResponse {
		writeRelayError(w, http.StatusForbidden, "bounds_exceeded")
		return
	}
	if r.ContentLength > authorization.claims.Bounds.MaxRequestBytes {
		writeRelayError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return
	}
	body, err := readRelayBody(r.Body, authorization.claims.Bounds.MaxRequestBytes)
	if err != nil {
		writeRelayError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return
	}
	escapedPath := r.URL.EscapedPath()
	prefix := "/v1/ports/" + r.PathValue("port")
	if !strings.HasPrefix(escapedPath, prefix) {
		writeRelayError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	path := strings.TrimPrefix(escapedPath, prefix)
	if path == "" {
		path = "/"
	}
	if !validRelayURLPath(path) {
		writeRelayError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	protocolRequest := portProtocolRequest(r.Method, path, r.URL.RawQuery, r.Header, body)
	canonical, err := canonicalRelayJSON(protocolRequest)
	if err != nil {
		writeRelayError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	digest, _ := RequestDigest(CapabilityProxyPort, canonical)
	binding, release, authorized := s.authorize(w, authorization, digest)
	if !authorized {
		return
	}
	defer release()
	defer binding.Revoke()
	if err := s.dataPlane.ValidatePort(port); err != nil {
		writeRelayError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(authorization.claims.Bounds.MaxDurationMillis)*time.Millisecond)
	defer cancel()
	upstreamURL, err := url.ParseRequestURI(path)
	if err != nil || upstreamURL.RawQuery != "" || upstreamURL.Fragment != "" {
		writeRelayError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	upstreamURL.RawQuery = r.URL.RawQuery
	request := &http.Request{
		Method:        r.Method,
		URL:           upstreamURL,
		Header:        headerFromCanonical(protocolRequest.Header),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	response, err := s.dataPlane.RoundTripPort(ctx, binding, port, request)
	if err != nil || response == nil || response.Body == nil {
		writeRelayError(w, http.StatusBadGateway, "execution_failed")
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 599 {
		writeRelayError(w, http.StatusBadGateway, "execution_failed")
		return
	}
	responseBody, err := readRelayBody(response.Body, authorization.claims.Bounds.MaxResponseBytes)
	if err != nil {
		writeRelayError(w, http.StatusBadGateway, "response_too_large")
		return
	}
	copyRelayResponseHeaders(w.Header(), response.Header)
	w.Header().Set("Content-Length", strconv.Itoa(len(responseBody)))
	w.WriteHeader(response.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = w.Write(responseBody)
	}
}

type relayAuthorization struct {
	routeID string
	claims  CapabilityClaims
}

// authenticate performs all work that does not require request content before
// a handler reads or hashes a potentially large body. It deliberately avoids
// a ledger lookup until the bearer token is cryptographically authenticated.
func (s *RelayServer) authenticate(w http.ResponseWriter, r *http.Request, operation CapabilityOperation) (relayAuthorization, bool) {
	routeID, ok := relayRouteID(r.Header)
	if !ok {
		writeRelayError(w, http.StatusUnauthorized, "missing_route")
		return relayAuthorization{}, false
	}
	token, ok := relayCapability(r.Header)
	if !ok {
		writeRelayError(w, http.StatusUnauthorized, "invalid_authorization")
		return relayAuthorization{}, false
	}
	claims, err := s.verifier.Authenticate(token)
	if err != nil {
		writeRelayError(w, http.StatusForbidden, "capability_rejected")
		return relayAuthorization{}, false
	}
	if claims.Audience != s.audience || claims.NodeID != s.nodeID || claims.BootEpoch != s.bootEpoch || claims.RouteID != routeID || len(claims.Operations) != 1 || claims.Operations[0] != operation {
		writeRelayError(w, http.StatusForbidden, "capability_rejected")
		return relayAuthorization{}, false
	}
	return relayAuthorization{routeID: routeID, claims: claims}, true
}

// authorize binds authenticated claims to the exact current route, request
// digest, and an in-flight generation lease. The lease is acquired before the
// final decision so lifecycle cannot replace or release the engine while the
// admitted operation runs.
func (s *RelayServer) authorize(w http.ResponseWriter, authorization relayAuthorization, digest string) (SandboxBinding, func(), bool) {
	claims := authorization.claims
	current, lease, err := s.ledger.AcquireOperation(authorization.routeID, claims.ProjectID, claims.SandboxID, claims.Generation)
	if err != nil {
		writeRelayLedgerError(w, err)
		return SandboxBinding{}, nil, false
	}
	releaseLease := func() { _ = lease.Release() }
	public := current.Public()
	if public.Generation > math.MaxInt64 || current.EngineID() == "" {
		releaseLease()
		writeRelayError(w, http.StatusConflict, "route_changed")
		return SandboxBinding{}, nil, false
	}
	if err := s.verifier.Match(claims, CapabilityExpected{
		Audience: s.audience, NodeID: s.nodeID, BootEpoch: s.bootEpoch,
		RouteID: public.RouteID, Generation: public.Generation,
		ProjectID: public.ProjectID, SandboxID: public.SandboxID,
		Operation: claims.Operations[0], RequestDigest: digest,
	}); err != nil {
		releaseLease()
		writeRelayError(w, http.StatusForbidden, "capability_rejected")
		return SandboxBinding{}, nil, false
	}
	if err := s.replay.Use(claims.JTI, time.Unix(claims.ExpiresAt, 0).UTC()); err != nil {
		releaseLease()
		writeRelayError(w, http.StatusForbidden, "capability_rejected")
		return SandboxBinding{}, nil, false
	}
	engineID := current.EngineID()
	generation := current.Public().Generation
	var active atomic.Bool
	active.Store(true)
	release := func() {
		if active.CompareAndSwap(true, false) {
			releaseLease()
		}
	}
	verify := func(projectID, sandboxID, candidateEngineID string, revision int64) bool {
		if !active.Load() || revision < 1 || uint64(revision) != generation {
			return false
		}
		current, err := s.ledger.Resolve(public.RouteID)
		if err != nil {
			return false
		}
		currentPublic := current.Public()
		return currentPublic.ProjectID == projectID && currentPublic.SandboxID == sandboxID && currentPublic.Generation == generation &&
			(currentPublic.State == nodeledger.StateReady || currentPublic.State == nodeledger.StateDraining) &&
			current.EngineID() == engineID && candidateEngineID == engineID
	}
	binding, err := BindAuthorizedSandbox(public.ProjectID, public.SandboxID, engineID, "relay", int64(generation), time.Unix(claims.ExpiresAt, 0).UTC(), verify, WithBindingClock(s.now), WithNodeGeneration(generation))
	if err != nil {
		release()
		writeRelayError(w, http.StatusConflict, "route_changed")
		return SandboxBinding{}, nil, false
	}
	return binding, release, true
}

func relayCapability(header http.Header) (string, bool) {
	values := header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	prefix := RelayCapabilityScheme + " "
	if !strings.HasPrefix(values[0], prefix) {
		return "", false
	}
	token := strings.TrimPrefix(values[0], prefix)
	if token == "" || strings.ContainsAny(token, " \t\r\n,") {
		return "", false
	}
	return token, true
}

func relayRouteID(header http.Header) (string, bool) {
	values := header.Values(RelayRouteHeader)
	returnValue := ""
	if len(values) == 1 {
		returnValue = values[0]
	}
	return returnValue, returnValue != "" && strings.TrimSpace(returnValue) == returnValue && !strings.ContainsAny(returnValue, ",\x00\r\n")
}

func relayJSONContentType(value string) bool {
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || mediaType != "application/json" {
		return false
	}
	if charset, exists := parameters["charset"]; exists && !strings.EqualFold(charset, "utf-8") {
		return false
	}
	return len(parameters) <= 1
}

func relayFilePath(value *url.URL) (string, bool) {
	if value == nil {
		return "", false
	}
	query := value.Query()
	paths, exists := query["path"]
	if !exists || len(paths) != 1 || len(query) != 1 || !validRelayGuestPath(paths[0]) {
		return "", false
	}
	return paths[0], true
}

func validateRelayCommand(request relayCommandRequest) error {
	if len(request.Argv) == 0 || len(request.Argv) > 256 {
		return errors.New("invalid argv")
	}
	total := 0
	for _, value := range request.Argv {
		if value == "" || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return errors.New("invalid argv")
		}
		total += len(value)
	}
	if total > 32<<10 || (request.Cwd != "" && !validRelayGuestPath(request.Cwd)) || len(request.Env) > 128 {
		return errors.New("invalid command")
	}
	environmentBytes := 0
	for key, value := range request.Env {
		if !relayEnvironmentKey.MatchString(key) || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return errors.New("invalid environment")
		}
		environmentBytes += len(key) + len(value)
	}
	if environmentBytes > 64<<10 {
		return errors.New("invalid environment")
	}
	return nil
}

func validRelayGuestPath(value string) bool {
	return value != "" && len(value) <= 4096 && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n") && filepath.IsAbs(value) && filepath.Clean(value) == value
}

func validRelayURLPath(value string) bool {
	return value != "" && len(value) <= relayMaxURLBytes && strings.HasPrefix(value, "/") && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func validRelayProxyMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return true
	default:
		return false
	}
}

func disallowedRelayRequestHeaders(header http.Header) bool {
	for name := range header {
		canonical := http.CanonicalHeaderKey(name)
		if strings.EqualFold(canonical, "Authorization") || strings.EqualFold(canonical, RelayRouteHeader) {
			continue
		}
		if !relayHeaderAllowed(canonical) || internalCredentialHeader(canonical) {
			return true
		}
	}
	return false
}

func internalCredentialHeader(name string) bool {
	switch strings.ToLower(name) {
	case "x-access-token", "e2b-sandbox-id", "e2b-sandbox-port", "e2b-traffic-access-token", "set-cookie":
		return true
	default:
		return false
	}
}

func headerFromCanonical(source map[string][]string) http.Header {
	header := make(http.Header, len(source))
	for name, values := range source {
		if relayHeaderAllowed(name) && !internalCredentialHeader(name) {
			header[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
	}
	return header
}

func copyRelayResponseHeaders(destination, source http.Header) {
	for name, values := range canonicalPortHeaders(source) {
		if internalCredentialHeader(name) || strings.EqualFold(name, "Content-Length") {
			continue
		}
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

func readRelayBody(source io.Reader, maximum int64) ([]byte, error) {
	if source == nil || maximum < 0 {
		return nil, errors.New("invalid relay body")
	}
	data, err := io.ReadAll(io.LimitReader(source, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, errors.New("relay body exceeds limit")
	}
	return data, nil
}

func requestHasBody(request *http.Request) bool {
	return request.ContentLength > 0 || len(request.TransferEncoding) > 0
}

func validRelayContentType(value string) bool {
	if value == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n") {
		return false
	}
	_, _, err := mime.ParseMediaType(value)
	return err == nil
}

func writeRelayLedgerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, nodeledger.ErrNotFound):
		writeRelayError(w, http.StatusNotFound, "route_not_found")
	case errors.Is(err, nodeledger.ErrInvalid):
		writeRelayError(w, http.StatusBadRequest, "invalid_route")
	case errors.Is(err, nodeledger.ErrStaleGeneration), errors.Is(err, nodeledger.ErrStateConflict), errors.Is(err, nodeledger.ErrActiveOperations):
		writeRelayError(w, http.StatusConflict, "route_changed")
	default:
		writeRelayError(w, http.StatusServiceUnavailable, "not_ready")
	}
}

func writeRelayError(w http.ResponseWriter, status int, code string) {
	writeRelayJSON(w, status, map[string]any{"error": map[string]string{"code": code}})
}

func writeRelayJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type relayBoundedWriter struct {
	destination io.Writer
	remaining   int64
}

func (w *relayBoundedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, errors.New("relay response exceeds authorization")
	}
	written, err := w.destination.Write(data)
	w.remaining -= int64(written)
	return written, err
}
