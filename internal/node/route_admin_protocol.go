package node

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/infercrane/brezel/internal/nodeledger"
)

const (
	routeAdminReadyPath        = "/readyz"
	routeAdminPrefix           = "/internal/v1/routes/"
	routeAdminMaxRequestBytes  = 8 << 10
	routeAdminMaxResponseBytes = 64 << 10
	routeAdminMaxEngineIDBytes = 1 << 10
)

var routeAdminIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// RouteBindRequest creates a node-private engine assignment. EngineID is
// accepted only by the node control endpoint and never appears in a response.
type RouteBindRequest struct {
	RouteID   string `json:"route_id"`
	ProjectID string `json:"project_id"`
	SandboxID string `json:"sandbox_id"`
	EngineID  string `json:"engine_id"`
}

// RouteTransitionRequest identifies the complete assignment and lifecycle
// state that the durable controller independently expects to mutate.
type RouteTransitionRequest struct {
	RouteID       string           `json:"route_id"`
	ProjectID     string           `json:"project_id"`
	SandboxID     string           `json:"sandbox_id"`
	Generation    uint64           `json:"generation"`
	ExpectedState nodeledger.State `json:"expected_state"`
}

// RouteRebindRequest replaces the private engine target of a standby route.
// A successful rebind always returns a newly allocated generation.
type RouteRebindRequest struct {
	RouteID       string           `json:"route_id"`
	ProjectID     string           `json:"project_id"`
	SandboxID     string           `json:"sandbox_id"`
	Generation    uint64           `json:"generation"`
	ExpectedState nodeledger.State `json:"expected_state"`
	EngineID      string           `json:"engine_id"`
}

// RouteAdminResult contains only the public node route. The node-private
// engine identity is deliberately absent.
type RouteAdminResult struct {
	NodeID   string           `json:"node_id"`
	Route    nodeledger.Route `json:"route"`
	Replayed bool             `json:"replayed"`
}

// RouteRemoveResult confirms removal without serializing the former private
// binding. Removal retries are not considered idempotent because the ledger
// intentionally retains no project-scoped removal receipt.
type RouteRemoveResult struct {
	NodeID     string `json:"node_id"`
	RouteID    string `json:"route_id"`
	ProjectID  string `json:"project_id"`
	SandboxID  string `json:"sandbox_id"`
	Generation uint64 `json:"generation"`
}

type routeAdminAction string

const (
	routeAdminBind        routeAdminAction = "bind"
	routeAdminAttachReady routeAdminAction = "attach-ready"
	routeAdminDrain       routeAdminAction = "drain"
	routeAdminStandby     routeAdminAction = "standby"
	routeAdminRebind      routeAdminAction = "rebind"
	routeAdminRelease     routeAdminAction = "release"
	routeAdminRemove      routeAdminAction = "remove"
)

func validateRouteAdminIdentity(routeID, projectID, sandboxID string) error {
	for _, value := range []string{routeID, projectID, sandboxID} {
		if !routeAdminIDPattern.MatchString(value) {
			return errors.New("invalid route identity")
		}
	}
	return nil
}

func validateRouteAdminEngineID(engineID string) error {
	if engineID == "" || len(engineID) > routeAdminMaxEngineIDBytes || !utf8.ValidString(engineID) || strings.TrimSpace(engineID) != engineID {
		return errors.New("invalid private engine identity")
	}
	for _, character := range engineID {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return errors.New("invalid private engine identity")
		}
	}
	return nil
}

func validateRouteBindRequest(request RouteBindRequest, pathRoute string) error {
	if request.RouteID != pathRoute {
		return errors.New("route path and body do not match")
	}
	if err := validateRouteAdminIdentity(request.RouteID, request.ProjectID, request.SandboxID); err != nil {
		return err
	}
	return validateRouteAdminEngineID(request.EngineID)
}

func validateRouteTransitionRequest(request RouteTransitionRequest, pathRoute string, action routeAdminAction) error {
	if request.RouteID != pathRoute || request.Generation == 0 {
		return errors.New("invalid route assignment")
	}
	if err := validateRouteAdminIdentity(request.RouteID, request.ProjectID, request.SandboxID); err != nil {
		return err
	}
	if !routeAdminExpectedStateAllowed(action, request.ExpectedState) {
		return fmt.Errorf("invalid expected state for %s", action)
	}
	return nil
}

func validateRouteRebindRequest(request RouteRebindRequest, pathRoute string) error {
	transition := RouteTransitionRequest{
		RouteID: request.RouteID, ProjectID: request.ProjectID, SandboxID: request.SandboxID,
		Generation: request.Generation, ExpectedState: request.ExpectedState,
	}
	if err := validateRouteTransitionRequest(transition, pathRoute, routeAdminRebind); err != nil {
		return err
	}
	return validateRouteAdminEngineID(request.EngineID)
}

func routeAdminExpectedStateAllowed(action routeAdminAction, state nodeledger.State) bool {
	switch action {
	case routeAdminAttachReady:
		return state == nodeledger.StateAttaching
	case routeAdminDrain:
		return state == nodeledger.StateReady
	case routeAdminStandby:
		return state == nodeledger.StateDraining
	case routeAdminRebind:
		return state == nodeledger.StateStandby
	case routeAdminRelease:
		return state == nodeledger.StateAttaching || state == nodeledger.StateDraining || state == nodeledger.StateStandby
	case routeAdminRemove:
		return state == nodeledger.StateReleased
	default:
		return false
	}
}

func validRouteAdminPublicRoute(route nodeledger.Route) bool {
	if validateRouteAdminIdentity(route.RouteID, route.ProjectID, route.SandboxID) != nil || route.Generation == 0 || route.LastActivity.IsZero() {
		return false
	}
	_, offset := route.LastActivity.Zone()
	if offset != 0 {
		return false
	}
	switch route.State {
	case nodeledger.StateAttaching, nodeledger.StateReady, nodeledger.StateDraining, nodeledger.StateStandby, nodeledger.StateReleased:
		return true
	default:
		return false
	}
}

func routeAdminNow(now func() time.Time) time.Time {
	if now == nil {
		return time.Now().UTC()
	}
	return now().UTC().Round(0)
}
