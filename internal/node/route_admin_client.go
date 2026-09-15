package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/infercrane/brezel/internal/nodeidentity"
	"github.com/infercrane/brezel/internal/nodeledger"
)

var routeAdminErrorCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// RouteAdminError is a content-minimal node-control failure. It deliberately
// excludes request bodies, route identities, and private engine identifiers.
type RouteAdminError struct {
	StatusCode int
	Code       string
}

func (e *RouteAdminError) Error() string {
	if e == nil {
		return "node route admin request failed"
	}
	return fmt.Sprintf("node route admin request failed with status %d (%s)", e.StatusCode, e.Code)
}

// RouteAdminClient applies durable-controller lifecycle decisions to one
// exact node. It is a private control client, not a public product API.
type RouteAdminClient struct {
	baseURL        *url.URL
	client         *http.Client
	expectedNodeID string
}

// Ready verifies the exact node control listener and its generation ledger.
// A healthy data relay is insufficient because lifecycle changes cannot be
// fenced safely while this endpoint is unavailable.
func (c *RouteAdminClient) Ready(ctx context.Context) error {
	var result struct {
		Status string `json:"status"`
		NodeID string `json:"node_id"`
	}
	if err := c.call(ctx, http.MethodGet, routeAdminReadyPath, nil, &result); err != nil {
		return err
	}
	if result.Status != "ready" || result.NodeID != c.expectedNodeID {
		return errors.New("node route admin readiness response did not match the expected node")
	}
	return nil
}

// Resolve returns the current public route assignment from the exact node.
// It is used only for reconciliation; private engine identity remains local.
func (c *RouteAdminClient) Resolve(ctx context.Context, routeID string) (RouteAdminResult, error) {
	if !routeAdminIDPattern.MatchString(routeID) {
		return RouteAdminResult{}, errors.New("invalid route identity")
	}
	var result RouteAdminResult
	if err := c.call(ctx, http.MethodGet, routeAdminPath(routeID, routeAdminBind), nil, &result); err != nil {
		return RouteAdminResult{}, err
	}
	if result.NodeID != c.expectedNodeID || !validRouteAdminPublicRoute(result.Route) || result.Route.RouteID != routeID {
		return RouteAdminResult{}, errors.New("node route admin response did not match the expected node and route")
	}
	return result, nil
}

// NewRouteAdminClient constructs a bounded, redirect-free mTLS client.
func NewRouteAdminClient(baseURL string, tlsConfig *tls.Config, expectedNodeID string) (*RouteAdminClient, error) {
	client, err := nodeidentity.NewControlHTTPClient(tlsConfig)
	if err != nil {
		return nil, err
	}
	return newRouteAdminClient(baseURL, client, expectedNodeID)
}

// newRouteAdminClient accepts an injected client for hermetic protocol tests.
// Release wiring must use NewRouteAdminClient so mTLS cannot be skipped.
func newRouteAdminClient(baseURL string, client *http.Client, expectedNodeID string) (*RouteAdminClient, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("node route admin requires an absolute HTTPS base URL without credentials, query, or fragment")
	}
	if client == nil || client.Transport == nil {
		return nil, errors.New("node route admin HTTP client is required")
	}
	if _, err := nodeidentity.NewIdentity(nodeidentity.RoleNode, expectedNodeID); err != nil {
		return nil, errors.New("valid expected node identity is required")
	}
	copyClient := *client
	if copyClient.Timeout <= 0 || copyClient.Timeout > 30*time.Second {
		copyClient.Timeout = 30 * time.Second
	}
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	copyURL := *parsed
	copyURL.Path = strings.TrimSuffix(copyURL.Path, "/")
	copyURL.RawPath = ""
	return &RouteAdminClient{baseURL: &copyURL, client: &copyClient, expectedNodeID: expectedNodeID}, nil
}

func (c *RouteAdminClient) Bind(ctx context.Context, request RouteBindRequest) (RouteAdminResult, error) {
	if err := validateRouteBindRequest(request, request.RouteID); err != nil {
		return RouteAdminResult{}, err
	}
	var result RouteAdminResult
	if err := c.call(ctx, http.MethodPut, routeAdminPath(request.RouteID, routeAdminBind), request, &result); err != nil {
		return RouteAdminResult{}, err
	}
	if err := c.validateResult(result, request.RouteID, request.ProjectID, request.SandboxID); err != nil {
		return RouteAdminResult{}, err
	}
	if result.Route.State != nodeledger.StateAttaching && !(result.Replayed && result.Route.State == nodeledger.StateReady) {
		return RouteAdminResult{}, errors.New("node route admin bind response has an invalid state")
	}
	return result, nil
}

func (c *RouteAdminClient) AttachReady(ctx context.Context, request RouteTransitionRequest) (RouteAdminResult, error) {
	return c.transition(ctx, routeAdminAttachReady, nodeledger.StateReady, request)
}

func (c *RouteAdminClient) Drain(ctx context.Context, request RouteTransitionRequest) (RouteAdminResult, error) {
	return c.transition(ctx, routeAdminDrain, nodeledger.StateDraining, request)
}

func (c *RouteAdminClient) Standby(ctx context.Context, request RouteTransitionRequest) (RouteAdminResult, error) {
	return c.transition(ctx, routeAdminStandby, nodeledger.StateStandby, request)
}

func (c *RouteAdminClient) Release(ctx context.Context, request RouteTransitionRequest) (RouteAdminResult, error) {
	return c.transition(ctx, routeAdminRelease, nodeledger.StateReleased, request)
}

func (c *RouteAdminClient) transition(ctx context.Context, action routeAdminAction, target nodeledger.State, request RouteTransitionRequest) (RouteAdminResult, error) {
	if err := validateRouteTransitionRequest(request, request.RouteID, action); err != nil {
		return RouteAdminResult{}, err
	}
	var result RouteAdminResult
	if err := c.call(ctx, http.MethodPost, routeAdminPath(request.RouteID, action), request, &result); err != nil {
		return RouteAdminResult{}, err
	}
	if err := c.validateResult(result, request.RouteID, request.ProjectID, request.SandboxID); err != nil {
		return RouteAdminResult{}, err
	}
	if result.Route.Generation != request.Generation || result.Route.State != target {
		return RouteAdminResult{}, errors.New("node route admin transition response did not match the requested assignment")
	}
	return result, nil
}

func (c *RouteAdminClient) Rebind(ctx context.Context, request RouteRebindRequest) (RouteAdminResult, error) {
	if err := validateRouteRebindRequest(request, request.RouteID); err != nil {
		return RouteAdminResult{}, err
	}
	var result RouteAdminResult
	if err := c.call(ctx, http.MethodPost, routeAdminPath(request.RouteID, routeAdminRebind), request, &result); err != nil {
		return RouteAdminResult{}, err
	}
	if err := c.validateResult(result, request.RouteID, request.ProjectID, request.SandboxID); err != nil {
		return RouteAdminResult{}, err
	}
	if result.Replayed || result.Route.Generation <= request.Generation || result.Route.State != nodeledger.StateAttaching {
		return RouteAdminResult{}, errors.New("node route admin rebind response did not contain a fresh attaching assignment")
	}
	return result, nil
}

func (c *RouteAdminClient) Remove(ctx context.Context, request RouteTransitionRequest) (RouteRemoveResult, error) {
	if err := validateRouteTransitionRequest(request, request.RouteID, routeAdminRemove); err != nil {
		return RouteRemoveResult{}, err
	}
	var result RouteRemoveResult
	if err := c.call(ctx, http.MethodPost, routeAdminPath(request.RouteID, routeAdminRemove), request, &result); err != nil {
		return RouteRemoveResult{}, err
	}
	if result.NodeID != c.expectedNodeID || result.RouteID != request.RouteID || result.ProjectID != request.ProjectID ||
		result.SandboxID != request.SandboxID || result.Generation != request.Generation {
		return RouteRemoveResult{}, errors.New("node route admin remove response did not match the requested assignment")
	}
	return result, nil
}

func (c *RouteAdminClient) validateResult(result RouteAdminResult, routeID, projectID, sandboxID string) error {
	if result.NodeID != c.expectedNodeID || !validRouteAdminPublicRoute(result.Route) || result.Route.RouteID != routeID ||
		result.Route.ProjectID != projectID || result.Route.SandboxID != sandboxID {
		return errors.New("node route admin response did not match the expected node and route")
	}
	return nil
}

func (c *RouteAdminClient) call(ctx context.Context, method, endpoint string, payload, destination any) error {
	if c == nil || c.baseURL == nil || c.client == nil {
		return errors.New("node route admin client is not initialized")
	}
	var encoded []byte
	if payload != nil {
		var err error
		encoded, err = json.Marshal(payload)
		if err != nil {
			return errors.New("encode node route admin request")
		}
		if len(encoded) == 0 || len(encoded) > routeAdminMaxRequestBytes {
			return errors.New("node route admin request exceeds its limit")
		}
	}
	target := *c.baseURL
	target.Path = strings.TrimSuffix(target.Path, "/") + endpoint
	target.RawPath = ""
	request, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(encoded))
	if err != nil {
		return errors.New("create node route admin request")
	}
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("call node route admin endpoint: %w", err)
	}
	defer response.Body.Close()
	if !validRouteAdminContentType(response.Header.Get("Content-Type")) {
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return &RouteAdminError{StatusCode: response.StatusCode, Code: "unexpected_response"}
		}
		return errors.New("node route admin response has an invalid content type")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeRouteAdminError(response)
	}
	if err := decodeRouteAdminResponse(response.Body, destination); err != nil {
		return err
	}
	return nil
}

func routeAdminPath(routeID string, action routeAdminAction) string {
	path := routeAdminPrefix + url.PathEscape(routeID)
	if action != routeAdminBind {
		path += "/" + string(action)
	}
	return path
}

func decodeRouteAdminResponse(body io.Reader, destination any) error {
	data, err := io.ReadAll(io.LimitReader(body, routeAdminMaxResponseBytes+1))
	if err != nil {
		return errors.New("read node route admin response")
	}
	if len(data) == 0 || len(data) > routeAdminMaxResponseBytes {
		return errors.New("node route admin response is empty or exceeds its limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("decode node route admin response")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("node route admin response contains trailing JSON")
	}
	return nil
}

func decodeRouteAdminError(response *http.Response) error {
	type errorEnvelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	var envelope errorEnvelope
	code := "unexpected_response"
	if err := decodeRouteAdminResponse(response.Body, &envelope); err == nil && routeAdminErrorCodePattern.MatchString(envelope.Error.Code) {
		code = envelope.Error.Code
	}
	return &RouteAdminError{StatusCode: response.StatusCode, Code: code}
}
