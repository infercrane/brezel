package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/brezel/internal/domain"
	"github.com/infercrane/brezel/internal/node"
	"github.com/infercrane/brezel/internal/nodeledger"
)

func TestNodeRouteLifecycleUsesFreshGenerationAndRemovesRoute(t *testing.T) {
	admin := &memoryRouteAdmin{nodeID: "node-a"}
	svc := &Service{routeAdmin: admin}
	sandbox := domain.Sandbox{ID: "sbx-one", ProjectID: "project-a", BackendID: "engine-one"}

	attached, err := svc.attachNodeRoute(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if attached.NodeGeneration != 1 || admin.route.State != nodeledger.StateReady {
		t.Fatalf("attached=%#v route=%#v", attached, admin.route)
	}
	drained, err := svc.prepareNodeRoutePause(context.Background(), attached)
	if err != nil {
		t.Fatal(err)
	}
	standby, err := svc.completeNodeRouteStandby(context.Background(), drained)
	if err != nil {
		t.Fatal(err)
	}
	if admin.route.State != nodeledger.StateStandby {
		t.Fatalf("route state=%s", admin.route.State)
	}
	standby.BackendID = "engine-two"
	resumed, err := svc.resumeNodeRoute(context.Background(), standby)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.NodeGeneration != 2 || admin.route.Generation != 2 || admin.route.State != nodeledger.StateReady {
		t.Fatalf("resumed=%#v route=%#v", resumed, admin.route)
	}
	removed, err := svc.removeNodeRoute(context.Background(), resumed)
	if err != nil {
		t.Fatal(err)
	}
	if !admin.removed || removed.NodeRouteID != "" || removed.NodeGeneration != 0 {
		t.Fatalf("removed=%#v admin=%#v", removed, admin)
	}
}

func TestNodeRouteResolveRejectsCrossSandboxAssignment(t *testing.T) {
	admin := &memoryRouteAdmin{nodeID: "node-a", route: nodeledger.Route{
		RouteID: "sbx-one", ProjectID: "project-b", SandboxID: "sbx-other",
		Generation: 2, State: nodeledger.StateReady, LastActivity: time.Now().UTC(),
	}}
	svc := &Service{routeAdmin: admin}
	_, err := svc.prepareNodeRoutePause(context.Background(), domain.Sandbox{
		ID: "sbx-one", ProjectID: "project-a", BackendID: "engine-one",
		NodeID: "node-a", NodeRouteID: "sbx-one", NodeGeneration: 1,
	})
	if err == nil {
		t.Fatal("cross-sandbox node assignment was accepted")
	}
}

type memoryRouteAdmin struct {
	mu       sync.Mutex
	nodeID   string
	route    nodeledger.Route
	engine   string
	removed  bool
	readyErr error
}

func (a *memoryRouteAdmin) Ready(context.Context) error { return a.readyErr }

func (a *memoryRouteAdmin) result() node.RouteAdminResult {
	return node.RouteAdminResult{NodeID: a.nodeID, Route: a.route}
}

func (a *memoryRouteAdmin) Resolve(_ context.Context, routeID string) (node.RouteAdminResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.removed || a.route.RouteID != routeID {
		return node.RouteAdminResult{}, &node.RouteAdminError{StatusCode: http.StatusNotFound, Code: "route_not_found"}
	}
	return a.result(), nil
}

func (a *memoryRouteAdmin) Bind(_ context.Context, request node.RouteBindRequest) (node.RouteAdminResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.route.RouteID != "" && !a.removed {
		return node.RouteAdminResult{}, errors.New("conflict")
	}
	a.removed = false
	a.engine = request.EngineID
	a.route = nodeledger.Route{RouteID: request.RouteID, ProjectID: request.ProjectID, SandboxID: request.SandboxID, Generation: 1, State: nodeledger.StateAttaching, LastActivity: time.Now().UTC()}
	return a.result(), nil
}

func (a *memoryRouteAdmin) AttachReady(_ context.Context, request node.RouteTransitionRequest) (node.RouteAdminResult, error) {
	return a.transition(request, nodeledger.StateReady)
}

func (a *memoryRouteAdmin) Drain(_ context.Context, request node.RouteTransitionRequest) (node.RouteAdminResult, error) {
	return a.transition(request, nodeledger.StateDraining)
}

func (a *memoryRouteAdmin) Standby(_ context.Context, request node.RouteTransitionRequest) (node.RouteAdminResult, error) {
	return a.transition(request, nodeledger.StateStandby)
}

func (a *memoryRouteAdmin) Release(_ context.Context, request node.RouteTransitionRequest) (node.RouteAdminResult, error) {
	return a.transition(request, nodeledger.StateReleased)
}

func (a *memoryRouteAdmin) transition(request node.RouteTransitionRequest, target nodeledger.State) (node.RouteAdminResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.removed || request.RouteID != a.route.RouteID || request.ProjectID != a.route.ProjectID || request.SandboxID != a.route.SandboxID || request.Generation != a.route.Generation {
		return node.RouteAdminResult{}, errors.New("stale route")
	}
	if a.route.State != request.ExpectedState && a.route.State != target {
		return node.RouteAdminResult{}, errors.New("wrong state")
	}
	a.route.State = target
	a.route.LastActivity = time.Now().UTC()
	return a.result(), nil
}

func (a *memoryRouteAdmin) Rebind(_ context.Context, request node.RouteRebindRequest) (node.RouteAdminResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.route.State != nodeledger.StateStandby || request.Generation != a.route.Generation {
		return node.RouteAdminResult{}, errors.New("cannot rebind")
	}
	a.engine = request.EngineID
	a.route.Generation++
	a.route.State = nodeledger.StateAttaching
	a.route.LastActivity = time.Now().UTC()
	return a.result(), nil
}

func (a *memoryRouteAdmin) Remove(_ context.Context, request node.RouteTransitionRequest) (node.RouteRemoveResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.route.State != nodeledger.StateReleased || request.Generation != a.route.Generation {
		return node.RouteRemoveResult{}, errors.New("cannot remove")
	}
	a.removed = true
	return node.RouteRemoveResult{NodeID: a.nodeID, RouteID: a.route.RouteID, ProjectID: a.route.ProjectID, SandboxID: a.route.SandboxID, Generation: a.route.Generation}, nil
}
