package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/infercrane/brezel/internal/backend"
	"github.com/infercrane/brezel/internal/domain"
	"github.com/infercrane/brezel/internal/service"
	"github.com/infercrane/brezel/internal/telemetry"
)

const (
	maxBodyBytes        = 1 << 20
	maxPortRequestBytes = 32 << 20
	maxPortLeaseSeconds = 15 * 60
	maxPortLeases       = 4096
	maxSandboxLeases    = 16
	defaultMaxInFlight  = 512
)

type Server struct {
	service      *service.Service
	authorizer   Authorizer
	mux          *http.ServeMux
	connector    http.Handler
	leaseMu      sync.Mutex
	leases       map[string]portLease
	now          func() time.Time
	maxInFlight  int
	inFlight     chan struct{}
	logger       *log.Logger
	metrics      requestMetrics
	phaseMetrics *telemetry.Registry
}

// Authorizer authenticates an opaque bearer credential and independently
// authorizes the requested project. Implementations must not infer project
// identity from an untrusted request header alone.
type Authorizer interface {
	Authorize(token, projectID string) (authenticated bool, authorized bool)
}

type trustedOperatorToken string

func (token trustedOperatorToken) Authorize(candidate, _ string) (bool, bool) {
	matched := subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1
	return matched, matched
}

type portLease struct {
	ProjectID string
	SandboxID string
	Port      uint16
	ExpiresAt time.Time
}

type Option func(*Server)

func WithConnectorHandler(handler http.Handler) Option {
	return func(s *Server) { s.connector = handler }
}

// WithAuthorizer replaces the trusted-operator token behavior used by New.
// Production deployments should always supply a project-binding authorizer.
func WithAuthorizer(authorizer Authorizer) Option {
	return func(s *Server) { s.authorizer = authorizer }
}

// WithMaxInFlightRequests sets a process-wide admission ceiling for API,
// connector, and preview traffic. Health, readiness, and metrics remain
// available while ordinary requests are saturated.
func WithMaxInFlightRequests(limit int) Option {
	return func(s *Server) { s.maxInFlight = limit }
}

// WithRequestLogger enables content-free access logs. The middleware records
// only request ID, method, matched route pattern, status, and duration.
func WithRequestLogger(logger *log.Logger) Option {
	return func(s *Server) { s.logger = logger }
}

// WithPhaseMetrics appends fixed, content-free lifecycle and guest phase
// histograms to the existing Prometheus endpoint.
func WithPhaseMetrics(metrics *telemetry.Registry) Option {
	return func(s *Server) { s.phaseMetrics = metrics }
}

func New(svc *service.Service, token string, options ...Option) (*Server, error) {
	if svc == nil {
		return nil, errors.New("service is required")
	}
	if token != "" && len(token) < 32 {
		return nil, errors.New("service token must be at least 32 characters")
	}
	s := &Server{service: svc, mux: http.NewServeMux(), leases: make(map[string]portLease), now: func() time.Time { return time.Now().UTC() }, maxInFlight: defaultMaxInFlight}
	if token != "" {
		s.authorizer = trustedOperatorToken(token)
	}
	for _, option := range options {
		option(s)
	}
	if s.authorizer == nil {
		return nil, errors.New("authorizer is required")
	}
	if s.maxInFlight < 1 {
		return nil, errors.New("maximum in-flight requests must be positive")
	}
	s.inFlight = make(chan struct{}, s.maxInFlight)
	s.routes()
	return s, nil
}

func (s *Server) Handler() http.Handler {
	return s.securityHeaders(s.observe(s.authenticate(s.admit(s.mux))))
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := s.service.Ready(ctx); err != nil {
			writeError(w, http.StatusServiceUnavailable, "not_ready", "runtime dependencies are not ready")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	s.mux.HandleFunc("GET /metrics", s.serveMetrics)
	s.mux.HandleFunc("GET /v1/capabilities", s.capabilities)
	s.mux.HandleFunc("GET /v1/receipt-public-key", s.receiptPublicKey)
	s.mux.HandleFunc("POST /v1/environments", s.createEnvironment)
	s.mux.HandleFunc("GET /v1/environments/{revision}", s.getEnvironment)
	s.mux.HandleFunc("POST /v1/connectors", s.createConnector)
	s.mux.HandleFunc("GET /v1/connectors/{revision}", s.getConnector)
	s.mux.HandleFunc("POST /v1/workspaces", s.createWorkspace)
	s.mux.HandleFunc("GET /v1/workspaces", s.listWorkspaces)
	s.mux.HandleFunc("GET /v1/workspaces/{id}", s.getWorkspace)
	s.mux.HandleFunc("DELETE /v1/workspaces/{id}", s.deleteWorkspace)
	s.mux.HandleFunc("POST /v1/sandboxes", s.createSandbox)
	s.mux.HandleFunc("GET /v1/sandboxes", s.listSandboxes)
	s.mux.HandleFunc("GET /v1/sandboxes/{id}", s.getSandbox)
	s.mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.deleteSandbox)
	s.mux.HandleFunc("POST /v1/sandboxes/{id}/commands", s.runCommand)
	s.mux.HandleFunc("PUT /v1/sandboxes/{id}/files", s.writeFile)
	s.mux.HandleFunc("GET /v1/sandboxes/{id}/files", s.readFile)
	s.mux.HandleFunc("POST /v1/sandboxes/{id}/ports/{port}/leases", s.createPortLease)
	s.mux.HandleFunc("/p/{token}", s.proxyPort)
	s.mux.HandleFunc("/p/{token}/{path...}", s.proxyPort)
	s.mux.HandleFunc("POST /v1/sandboxes/{action}", s.sandboxAction)
	s.mux.HandleFunc("POST /v1/sandboxes/{id}/checkpoints", s.createCheckpoint)
	s.mux.HandleFunc("DELETE /v1/checkpoints/{id}", s.deleteCheckpoint)
	s.mux.HandleFunc("GET /v1/sandboxes/{id}/events", s.events)
	s.mux.HandleFunc("GET /v1/sandboxes/{id}/receipt", s.sandboxReceipt)
	s.mux.HandleFunc("GET /v1/operations/{id}", s.getOperation)
	if s.connector != nil {
		s.mux.Handle("/connector/", s.connector)
	}
}

func (s *Server) createPortLease(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TTLSeconds int64 `json:"ttl_seconds,omitempty"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if in.TTLSeconds == 0 {
		in.TTLSeconds = 5 * 60
	}
	if in.TTLSeconds < 30 || in.TTLSeconds > maxPortLeaseSeconds {
		writeError(w, http.StatusBadRequest, "invalid_request", "ttl_seconds must be between 30 and 900")
		return
	}
	parsed, err := strconv.ParseUint(r.PathValue("port"), 10, 16)
	if err != nil || parsed == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "port must be between 1 and 65535")
		return
	}
	port := uint16(parsed)
	if err := s.service.ValidatePortAccess(r.Context(), project(r), r.PathValue("id"), port); err != nil {
		writeServiceError(w, err)
		return
	}
	token, err := opaqueToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not create port lease")
		return
	}
	expiresAt := s.now().Add(time.Duration(in.TTLSeconds) * time.Second)
	s.leaseMu.Lock()
	s.removeExpiredLeasesLocked()
	if len(s.leases) >= maxPortLeases || s.sandboxLeaseCountLocked(project(r), r.PathValue("id")) >= maxSandboxLeases {
		s.leaseMu.Unlock()
		writeError(w, http.StatusTooManyRequests, "quota_exceeded", "active port lease limit reached")
		return
	}
	s.leases[token] = portLease{ProjectID: project(r), SandboxID: r.PathValue("id"), Port: port, ExpiresAt: expiresAt}
	s.leaseMu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"path": "/p/" + token + "/", "expires_at": expiresAt})
}

func (s *Server) proxyPort(w http.ResponseWriter, r *http.Request) {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "WebSocket previews are not available in this release")
		return
	}
	token := r.PathValue("token")
	s.leaseMu.Lock()
	lease, ok := s.leases[token]
	if ok && !s.now().Before(lease.ExpiresAt) {
		delete(s.leases, token)
		ok = false
	}
	s.leaseMu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "port lease not found or expired")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/p/"+token)
	if rest == "" {
		rest = "/"
	}
	outgoing := r.Clone(r.Context())
	outgoing.URL.Path = rest
	outgoing.URL.RawPath = ""
	outgoing.Header = r.Header.Clone()
	for _, header := range []string{"Cookie", "Forwarded", "Origin", "Referer", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
		outgoing.Header.Del(header)
	}
	outgoing.Body = http.MaxBytesReader(w, r.Body, maxPortRequestBytes)
	ctx, cancel := context.WithDeadline(r.Context(), lease.ExpiresAt)
	defer cancel()
	response, err := s.service.RoundTripPort(ctx, lease.ProjectID, lease.SandboxID, lease.Port, outgoing)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	defer response.Body.Close()
	copyResponseHeaders(w.Header(), response.Header)
	if location := w.Header().Get("Location"); strings.HasPrefix(location, "/") && !strings.HasPrefix(location, "//") {
		w.Header().Set("Location", "/p/"+token+location)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func (s *Server) sandboxLeaseCountLocked(projectID, sandboxID string) int {
	count := 0
	for _, lease := range s.leases {
		if lease.ProjectID == projectID && lease.SandboxID == sandboxID {
			count++
		}
	}
	return count
}

func (s *Server) removeExpiredLeasesLocked() {
	now := s.now()
	for token, lease := range s.leases {
		if !now.Before(lease.ExpiresAt) {
			delete(s.leases, token)
		}
	}
}

func opaqueToken() (string, error) {
	return opaqueTokenBytes(32)
}

func opaqueTokenBytes(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func copyResponseHeaders(destination, source http.Header) {
	blocked := map[string]bool{}
	for _, value := range source.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			blocked[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	for name, values := range source {
		lower := strings.ToLower(name)
		if blocked[lower] {
			continue
		}
		switch lower {
		case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "set-cookie", "te", "trailer", "transfer-encoding", "upgrade", "x-access-token", "e2b-traffic-access-token", "e2b-sandbox-id", "e2b-sandbox-port":
			continue
		}
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

func (s *Server) capabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"runtime":            backend.DefaultName,
		"implemented":        s.service.Capabilities(),
		"qualification":      "unverified",
		"qualification_note": "Run conformance on the exact deployment before making assurance or performance claims.",
	})
}

func (s *Server) receiptPublicKey(w http.ResponseWriter, _ *http.Request) {
	public, keyID, ok := s.service.ReceiptPublicKey()
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "receipt signing is not configured")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"algorithm": "Ed25519", "key_id": keyID, "public_key": base64.StdEncoding.EncodeToString(public)})
}

func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request) {
	var in service.CreateEnvironmentInput
	if !decodeBody(w, r, &in) {
		return
	}
	resource, operation, err := s.service.CreateEnvironment(project(r), r.Header.Get("Idempotency-Key"), in)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, mutationResponse{Resource: environmentViewOf(resource), Operation: operationViewOf(operation)})
}

func (s *Server) getEnvironment(w http.ResponseWriter, r *http.Request) {
	resource, err := s.service.GetEnvironment(project(r), r.PathValue("revision"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, environmentViewOf(resource))
}

func (s *Server) createConnector(w http.ResponseWriter, r *http.Request) {
	var in service.CreateConnectorInput
	if !decodeBody(w, r, &in) {
		return
	}
	resource, operation, err := s.service.CreateConnector(project(r), r.Header.Get("Idempotency-Key"), in)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, mutationResponse{Resource: connectorViewOf(resource), Operation: operationViewOf(operation)})
}

func (s *Server) getConnector(w http.ResponseWriter, r *http.Request) {
	resource, err := s.service.GetConnector(project(r), r.PathValue("revision"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, connectorViewOf(resource))
}

func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	var in service.CreateWorkspaceInput
	if !decodeBody(w, r, &in) {
		return
	}
	resource, operation, err := s.service.CreateWorkspace(r.Context(), project(r), r.Header.Get("Idempotency-Key"), in)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, mutationResponse{Resource: workspaceViewOf(resource), Operation: operationViewOf(operation)})
}

func (s *Server) getWorkspace(w http.ResponseWriter, r *http.Request) {
	resource, err := s.service.GetWorkspace(project(r), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workspaceViewOf(resource))
}

func (s *Server) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	includeTerminal, err := includeTerminal(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "include_terminal must be true or false")
		return
	}
	resources, err := s.service.ListWorkspaces(project(r), includeTerminal)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	views := make([]workspaceView, 0, len(resources))
	for _, resource := range resources {
		views = append(views, workspaceViewOf(resource))
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": views})
}

func (s *Server) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if !emptyBody(w, r) {
		return
	}
	resource, operation, err := s.service.DeleteWorkspace(r.Context(), project(r), r.PathValue("id"), r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, mutationResponse{Resource: workspaceViewOf(resource), Operation: operationViewOf(operation)})
}

func (s *Server) createSandbox(w http.ResponseWriter, r *http.Request) {
	var in service.CreateSandboxInput
	if !decodeBody(w, r, &in) {
		return
	}
	resource, operation, err := s.service.CreateSandbox(r.Context(), project(r), r.Header.Get("Idempotency-Key"), in)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, mutationResponse{Resource: sandboxViewOf(resource), Operation: operationViewOf(operation)})
}

func (s *Server) getSandbox(w http.ResponseWriter, r *http.Request) {
	resource, err := s.service.GetSandbox(r.Context(), project(r), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sandboxViewOf(resource))
}

func (s *Server) listSandboxes(w http.ResponseWriter, r *http.Request) {
	includeTerminal, err := includeTerminal(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "include_terminal must be true or false")
		return
	}
	resources, err := s.service.ListSandboxes(project(r), includeTerminal)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	views := make([]sandboxView, 0, len(resources))
	for _, resource := range resources {
		views = append(views, sandboxViewOf(resource))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": views})
}

func (s *Server) runCommand(w http.ResponseWriter, r *http.Request) {
	var in service.RunCommandInput
	if !decodeBody(w, r, &in) {
		return
	}
	wrote := false
	emit := func(event backend.CommandEvent) error {
		if !wrote {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.Header().Set("Trailer", "X-Brezel-Stream-Error")
			w.WriteHeader(http.StatusOK)
			wrote = true
		}
		if err := json.NewEncoder(w).Encode(commandEventViewFrom(event)); err != nil {
			return err
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return nil
	}
	executionID, err := s.service.RunCommand(r.Context(), project(r), r.PathValue("id"), in, emit)
	if err == nil {
		return
	}
	if !wrote {
		writeServiceError(w, err)
		return
	}
	w.Header().Set("X-Brezel-Stream-Error", "command_failed")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"execution_id": executionID,
		"type":         "error",
		"error":        map[string]string{"code": "command_failed", "message": "command stream ended before a confirmed exit"},
	})
}

// commandEventView keeps a successful zero exit status explicit on the public
// NDJSON wire without adding an irrelevant exit_code field to non-terminal
// events. A missing terminal status is ambiguous to streaming SDKs because it
// cannot be distinguished from a truncated event.
type commandEventView struct {
	ExecutionID string                   `json:"execution_id,omitempty"`
	Type        backend.CommandEventType `json:"type"`
	PID         uint32                   `json:"pid,omitempty"`
	Data        []byte                   `json:"data,omitempty"`
	ExitCode    *int32                   `json:"exit_code,omitempty"`
	Exited      bool                     `json:"exited,omitempty"`
	Status      string                   `json:"status,omitempty"`
	Error       string                   `json:"error,omitempty"`
}

func commandEventViewFrom(event backend.CommandEvent) commandEventView {
	view := commandEventView{
		ExecutionID: event.ExecutionID,
		Type:        event.Type,
		PID:         event.PID,
		Data:        event.Data,
		Exited:      event.Exited,
		Status:      event.Status,
		Error:       event.Error,
	}
	if event.Type == backend.CommandExited {
		exitCode := event.ExitCode
		view.ExitCode = &exitCode
	}
	return view
}

func (s *Server) writeFile(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > service.MaxFileUpload {
		writeError(w, http.StatusRequestEntityTooLarge, "file_too_large", "file exceeds upload limit")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, service.MaxFileUpload+1)
	info, err := s.service.WriteFile(r.Context(), project(r), r.PathValue("id"), r.URL.Query().Get("path"), r.Body)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) readFile(w http.ResponseWriter, r *http.Request) {
	result, err := s.service.ReadFile(r.Context(), project(r), r.PathValue("id"), r.URL.Query().Get("path"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	contentType := result.Info.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprint(len(result.Data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result.Data)
}

func (s *Server) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	resource, operation, err := s.service.Delete(r.Context(), project(r), r.PathValue("id"), r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, mutationResponse{Resource: sandboxViewOf(resource), Operation: operationViewOf(operation)})
}

func (s *Server) pauseSandbox(w http.ResponseWriter, r *http.Request) {
	if !emptyBody(w, r) {
		return
	}
	resource, operation, err := s.service.Pause(r.Context(), project(r), r.PathValue("id"), r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, mutationResponse{Resource: sandboxViewOf(resource), Operation: operationViewOf(operation)})
}

func (s *Server) sandboxAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if id, ok := strings.CutSuffix(action, ":pause"); ok && id != "" {
		r.SetPathValue("id", id)
		s.pauseSandbox(w, r)
		return
	}
	if id, ok := strings.CutSuffix(action, ":resume"); ok && id != "" {
		r.SetPathValue("id", id)
		s.resumeSandbox(w, r)
		return
	}
	writeError(w, http.StatusNotFound, "not_found", "resource not found")
}

func (s *Server) resumeSandbox(w http.ResponseWriter, r *http.Request) {
	if !emptyBody(w, r) {
		return
	}
	resource, operation, err := s.service.Resume(r.Context(), project(r), r.PathValue("id"), r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, mutationResponse{Resource: sandboxViewOf(resource), Operation: operationViewOf(operation)})
}

func (s *Server) createCheckpoint(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string                `json:"name"`
		Kind domain.CheckpointKind `json:"kind"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	resource, operation, err := s.service.Checkpoint(r.Context(), project(r), r.PathValue("id"), r.Header.Get("Idempotency-Key"), in.Name, in.Kind)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, mutationResponse{Resource: checkpointViewOf(resource), Operation: operationViewOf(operation)})
}

func (s *Server) deleteCheckpoint(w http.ResponseWriter, r *http.Request) {
	if !emptyBody(w, r) {
		return
	}
	operation, err := s.service.DeleteCheckpoint(r.Context(), project(r), r.PathValue("id"), r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"operation": operationViewOf(operation)})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	resources, err := s.service.Events(project(r), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	views := make([]eventView, 0, len(resources))
	for _, resource := range resources {
		views = append(views, eventViewOf(resource))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": views})
}

func (s *Server) sandboxReceipt(w http.ResponseWriter, r *http.Request) {
	envelope, err := s.service.Receipt(project(r), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, envelope)
}

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	operation, err := s.service.GetOperation(project(r), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, operationViewOf(operation))
}

type mutationResponse struct {
	Resource  any `json:"resource"`
	Operation any `json:"operation"`
}

type environmentView struct {
	RevisionID     string    `json:"revision_id"`
	Name           string    `json:"name"`
	Template       string    `json:"template"`
	ImageDigest    string    `json:"image_digest,omitempty"`
	PolicyRevision string    `json:"policy_revision,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

func environmentViewOf(value domain.Environment) environmentView {
	return environmentView{RevisionID: value.RevisionID, Name: value.Name, Template: value.BackendTemplate, ImageDigest: value.ImageDigest, PolicyRevision: value.PolicyRevision, CreatedAt: value.CreatedAt}
}

type sandboxView struct {
	ID                  string                  `json:"id"`
	EnvironmentRevision string                  `json:"environment_revision"`
	SourceCheckpointID  string                  `json:"source_checkpoint_id,omitempty"`
	State               domain.SandboxState     `json:"state"`
	CleanupTarget       domain.SandboxState     `json:"cleanup_target,omitempty"`
	Lifecycle           domain.Lifecycle        `json:"lifecycle"`
	Network             domain.NetworkPolicy    `json:"network"`
	ConnectorRevisions  []string                `json:"connector_revisions,omitempty"`
	WorkspaceMounts     []domain.WorkspaceMount `json:"workspace_mounts,omitempty"`
	CreatedAt           time.Time               `json:"created_at"`
	UpdatedAt           time.Time               `json:"updated_at"`
	LastActiveAt        time.Time               `json:"last_active_at,omitempty"`
	StandbyEligibleAt   time.Time               `json:"standby_eligible_at,omitempty"`
	ExpiresAt           time.Time               `json:"expires_at"`
	Revision            int64                   `json:"revision"`
	Failure             *domain.Failure         `json:"failure,omitempty"`
}

func sandboxViewOf(value domain.Sandbox) sandboxView {
	return sandboxView{ID: value.ID, EnvironmentRevision: value.EnvironmentRevision, SourceCheckpointID: value.SourceCheckpointID, State: value.State, CleanupTarget: value.CleanupTarget, Lifecycle: value.Lifecycle, Network: value.Network, ConnectorRevisions: value.ConnectorRevisions, WorkspaceMounts: value.WorkspaceMounts, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt, LastActiveAt: value.LastActiveAt, StandbyEligibleAt: value.StandbyEligibleAt, ExpiresAt: value.ExpiresAt, Revision: value.Revision, Failure: value.Failure}
}

type workspaceView struct {
	ID            string                `json:"id"`
	Name          string                `json:"name"`
	State         domain.WorkspaceState `json:"state"`
	CleanupTarget domain.WorkspaceState `json:"cleanup_target,omitempty"`
	CreatedAt     time.Time             `json:"created_at"`
	UpdatedAt     time.Time             `json:"updated_at"`
	Failure       *domain.Failure       `json:"failure,omitempty"`
}

func workspaceViewOf(value domain.Workspace) workspaceView {
	return workspaceView{ID: value.ID, Name: value.Name, State: value.State, CleanupTarget: value.CleanupTarget, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt, Failure: value.Failure}
}

type checkpointView struct {
	ID                  string                `json:"id"`
	SourceSandboxID     string                `json:"source_sandbox_id"`
	EnvironmentRevision string                `json:"environment_revision"`
	Kind                domain.CheckpointKind `json:"kind"`
	CreatedAt           time.Time             `json:"created_at"`
}

func checkpointViewOf(value domain.Checkpoint) checkpointView {
	return checkpointView{ID: value.ID, SourceSandboxID: value.SourceSandboxID, EnvironmentRevision: value.EnvironmentRevision, Kind: value.Kind, CreatedAt: value.CreatedAt}
}

type connectorView struct {
	RevisionID          string    `json:"revision_id"`
	Name                string    `json:"name"`
	Destination         string    `json:"destination"`
	AllowedMethods      []string  `json:"allowed_methods"`
	AllowedPaths        []string  `json:"allowed_paths"`
	AllowPrivateNetwork bool      `json:"allow_private_network"`
	Status              string    `json:"status"`
	CreatedAt           time.Time `json:"created_at"`
}

func connectorViewOf(value domain.Connector) connectorView {
	return connectorView{RevisionID: value.RevisionID, Name: value.Name, Destination: value.Destination, AllowedMethods: value.AllowedMethods, AllowedPaths: value.AllowedPaths, AllowPrivateNetwork: value.AllowPrivateNetwork, Status: value.Status, CreatedAt: value.CreatedAt}
}

type operationView struct {
	ID         string                `json:"id"`
	Kind       string                `json:"kind"`
	ResourceID string                `json:"resource_id"`
	State      domain.OperationState `json:"state"`
	CreatedAt  time.Time             `json:"created_at"`
	UpdatedAt  time.Time             `json:"updated_at"`
	Failure    *domain.Failure       `json:"failure,omitempty"`
}

func operationViewOf(value domain.Operation) operationView {
	return operationView{ID: value.ID, Kind: value.Kind, ResourceID: value.ResourceID, State: value.State, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt, Failure: value.Failure}
}

type eventView struct {
	ID          string              `json:"id"`
	Sequence    int64               `json:"sequence"`
	ResourceID  string              `json:"resource_id"`
	OperationID string              `json:"operation_id,omitempty"`
	Type        string              `json:"type"`
	State       domain.SandboxState `json:"state,omitempty"`
	At          time.Time           `json:"at"`
	Details     map[string]any      `json:"details,omitempty"`
}

func eventViewOf(value domain.Event) eventView {
	return eventView{ID: value.ID, Sequence: value.Sequence, ResourceID: value.ResourceID, OperationID: value.OperationID, Type: value.Type, State: value.State, At: value.At, Details: value.Details}
}

type requestMetrics struct {
	total    atomic.Uint64
	active   atomic.Int64
	status2x atomic.Uint64
	status4x atomic.Uint64
	status5x atomic.Uint64
	rejected atomic.Uint64
}

type observedResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *observedResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *observedResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}

func (w *observedResponseWriter) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *observedResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID, err := opaqueTokenBytes(16)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "could not create request identity")
			return
		}
		w.Header().Set("X-Request-ID", requestID)
		started := time.Now()
		s.metrics.total.Add(1)
		s.metrics.active.Add(1)
		defer s.metrics.active.Add(-1)
		observed := &observedResponseWriter{ResponseWriter: w}
		next.ServeHTTP(observed, r)
		status := observed.status
		if status == 0 {
			status = http.StatusOK
		}
		switch status / 100 {
		case 2:
			s.metrics.status2x.Add(1)
		case 4:
			s.metrics.status4x.Add(1)
		case 5:
			s.metrics.status5x.Add(1)
		}
		if s.logger != nil && r.URL.Path != "/healthz" && r.URL.Path != "/readyz" && r.URL.Path != "/metrics" {
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			s.logger.Printf("request_id=%s method=%s route=%q status=%d duration_ms=%d", requestID, r.Method, route, status, time.Since(started).Milliseconds())
		}
	})
}

func (s *Server) admit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		select {
		case s.inFlight <- struct{}{}:
			defer func() { <-s.inFlight }()
			next.ServeHTTP(w, r)
		default:
			s.metrics.rejected.Add(1)
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusServiceUnavailable, "overloaded", "runtime request capacity is temporarily exhausted")
		}
	})
}

func (s *Server) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "# TYPE runtime_http_requests_total counter\nruntime_http_requests_total %d\n", s.metrics.total.Load())
	_, _ = fmt.Fprintf(w, "# TYPE runtime_http_requests_active gauge\nruntime_http_requests_active %d\n", s.metrics.active.Load())
	_, _ = fmt.Fprintf(w, "# TYPE runtime_http_responses_total counter\nruntime_http_responses_total{class=\"2xx\"} %d\nruntime_http_responses_total{class=\"4xx\"} %d\nruntime_http_responses_total{class=\"5xx\"} %d\n", s.metrics.status2x.Load(), s.metrics.status4x.Load(), s.metrics.status5x.Load())
	_, _ = fmt.Fprintf(w, "# TYPE runtime_http_admission_rejections_total counter\nruntime_http_admission_rejections_total %d\n", s.metrics.rejected.Load())
	_ = s.phaseMetrics.WritePrometheus(w)
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" || strings.HasPrefix(r.URL.Path, "/p/") || (s.connector != nil && strings.HasPrefix(r.URL.Path, "/connector/")) {
			next.ServeHTTP(w, r)
			return
		}
		authorization := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(authorization, prefix) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
			return
		}
		if project(r) == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "X-Project-ID is required")
			return
		}
		if err := domain.ValidateProjectID(project(r)); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "X-Project-ID is invalid")
			return
		}
		authenticated, authorized := s.authorizer.Authorize(strings.TrimPrefix(authorization, prefix), project(r))
		if !authenticated {
			writeError(w, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
			return
		}
		if !authorized {
			writeError(w, http.StatusForbidden, "forbidden", "credential is not authorized for this project")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/p/") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func project(r *http.Request) string { return r.Header.Get("X-Project-ID") }

func includeTerminal(r *http.Request) (bool, error) {
	raw := r.URL.Query().Get("include_terminal")
	if raw == "" {
		return false, nil
	}
	return strconv.ParseBool(raw)
}

func decodeBody(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must be valid JSON with known fields")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must contain one JSON value")
		return false
	}
	return true
}

func emptyBody(w http.ResponseWriter, r *http.Request) bool {
	data, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(data) != 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be empty")
		return false
	}
	return true
}

func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, service.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, service.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, service.ErrDenied):
		writeError(w, http.StatusUnprocessableEntity, "capability_unavailable", err.Error())
	case errors.Is(err, service.ErrQuota):
		writeError(w, http.StatusTooManyRequests, "quota_exceeded", "project capacity limit reached")
	case errors.Is(err, service.ErrCapacity):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "capacity_exhausted", "runtime capacity is temporarily exhausted")
	case errors.Is(err, service.ErrBackend):
		writeError(w, http.StatusBadGateway, "backend_failure", "sandbox backend did not confirm the operation")
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		fmt.Printf("encode response failed: %v\n", err)
	}
}
