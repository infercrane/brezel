package httpapi

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/infercrane/sandbox-runtime-lab/internal/backend/e2b"
	"github.com/infercrane/sandbox-runtime-lab/internal/domain"
	"github.com/infercrane/sandbox-runtime-lab/internal/service"
)

const maxBodyBytes = 1 << 20

type Server struct {
	service   *service.Service
	token     string
	mux       *http.ServeMux
	connector http.Handler
}

type Option func(*Server)

func WithConnectorHandler(handler http.Handler) Option {
	return func(s *Server) { s.connector = handler }
}

func New(svc *service.Service, token string, options ...Option) (*Server, error) {
	if svc == nil {
		return nil, errors.New("service is required")
	}
	if len(token) < 32 {
		return nil, errors.New("service token must be at least 32 characters")
	}
	s := &Server{service: svc, token: token, mux: http.NewServeMux()}
	for _, option := range options {
		option(s)
	}
	s.routes()
	return s, nil
}

func (s *Server) Handler() http.Handler {
	return s.securityHeaders(s.authenticate(s.mux))
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	s.mux.HandleFunc("GET /v1/capabilities", s.capabilities)
	s.mux.HandleFunc("GET /v1/receipt-public-key", s.receiptPublicKey)
	s.mux.HandleFunc("POST /v1/environments", s.createEnvironment)
	s.mux.HandleFunc("GET /v1/environments/{revision}", s.getEnvironment)
	s.mux.HandleFunc("POST /v1/connectors", s.createConnector)
	s.mux.HandleFunc("GET /v1/connectors/{revision}", s.getConnector)
	s.mux.HandleFunc("POST /v1/sandboxes", s.createSandbox)
	s.mux.HandleFunc("GET /v1/sandboxes/{id}", s.getSandbox)
	s.mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.deleteSandbox)
	s.mux.HandleFunc("POST /v1/sandboxes/{action}", s.sandboxAction)
	s.mux.HandleFunc("POST /v1/sandboxes/{id}/checkpoints", s.createCheckpoint)
	s.mux.HandleFunc("GET /v1/sandboxes/{id}/events", s.events)
	s.mux.HandleFunc("GET /v1/sandboxes/{id}/receipt", s.sandboxReceipt)
	s.mux.HandleFunc("GET /v1/operations/{id}", s.getOperation)
	if s.connector != nil {
		s.mux.Handle("/connector/", s.connector)
	}
}

func (s *Server) capabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"backend":                  "e2b",
		"adapter_audited_revision": e2b.AuditedRevision,
		"implemented":              s.service.Capabilities(),
		"qualification":            "unverified",
		"qualification_note":       "Run conformance on the exact deployment before making assurance or performance claims.",
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
	writeJSON(w, http.StatusCreated, mutationResponse{Resource: resource, Operation: operation})
}

func (s *Server) getEnvironment(w http.ResponseWriter, r *http.Request) {
	resource, err := s.service.GetEnvironment(project(r), r.PathValue("revision"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resource)
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
	writeJSON(w, http.StatusCreated, mutationResponse{Resource: resource, Operation: operation})
}

func (s *Server) getConnector(w http.ResponseWriter, r *http.Request) {
	resource, err := s.service.GetConnector(project(r), r.PathValue("revision"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resource)
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
	writeJSON(w, http.StatusAccepted, mutationResponse{Resource: resource, Operation: operation})
}

func (s *Server) getSandbox(w http.ResponseWriter, r *http.Request) {
	resource, err := s.service.GetSandbox(r.Context(), project(r), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resource)
}

func (s *Server) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	resource, operation, err := s.service.Delete(r.Context(), project(r), r.PathValue("id"), r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, mutationResponse{Resource: resource, Operation: operation})
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
	writeJSON(w, http.StatusAccepted, mutationResponse{Resource: resource, Operation: operation})
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
	writeJSON(w, http.StatusAccepted, mutationResponse{Resource: resource, Operation: operation})
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
	writeJSON(w, http.StatusAccepted, mutationResponse{Resource: resource, Operation: operation})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	resources, err := s.service.Events(project(r), r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": resources})
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
	writeJSON(w, http.StatusOK, operation)
}

type mutationResponse struct {
	Resource  any `json:"resource"`
	Operation any `json:"operation"`
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || (s.connector != nil && strings.HasPrefix(r.URL.Path, "/connector/")) {
			next.ServeHTTP(w, r)
			return
		}
		authorization := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(authorization, prefix) || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(authorization, prefix)), []byte(s.token)) != 1 {
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
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func project(r *http.Request) string { return r.Header.Get("X-Project-ID") }

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
