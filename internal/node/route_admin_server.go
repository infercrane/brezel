package node

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/infercrane/brezel/internal/nodeidentity"
	"github.com/infercrane/brezel/internal/nodeledger"
)

// RouteAdminHandlerConfig supplies the node-local authority for route
// lifecycle. The handler must be served by nodeidentity.NewControlHTTPServer;
// it additionally rejects requests that lack a verified TLS peer chain.
type RouteAdminHandlerConfig struct {
	NodeID string
	Ledger *nodeledger.Ledger
	Now    func() time.Time
}

type routeAdminHandler struct {
	nodeID string
	ledger *nodeledger.Ledger
	now    func() time.Time
}

// NewRouteAdminHandler creates the private node lifecycle endpoint. It does
// not authorize product lifecycle or placement decisions; it only applies an
// already-authorized controller decision to the fenced node-local ledger.
func NewRouteAdminHandler(config RouteAdminHandlerConfig) (http.Handler, error) {
	if _, err := nodeidentity.NewIdentity(nodeidentity.RoleNode, config.NodeID); err != nil {
		return nil, errors.New("valid node identity is required")
	}
	if config.Ledger == nil {
		return nil, errors.New("node generation ledger is required")
	}
	if err := config.Ledger.Ready(); err != nil {
		return nil, errors.New("node generation ledger is not ready")
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &routeAdminHandler{nodeID: config.NodeID, ledger: config.Ledger, now: now}, nil
}

func (h *routeAdminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !routeAdminHasVerifiedPeer(r) {
		writeRouteAdminError(w, http.StatusUnauthorized, "mtls_required")
		return
	}
	routeID, action, ok := parseRouteAdminPath(r.URL.Path)
	if !ok || r.URL.RawQuery != "" {
		writeRouteAdminError(w, http.StatusNotFound, "route_not_found")
		return
	}
	wantMethod := http.MethodPost
	if action == routeAdminBind {
		wantMethod = http.MethodPut
	}
	if r.Method != wantMethod {
		w.Header().Set("Allow", wantMethod)
		writeRouteAdminError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if !validRouteAdminContentType(r.Header.Get("Content-Type")) || r.Header.Get("Content-Encoding") != "" {
		writeRouteAdminError(w, http.StatusUnsupportedMediaType, "invalid_content_type")
		return
	}

	switch action {
	case routeAdminBind:
		h.bind(w, r, routeID)
	case routeAdminRebind:
		h.rebind(w, r, routeID)
	case routeAdminAttachReady, routeAdminDrain, routeAdminStandby, routeAdminRelease:
		h.transition(w, r, routeID, action)
	case routeAdminRemove:
		h.remove(w, r, routeID)
	default:
		writeRouteAdminError(w, http.StatusNotFound, "route_not_found")
	}
}

func (h *routeAdminHandler) bind(w http.ResponseWriter, r *http.Request, routeID string) {
	var request RouteBindRequest
	if !decodeRouteAdminRequest(w, r, &request) {
		return
	}
	if validateRouteBindRequest(request, routeID) != nil {
		writeRouteAdminError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	binding, err := h.ledger.Bind(request.RouteID, request.ProjectID, request.SandboxID, request.EngineID, routeAdminNow(h.now))
	if err == nil {
		writeRouteAdminJSON(w, http.StatusOK, RouteAdminResult{NodeID: h.nodeID, Route: binding.Public()})
		return
	}
	if errors.Is(err, nodeledger.ErrConflict) {
		current, resolveErr := h.ledger.Resolve(request.RouteID)
		if resolveErr == nil && routeAdminBindingMatches(current, request.ProjectID, request.SandboxID) && current.EngineID() == request.EngineID &&
			(current.Public().State == nodeledger.StateAttaching || current.Public().State == nodeledger.StateReady) {
			writeRouteAdminJSON(w, http.StatusOK, RouteAdminResult{NodeID: h.nodeID, Route: current.Public(), Replayed: true})
			return
		}
	}
	writeRouteAdminLedgerError(w, err)
}

func (h *routeAdminHandler) transition(w http.ResponseWriter, r *http.Request, routeID string, action routeAdminAction) {
	var request RouteTransitionRequest
	if !decodeRouteAdminRequest(w, r, &request) {
		return
	}
	if err := validateRouteTransitionRequest(request, routeID, action); err != nil {
		writeRouteAdminError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	target := routeAdminTargetState(action)
	binding, replayed, err := h.transitionWithReplay(request, target)
	if err != nil {
		writeRouteAdminLedgerError(w, err)
		return
	}
	writeRouteAdminJSON(w, http.StatusOK, RouteAdminResult{NodeID: h.nodeID, Route: binding.Public(), Replayed: replayed})
}

func (h *routeAdminHandler) transitionWithReplay(request RouteTransitionRequest, target nodeledger.State) (nodeledger.Binding, bool, error) {
	current, err := h.ledger.Resolve(request.RouteID)
	if err != nil {
		return nodeledger.Binding{}, false, err
	}
	if !routeAdminBindingMatches(current, request.ProjectID, request.SandboxID) {
		return nodeledger.Binding{}, false, nodeledger.ErrNotFound
	}
	if current.Public().Generation != request.Generation {
		return nodeledger.Binding{}, false, nodeledger.ErrStaleGeneration
	}
	if current.Public().State == target {
		return current, true, nil
	}
	if current.Public().State != request.ExpectedState {
		return nodeledger.Binding{}, false, nodeledger.ErrStateConflict
	}
	result, err := h.ledger.Transition(request.RouteID, request.Generation, request.ExpectedState, target)
	if err == nil {
		return result, false, nil
	}
	if errors.Is(err, nodeledger.ErrStateConflict) {
		current, resolveErr := h.ledger.Resolve(request.RouteID)
		if resolveErr == nil && routeAdminBindingMatches(current, request.ProjectID, request.SandboxID) &&
			current.Public().Generation == request.Generation && current.Public().State == target {
			return current, true, nil
		}
	}
	return nodeledger.Binding{}, false, err
}

func (h *routeAdminHandler) rebind(w http.ResponseWriter, r *http.Request, routeID string) {
	var request RouteRebindRequest
	if !decodeRouteAdminRequest(w, r, &request) {
		return
	}
	if err := validateRouteRebindRequest(request, routeID); err != nil {
		writeRouteAdminError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	current, err := h.ledger.Resolve(request.RouteID)
	if err != nil {
		writeRouteAdminLedgerError(w, err)
		return
	}
	if !routeAdminBindingMatches(current, request.ProjectID, request.SandboxID) {
		writeRouteAdminLedgerError(w, nodeledger.ErrNotFound)
		return
	}
	if current.Public().Generation != request.Generation {
		writeRouteAdminLedgerError(w, nodeledger.ErrStaleGeneration)
		return
	}
	if current.Public().State != request.ExpectedState {
		writeRouteAdminLedgerError(w, nodeledger.ErrStateConflict)
		return
	}
	binding, err := h.ledger.Rebind(request.RouteID, request.Generation, request.ExpectedState, request.EngineID, routeAdminNow(h.now))
	if err != nil {
		writeRouteAdminLedgerError(w, err)
		return
	}
	writeRouteAdminJSON(w, http.StatusOK, RouteAdminResult{NodeID: h.nodeID, Route: binding.Public()})
}

func (h *routeAdminHandler) remove(w http.ResponseWriter, r *http.Request, routeID string) {
	var request RouteTransitionRequest
	if !decodeRouteAdminRequest(w, r, &request) {
		return
	}
	if err := validateRouteTransitionRequest(request, routeID, routeAdminRemove); err != nil {
		writeRouteAdminError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	current, err := h.ledger.Resolve(request.RouteID)
	if err != nil {
		writeRouteAdminLedgerError(w, err)
		return
	}
	if !routeAdminBindingMatches(current, request.ProjectID, request.SandboxID) {
		writeRouteAdminLedgerError(w, nodeledger.ErrNotFound)
		return
	}
	if current.Public().Generation != request.Generation {
		writeRouteAdminLedgerError(w, nodeledger.ErrStaleGeneration)
		return
	}
	if current.Public().State != request.ExpectedState {
		writeRouteAdminLedgerError(w, nodeledger.ErrStateConflict)
		return
	}
	if err := h.ledger.Remove(request.RouteID, request.Generation, request.ExpectedState); err != nil {
		writeRouteAdminLedgerError(w, err)
		return
	}
	writeRouteAdminJSON(w, http.StatusOK, RouteRemoveResult{
		NodeID: h.nodeID, RouteID: request.RouteID, ProjectID: request.ProjectID,
		SandboxID: request.SandboxID, Generation: request.Generation,
	})
}

func routeAdminHasVerifiedPeer(r *http.Request) bool {
	if r == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return false
	}
	peer := r.TLS.PeerCertificates[0]
	verified := r.TLS.VerifiedChains[0][0]
	return peer != nil && verified != nil && peer.Equal(verified)
}

func parseRouteAdminPath(path string) (string, routeAdminAction, bool) {
	if !strings.HasPrefix(path, routeAdminPrefix) {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, routeAdminPrefix), "/")
	if len(parts) == 1 && routeAdminIDPattern.MatchString(parts[0]) {
		return parts[0], routeAdminBind, true
	}
	if len(parts) != 2 || !routeAdminIDPattern.MatchString(parts[0]) {
		return "", "", false
	}
	action := routeAdminAction(parts[1])
	switch action {
	case routeAdminAttachReady, routeAdminDrain, routeAdminStandby, routeAdminRebind, routeAdminRelease, routeAdminRemove:
		return parts[0], action, true
	default:
		return "", "", false
	}
}

func routeAdminTargetState(action routeAdminAction) nodeledger.State {
	switch action {
	case routeAdminAttachReady:
		return nodeledger.StateReady
	case routeAdminDrain:
		return nodeledger.StateDraining
	case routeAdminStandby:
		return nodeledger.StateStandby
	case routeAdminRelease:
		return nodeledger.StateReleased
	default:
		return ""
	}
}

func routeAdminBindingMatches(binding nodeledger.Binding, projectID, sandboxID string) bool {
	route := binding.Public()
	return route.ProjectID == projectID && route.SandboxID == sandboxID
}

func decodeRouteAdminRequest(w http.ResponseWriter, r *http.Request, destination any) bool {
	if r == nil || r.Body == nil {
		writeRouteAdminError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	if r.ContentLength > routeAdminMaxRequestBytes {
		writeRouteAdminError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, routeAdminMaxRequestBytes+1))
	if err != nil {
		writeRouteAdminError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	if len(data) == 0 {
		writeRouteAdminError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	if len(data) > routeAdminMaxRequestBytes {
		writeRouteAdminError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeRouteAdminError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeRouteAdminError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

func validRouteAdminContentType(value string) bool {
	if value == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n") {
		return false
	}
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || mediaType != "application/json" {
		return false
	}
	for name, value := range parameters {
		if !strings.EqualFold(name, "charset") || !strings.EqualFold(value, "utf-8") {
			return false
		}
	}
	return true
}

func writeRouteAdminLedgerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, nodeledger.ErrNotFound):
		writeRouteAdminError(w, http.StatusNotFound, "route_not_found")
	case errors.Is(err, nodeledger.ErrInvalid):
		writeRouteAdminError(w, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, nodeledger.ErrActiveOperations):
		writeRouteAdminError(w, http.StatusConflict, "route_busy")
	case errors.Is(err, nodeledger.ErrConflict), errors.Is(err, nodeledger.ErrStaleGeneration), errors.Is(err, nodeledger.ErrStateConflict), errors.Is(err, nodeledger.ErrIllegalTransition):
		writeRouteAdminError(w, http.StatusConflict, "route_conflict")
	default:
		writeRouteAdminError(w, http.StatusServiceUnavailable, "node_unavailable")
	}
}

func writeRouteAdminError(w http.ResponseWriter, status int, code string) {
	writeRouteAdminJSON(w, status, map[string]any{"error": map[string]string{"code": code}})
}

func writeRouteAdminJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
