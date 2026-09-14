package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/brezel/internal/nodeledger"
)

const (
	routeAdminTestNode    = "node-a"
	routeAdminTestRoute   = "route-a"
	routeAdminTestProject = "project-a"
	routeAdminTestSandbox = "sandbox-a"
	routeAdminTestEngine  = "private-engine-credential-a"
)

var routeAdminTestTime = time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

type routeAdminHarness struct {
	t       *testing.T
	ledger  *nodeledger.Ledger
	handler http.Handler
	client  *RouteAdminClient
}

func newRouteAdminHarness(t *testing.T) *routeAdminHarness {
	t.Helper()
	directory := t.TempDir() + "/node"
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	ledger, err := nodeledger.Open(directory + "/ledger.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	handler, err := NewRouteAdminHandler(RouteAdminHandlerConfig{
		NodeID: routeAdminTestNode,
		Ledger: ledger,
		Now:    func() time.Time { return routeAdminTestTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	transport := &routeAdminHandlerTransport{handler: handler}
	client, err := newRouteAdminClient("https://node.test", &http.Client{Transport: transport, Timeout: time.Second}, routeAdminTestNode)
	if err != nil {
		t.Fatal(err)
	}
	return &routeAdminHarness{t: t, ledger: ledger, handler: handler, client: client}
}

func TestRouteAdminLifecycleAndRetrySemantics(t *testing.T) {
	harness := newRouteAdminHarness(t)
	ctx := context.Background()
	bindRequest := RouteBindRequest{
		RouteID: routeAdminTestRoute, ProjectID: routeAdminTestProject,
		SandboxID: routeAdminTestSandbox, EngineID: routeAdminTestEngine,
	}
	bound, err := harness.client.Bind(ctx, bindRequest)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Replayed || bound.Route.State != nodeledger.StateAttaching || bound.Route.Generation == 0 {
		t.Fatalf("unexpected bind result: %#v", bound)
	}
	replayedBind, err := harness.client.Bind(ctx, bindRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !replayedBind.Replayed || replayedBind.Route != bound.Route {
		t.Fatalf("bind retry did not converge: %#v", replayedBind)
	}

	readyRequest := routeAdminTransition(bound.Route, nodeledger.StateAttaching)
	ready, err := harness.client.AttachReady(ctx, readyRequest)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Route.State != nodeledger.StateReady || ready.Replayed {
		t.Fatalf("unexpected attach-ready result: %#v", ready)
	}
	replayedReady, err := harness.client.AttachReady(ctx, readyRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !replayedReady.Replayed || replayedReady.Route.State != nodeledger.StateReady {
		t.Fatalf("attach-ready retry did not converge: %#v", replayedReady)
	}

	// A bind retry may observe that the exact assignment has advanced to ready.
	replayedBind, err = harness.client.Bind(ctx, bindRequest)
	if err != nil || !replayedBind.Replayed || replayedBind.Route.State != nodeledger.StateReady {
		t.Fatalf("advanced bind retry result=%#v error=%v", replayedBind, err)
	}

	_, lease, err := harness.ledger.AcquireOperation(routeAdminTestRoute, routeAdminTestProject, routeAdminTestSandbox, ready.Route.Generation)
	if err != nil {
		t.Fatal(err)
	}
	drainRequest := routeAdminTransition(ready.Route, nodeledger.StateReady)
	drained, err := harness.client.Drain(ctx, drainRequest)
	if err != nil {
		t.Fatal(err)
	}
	if drained.Route.State != nodeledger.StateDraining {
		t.Fatalf("unexpected drain result: %#v", drained)
	}
	standbyRequest := routeAdminTransition(drained.Route, nodeledger.StateDraining)
	if _, err := harness.client.Standby(ctx, standbyRequest); !routeAdminErrorIs(err, http.StatusConflict, "route_busy") {
		t.Fatalf("standby with active operation error=%v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	standby, err := harness.client.Standby(ctx, standbyRequest)
	if err != nil {
		t.Fatal(err)
	}
	if standby.Route.State != nodeledger.StateStandby {
		t.Fatalf("unexpected standby result: %#v", standby)
	}
	standbyRetry, err := harness.client.Standby(ctx, standbyRequest)
	if err != nil || !standbyRetry.Replayed {
		t.Fatalf("standby retry result=%#v error=%v", standbyRetry, err)
	}

	rebindRequest := RouteRebindRequest{
		RouteID: routeAdminTestRoute, ProjectID: routeAdminTestProject,
		SandboxID: routeAdminTestSandbox, Generation: standby.Route.Generation,
		ExpectedState: nodeledger.StateStandby, EngineID: "private-engine-credential-b",
	}
	rebound, err := harness.client.Rebind(ctx, rebindRequest)
	if err != nil {
		t.Fatal(err)
	}
	if rebound.Route.State != nodeledger.StateAttaching || rebound.Route.Generation <= standby.Route.Generation {
		t.Fatalf("unexpected rebind result: %#v", rebound)
	}
	if _, err := harness.client.Rebind(ctx, rebindRequest); !routeAdminErrorIs(err, http.StatusConflict, "route_conflict") {
		t.Fatalf("ambiguous rebind retry must fail stale: %v", err)
	}
	private, err := harness.ledger.Resolve(routeAdminTestRoute)
	if err != nil || private.EngineID() != rebindRequest.EngineID {
		t.Fatalf("private rebind target=%q error=%v", private.EngineID(), err)
	}

	ready, err = harness.client.AttachReady(ctx, routeAdminTransition(rebound.Route, nodeledger.StateAttaching))
	if err != nil {
		t.Fatal(err)
	}
	drained, err = harness.client.Drain(ctx, routeAdminTransition(ready.Route, nodeledger.StateReady))
	if err != nil {
		t.Fatal(err)
	}
	releaseRequest := routeAdminTransition(drained.Route, nodeledger.StateDraining)
	released, err := harness.client.Release(ctx, releaseRequest)
	if err != nil {
		t.Fatal(err)
	}
	if released.Route.State != nodeledger.StateReleased {
		t.Fatalf("unexpected release result: %#v", released)
	}
	releasedRetry, err := harness.client.Release(ctx, releaseRequest)
	if err != nil || !releasedRetry.Replayed {
		t.Fatalf("release retry result=%#v error=%v", releasedRetry, err)
	}
	removed, err := harness.client.Remove(ctx, routeAdminTransition(released.Route, nodeledger.StateReleased))
	if err != nil {
		t.Fatal(err)
	}
	if removed.RouteID != routeAdminTestRoute || removed.Generation != released.Route.Generation {
		t.Fatalf("unexpected remove result: %#v", removed)
	}
	if _, err := harness.client.Remove(ctx, routeAdminTransition(released.Route, nodeledger.StateReleased)); !routeAdminErrorIs(err, http.StatusNotFound, "route_not_found") {
		t.Fatalf("remove retry without receipt must fail not found: %v", err)
	}
}

func TestRouteAdminRejectsUnauthenticatedMalformedAndUnboundedRequests(t *testing.T) {
	harness := newRouteAdminHarness(t)
	valid, err := json.Marshal(RouteBindRequest{
		RouteID: routeAdminTestRoute, ProjectID: routeAdminTestProject,
		SandboxID: routeAdminTestSandbox, EngineID: routeAdminTestEngine,
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		method      string
		path        string
		contentType string
		body        []byte
		auth        bool
		status      int
		code        string
	}{
		{name: "missing mTLS", method: http.MethodPut, path: routeAdminPath(routeAdminTestRoute, routeAdminBind), contentType: "application/json", body: valid, status: http.StatusUnauthorized, code: "mtls_required"},
		{name: "wrong method", method: http.MethodPost, path: routeAdminPath(routeAdminTestRoute, routeAdminBind), contentType: "application/json", body: valid, auth: true, status: http.StatusMethodNotAllowed, code: "method_not_allowed"},
		{name: "query", method: http.MethodPut, path: routeAdminPath(routeAdminTestRoute, routeAdminBind) + "?engine=x", contentType: "application/json", body: valid, auth: true, status: http.StatusNotFound, code: "route_not_found"},
		{name: "bad content type", method: http.MethodPut, path: routeAdminPath(routeAdminTestRoute, routeAdminBind), contentType: "text/plain", body: valid, auth: true, status: http.StatusUnsupportedMediaType, code: "invalid_content_type"},
		{name: "content encoding", method: http.MethodPut, path: routeAdminPath(routeAdminTestRoute, routeAdminBind), contentType: "application/json", body: valid, auth: true, status: http.StatusUnsupportedMediaType, code: "invalid_content_type"},
		{name: "empty", method: http.MethodPut, path: routeAdminPath(routeAdminTestRoute, routeAdminBind), contentType: "application/json", auth: true, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "malformed", method: http.MethodPut, path: routeAdminPath(routeAdminTestRoute, routeAdminBind), contentType: "application/json", body: []byte(`{"route_id":`), auth: true, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "unknown field", method: http.MethodPut, path: routeAdminPath(routeAdminTestRoute, routeAdminBind), contentType: "application/json", body: append(valid[:len(valid)-1], []byte(`,"unexpected":true}`)...), auth: true, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "trailing value", method: http.MethodPut, path: routeAdminPath(routeAdminTestRoute, routeAdminBind), contentType: "application/json", body: append(valid, []byte(` {}`)...), auth: true, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "oversized", method: http.MethodPut, path: routeAdminPath(routeAdminTestRoute, routeAdminBind), contentType: "application/json", body: bytes.Repeat([]byte("x"), routeAdminMaxRequestBytes+1), auth: true, status: http.StatusRequestEntityTooLarge, code: "request_too_large"},
		{name: "path body mismatch", method: http.MethodPut, path: routeAdminPath("route-b", routeAdminBind), contentType: "application/json", body: valid, auth: true, status: http.StatusBadRequest, code: "invalid_request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var response *httptest.ResponseRecorder
			if test.name == "content encoding" {
				request := routeAdminHTTPRequest(test.method, test.path, test.contentType, test.body, test.auth)
				request.Header.Set("Content-Encoding", "gzip")
				response = httptest.NewRecorder()
				harness.handler.ServeHTTP(response, request)
			} else {
				response = routeAdminRawRequest(harness.handler, test.method, test.path, test.contentType, test.body, test.auth)
			}
			assertRouteAdminErrorResponse(t, response, test.status, test.code)
		})
	}
}

func TestRouteAdminStateMatrixFailsClosed(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(*testing.T, *routeAdminHarness) nodeledger.Route
		action     routeAdminAction
		expected   nodeledger.State
		wantStatus int
	}{
		{
			name: "drain attaching", action: routeAdminDrain, expected: nodeledger.StateReady, wantStatus: http.StatusConflict,
			prepare: func(t *testing.T, h *routeAdminHarness) nodeledger.Route { return routeAdminBindDirect(t, h) },
		},
		{
			name: "standby ready", action: routeAdminStandby, expected: nodeledger.StateDraining, wantStatus: http.StatusConflict,
			prepare: func(t *testing.T, h *routeAdminHarness) nodeledger.Route {
				bound := routeAdminBindDirect(t, h)
				ready, err := h.ledger.Transition(bound.RouteID, bound.Generation, nodeledger.StateAttaching, nodeledger.StateReady)
				if err != nil {
					t.Fatal(err)
				}
				return ready.Public()
			},
		},
		{
			name: "attach ready from standby", action: routeAdminAttachReady, expected: nodeledger.StateAttaching, wantStatus: http.StatusConflict,
			prepare: func(t *testing.T, h *routeAdminHarness) nodeledger.Route { return routeAdminStandbyDirect(t, h) },
		},
		{
			name: "rebind ready", action: routeAdminRebind, expected: nodeledger.StateStandby, wantStatus: http.StatusConflict,
			prepare: func(t *testing.T, h *routeAdminHarness) nodeledger.Route {
				bound := routeAdminBindDirect(t, h)
				ready, err := h.ledger.Transition(bound.RouteID, bound.Generation, nodeledger.StateAttaching, nodeledger.StateReady)
				if err != nil {
					t.Fatal(err)
				}
				return ready.Public()
			},
		},
		{
			name: "remove attaching", action: routeAdminRemove, expected: nodeledger.StateReleased, wantStatus: http.StatusConflict,
			prepare: func(t *testing.T, h *routeAdminHarness) nodeledger.Route { return routeAdminBindDirect(t, h) },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newRouteAdminHarness(t)
			route := test.prepare(t, harness)
			if test.action == routeAdminRebind {
				request := RouteRebindRequest{
					RouteID: route.RouteID, ProjectID: route.ProjectID, SandboxID: route.SandboxID,
					Generation: route.Generation, ExpectedState: test.expected, EngineID: "replacement-engine",
				}
				body, _ := json.Marshal(request)
				response := routeAdminRawRequest(harness.handler, http.MethodPost, routeAdminPath(route.RouteID, test.action), "application/json", body, true)
				assertRouteAdminErrorResponse(t, response, test.wantStatus, "route_conflict")
				return
			}
			request := routeAdminTransition(route, test.expected)
			body, _ := json.Marshal(request)
			response := routeAdminRawRequest(harness.handler, http.MethodPost, routeAdminPath(route.RouteID, test.action), "application/json", body, true)
			assertRouteAdminErrorResponse(t, response, test.wantStatus, "route_conflict")
		})
	}
}

func TestRouteAdminReleasePermittedSources(t *testing.T) {
	for _, source := range []nodeledger.State{nodeledger.StateAttaching, nodeledger.StateDraining, nodeledger.StateStandby} {
		t.Run(string(source), func(t *testing.T) {
			harness := newRouteAdminHarness(t)
			var route nodeledger.Route
			switch source {
			case nodeledger.StateAttaching:
				route = routeAdminBindDirect(t, harness)
			case nodeledger.StateDraining:
				route = routeAdminBindDirect(t, harness)
				_, err := harness.ledger.Transition(route.RouteID, route.Generation, nodeledger.StateAttaching, nodeledger.StateReady)
				if err != nil {
					t.Fatal(err)
				}
				draining, err := harness.ledger.Transition(route.RouteID, route.Generation, nodeledger.StateReady, nodeledger.StateDraining)
				if err != nil {
					t.Fatal(err)
				}
				route = draining.Public()
			case nodeledger.StateStandby:
				route = routeAdminStandbyDirect(t, harness)
			}
			result, err := harness.client.Release(context.Background(), routeAdminTransition(route, source))
			if err != nil || result.Route.State != nodeledger.StateReleased {
				t.Fatalf("release result=%#v error=%v", result, err)
			}
		})
	}
}

func TestRouteAdminRejectsWrongIdentityGenerationAndState(t *testing.T) {
	harness := newRouteAdminHarness(t)
	bound, err := harness.client.Bind(context.Background(), RouteBindRequest{
		RouteID: routeAdminTestRoute, ProjectID: routeAdminTestProject,
		SandboxID: routeAdminTestSandbox, EngineID: routeAdminTestEngine,
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		request RouteTransitionRequest
		status  int
		code    string
	}{
		{name: "wrong project", request: RouteTransitionRequest{RouteID: bound.Route.RouteID, ProjectID: "project-b", SandboxID: bound.Route.SandboxID, Generation: bound.Route.Generation, ExpectedState: nodeledger.StateAttaching}, status: http.StatusNotFound, code: "route_not_found"},
		{name: "wrong sandbox", request: RouteTransitionRequest{RouteID: bound.Route.RouteID, ProjectID: bound.Route.ProjectID, SandboxID: "sandbox-b", Generation: bound.Route.Generation, ExpectedState: nodeledger.StateAttaching}, status: http.StatusNotFound, code: "route_not_found"},
		{name: "stale generation", request: RouteTransitionRequest{RouteID: bound.Route.RouteID, ProjectID: bound.Route.ProjectID, SandboxID: bound.Route.SandboxID, Generation: bound.Route.Generation + 1, ExpectedState: nodeledger.StateAttaching}, status: http.StatusConflict, code: "route_conflict"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, _ := json.Marshal(test.request)
			response := routeAdminRawRequest(harness.handler, http.MethodPost, routeAdminPath(routeAdminTestRoute, routeAdminAttachReady), "application/json", body, true)
			assertRouteAdminErrorResponse(t, response, test.status, test.code)
		})
	}
	invalidState := RouteTransitionRequest{
		RouteID: routeAdminTestRoute, ProjectID: routeAdminTestProject, SandboxID: routeAdminTestSandbox,
		Generation: bound.Route.Generation, ExpectedState: nodeledger.StateReady,
	}
	body, _ := json.Marshal(invalidState)
	response := routeAdminRawRequest(harness.handler, http.MethodPost, routeAdminPath(routeAdminTestRoute, routeAdminAttachReady), "application/json", body, true)
	assertRouteAdminErrorResponse(t, response, http.StatusBadRequest, "invalid_request")

	// A syntactically valid but illegal lifecycle source is a state conflict,
	// while action/source combinations invalid on the wire are bad requests.
	if _, err := harness.client.AttachReady(context.Background(), routeAdminTransition(bound.Route, nodeledger.StateAttaching)); err != nil {
		t.Fatal(err)
	}
	body, _ = json.Marshal(routeAdminTransition(bound.Route, nodeledger.StateAttaching))
	response = routeAdminRawRequest(harness.handler, http.MethodPost, routeAdminPath(routeAdminTestRoute, routeAdminAttachReady), "application/json", body, true)
	if response.Code != http.StatusOK {
		t.Fatalf("idempotent ready retry status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestRouteAdminConcurrentIdempotentTransition(t *testing.T) {
	harness := newRouteAdminHarness(t)
	bound, err := harness.client.Bind(context.Background(), RouteBindRequest{
		RouteID: routeAdminTestRoute, ProjectID: routeAdminTestProject,
		SandboxID: routeAdminTestSandbox, EngineID: routeAdminTestEngine,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := routeAdminTransition(bound.Route, nodeledger.StateAttaching)
	const workers = 24
	results := make(chan RouteAdminResult, workers)
	errorsFound := make(chan error, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			result, callErr := harness.client.AttachReady(context.Background(), request)
			if callErr != nil {
				errorsFound <- callErr
				return
			}
			results <- result
		}()
	}
	group.Wait()
	close(results)
	close(errorsFound)
	for callErr := range errorsFound {
		t.Errorf("concurrent transition failed: %v", callErr)
	}
	nonReplayed := 0
	count := 0
	for result := range results {
		count++
		if !result.Replayed {
			nonReplayed++
		}
		if result.Route.Generation != bound.Route.Generation || result.Route.State != nodeledger.StateReady {
			t.Errorf("unexpected concurrent result: %#v", result)
		}
	}
	if count != workers || nonReplayed != 1 {
		t.Fatalf("results=%d non-replayed=%d", count, nonReplayed)
	}
}

func TestRouteAdminNeverExposesPrivateEngineIdentity(t *testing.T) {
	harness := newRouteAdminHarness(t)
	request := RouteBindRequest{
		RouteID: routeAdminTestRoute, ProjectID: routeAdminTestProject,
		SandboxID: routeAdminTestSandbox, EngineID: routeAdminTestEngine,
	}
	body, _ := json.Marshal(request)
	response := routeAdminRawRequest(harness.handler, http.MethodPut, routeAdminPath(routeAdminTestRoute, routeAdminBind), "application/json", body, true)
	if response.Code != http.StatusOK {
		t.Fatalf("bind status=%d body=%q", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), routeAdminTestEngine) || strings.Contains(response.Body.String(), "engine_id") {
		t.Fatalf("private engine identity leaked in success: %q", response.Body.String())
	}
	request.EngineID = "private-engine-credential-b"
	body, _ = json.Marshal(request)
	response = routeAdminRawRequest(harness.handler, http.MethodPut, routeAdminPath(routeAdminTestRoute, routeAdminBind), "application/json", body, true)
	assertRouteAdminErrorResponse(t, response, http.StatusConflict, "route_conflict")
	if strings.Contains(response.Body.String(), request.EngineID) || strings.Contains(response.Body.String(), routeAdminTestEngine) {
		t.Fatalf("private engine identity leaked in conflict: %q", response.Body.String())
	}
}

func TestRouteAdminClientRejectsUntrustedResponses(t *testing.T) {
	validRoute := nodeledger.Route{
		RouteID: routeAdminTestRoute, ProjectID: routeAdminTestProject, SandboxID: routeAdminTestSandbox,
		Generation: 1, State: nodeledger.StateAttaching, LastActivity: routeAdminTestTime,
	}
	tests := []struct {
		name string
		body []byte
	}{
		{name: "wrong node", body: mustRouteAdminJSON(t, RouteAdminResult{NodeID: "node-b", Route: validRoute})},
		{name: "wrong route", body: mustRouteAdminJSON(t, RouteAdminResult{NodeID: routeAdminTestNode, Route: func() nodeledger.Route { changed := validRoute; changed.RouteID = "route-b"; return changed }()})},
		{name: "private field", body: []byte(`{"node_id":"node-a","route":{"route_id":"route-a","project_id":"project-a","sandbox_id":"sandbox-a","generation":1,"state":"attaching","last_activity":"2026-09-14T12:00:00Z","engine_id":"secret"},"replayed":false}`)},
		{name: "trailing", body: append(mustRouteAdminJSON(t, RouteAdminResult{NodeID: routeAdminTestNode, Route: validRoute}), []byte(` {}`)...)},
		{name: "oversized", body: bytes.Repeat([]byte("x"), routeAdminMaxResponseBytes+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(test.body)), Request: request}, nil
			})
			client, err := newRouteAdminClient("https://node.test", &http.Client{Transport: transport}, routeAdminTestNode)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Bind(context.Background(), RouteBindRequest{
				RouteID: routeAdminTestRoute, ProjectID: routeAdminTestProject,
				SandboxID: routeAdminTestSandbox, EngineID: routeAdminTestEngine,
			})
			if err == nil {
				t.Fatal("untrusted response was accepted")
			}
			if strings.Contains(err.Error(), routeAdminTestEngine) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("client error leaked private content: %v", err)
			}
		})
	}
}

func TestRouteAdminClientErrorIsBoundedAndContentMinimal(t *testing.T) {
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusConflict,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"route_conflict"}}`)),
			Request:    request,
		}, nil
	})
	client, err := newRouteAdminClient("https://node.test", &http.Client{Transport: transport}, routeAdminTestNode)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Bind(context.Background(), RouteBindRequest{
		RouteID: routeAdminTestRoute, ProjectID: routeAdminTestProject,
		SandboxID: routeAdminTestSandbox, EngineID: routeAdminTestEngine,
	})
	if !routeAdminErrorIs(err, http.StatusConflict, "route_conflict") {
		t.Fatalf("unexpected typed error: %v", err)
	}
	if strings.Contains(err.Error(), routeAdminTestEngine) {
		t.Fatalf("private engine identity leaked in client error: %v", err)
	}

	oversized := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("s"), routeAdminMaxResponseBytes+1))), Request: request}, nil
	})
	client, err = newRouteAdminClient("https://node.test", &http.Client{Transport: oversized}, routeAdminTestNode)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Bind(context.Background(), RouteBindRequest{
		RouteID: routeAdminTestRoute, ProjectID: routeAdminTestProject,
		SandboxID: routeAdminTestSandbox, EngineID: routeAdminTestEngine,
	})
	if !routeAdminErrorIs(err, http.StatusBadGateway, "unexpected_response") {
		t.Fatalf("oversized error response was not reduced: %v", err)
	}
}

func TestRouteAdminConstructorsFailClosed(t *testing.T) {
	if _, err := NewRouteAdminHandler(RouteAdminHandlerConfig{}); err == nil {
		t.Fatal("handler accepted empty config")
	}
	if _, err := newRouteAdminClient("http://node.test", &http.Client{Transport: http.DefaultTransport}, routeAdminTestNode); err == nil {
		t.Fatal("client accepted plaintext base URL")
	}
	if _, err := newRouteAdminClient("https://node.test", &http.Client{}, routeAdminTestNode); err == nil {
		t.Fatal("client accepted ambient default transport")
	}
	if _, err := newRouteAdminClient("https://node.test", &http.Client{Transport: http.DefaultTransport}, "NODE A"); err == nil {
		t.Fatal("client accepted invalid node identity")
	}
	if _, err := NewRouteAdminClient("https://node.test", &tls.Config{}, routeAdminTestNode); err == nil {
		t.Fatal("public client accepted incomplete TLS config")
	}
	harness := newRouteAdminHarness(t)
	if harness.client.client.CheckRedirect == nil {
		t.Fatal("client did not install redirect denial")
	}
	redirectRequest := httptest.NewRequest(http.MethodGet, "https://elsewhere.test", nil)
	if err := harness.client.client.CheckRedirect(redirectRequest, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy error=%v", err)
	}
}

func TestRouteAdminFailsClosedWhenLedgerBecomesUnavailable(t *testing.T) {
	harness := newRouteAdminHarness(t)
	if err := harness.ledger.Close(); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(RouteBindRequest{
		RouteID: routeAdminTestRoute, ProjectID: routeAdminTestProject,
		SandboxID: routeAdminTestSandbox, EngineID: routeAdminTestEngine,
	})
	response := routeAdminRawRequest(harness.handler, http.MethodPut, routeAdminPath(routeAdminTestRoute, routeAdminBind), "application/json", body, true)
	assertRouteAdminErrorResponse(t, response, http.StatusServiceUnavailable, "node_unavailable")
}

func routeAdminTransition(route nodeledger.Route, expected nodeledger.State) RouteTransitionRequest {
	return RouteTransitionRequest{
		RouteID: route.RouteID, ProjectID: route.ProjectID, SandboxID: route.SandboxID,
		Generation: route.Generation, ExpectedState: expected,
	}
}

func routeAdminBindDirect(t *testing.T, harness *routeAdminHarness) nodeledger.Route {
	t.Helper()
	binding, err := harness.ledger.Bind(routeAdminTestRoute, routeAdminTestProject, routeAdminTestSandbox, routeAdminTestEngine, routeAdminTestTime)
	if err != nil {
		t.Fatal(err)
	}
	return binding.Public()
}

func routeAdminStandbyDirect(t *testing.T, harness *routeAdminHarness) nodeledger.Route {
	t.Helper()
	route := routeAdminBindDirect(t, harness)
	_, err := harness.ledger.Transition(route.RouteID, route.Generation, nodeledger.StateAttaching, nodeledger.StateReady)
	if err != nil {
		t.Fatal(err)
	}
	_, err = harness.ledger.Transition(route.RouteID, route.Generation, nodeledger.StateReady, nodeledger.StateDraining)
	if err != nil {
		t.Fatal(err)
	}
	standby, err := harness.ledger.Transition(route.RouteID, route.Generation, nodeledger.StateDraining, nodeledger.StateStandby)
	if err != nil {
		t.Fatal(err)
	}
	return standby.Public()
}

func routeAdminHTTPRequest(method, path, contentType string, body []byte, authenticated bool) *http.Request {
	request := httptest.NewRequest(method, "https://node.test"+path, bytes.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if authenticated {
		certificate := &x509.Certificate{Raw: []byte("verified-api-peer")}
		request.TLS = &tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{certificate},
			VerifiedChains:   [][]*x509.Certificate{{certificate}},
		}
	} else {
		request.TLS = nil
	}
	return request
}

func routeAdminRawRequest(handler http.Handler, method, path, contentType string, body []byte, authenticated bool) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, routeAdminHTTPRequest(method, path, contentType, body, authenticated))
	return response
}

func assertRouteAdminErrorResponse(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status=%d want=%d body=%q", response.Code, status, response.Body.String())
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != code {
		t.Fatalf("error response=%q decode=%v", response.Body.String(), err)
	}
}

func routeAdminErrorIs(err error, status int, code string) bool {
	var adminErr *RouteAdminError
	return errors.As(err, &adminErr) && adminErr.StatusCode == status && adminErr.Code == code
}

func mustRouteAdminJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

type routeAdminHandlerTransport struct {
	handler http.Handler
}

func (t *routeAdminHandlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	certificate := &x509.Certificate{Raw: []byte("verified-api-peer")}
	request.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{certificate},
		VerifiedChains:   [][]*x509.Certificate{{certificate}},
	}
	response := httptest.NewRecorder()
	t.handler.ServeHTTP(response, request)
	return response.Result(), nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
