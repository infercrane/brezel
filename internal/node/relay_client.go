package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/brezel/internal/backend"
	"github.com/infercrane/brezel/internal/nodeidentity"
	"github.com/infercrane/brezel/internal/nodeledger"
)

const (
	relayClientMaxRouteResponse   = 64 << 10
	relayClientMaxCommandResponse = 96 << 20
	relayClientMaxCommandOutput   = 64 << 20
	relayClientMaxFileUpload      = 32 << 20
	relayClientMaxFileDownload    = 64 << 20
	relayClientMaxPortRequest     = 32 << 20
	relayClientMaxPortResponse    = 64 << 20
	relayClientDefaultCommandTime = 5 * time.Minute
	relayClientMaxOperationTime   = time.Hour
)

var (
	relayRouteIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	errRelayBodyLimit   = errors.New("node relay body exceeded its authorized limit")
)

// RelayDataPlane is the API-side half of the node relay. The durable service
// keeps authorizing product resources, while this client exchanges that
// decision for one short-lived, request-bound operation capability.
type RelayDataPlane struct {
	baseURL  *url.URL
	client   *http.Client
	signer   *CapabilitySigner
	audience string
	nodeID   string
}

func (*RelayDataPlane) ownedNodeDataPlane() {}

// NewRelayDataPlane requires an HTTPS endpoint and constructs the relay client
// through the hardened mTLS identity boundary. Redirects are disabled so
// credentials cannot cross origins.
func NewRelayDataPlane(baseURL string, tlsConfig *tls.Config, signer *CapabilitySigner, audience, nodeID string) (*RelayDataPlane, error) {
	client, err := nodeidentity.NewDataHTTPClient(tlsConfig)
	if err != nil {
		return nil, err
	}
	return newRelayDataPlane(baseURL, client, signer, audience, nodeID)
}

// newRelayDataPlane accepts an injected client for hermetic protocol tests.
// Release wiring must use NewRelayDataPlane so TLS policy cannot be skipped.
func newRelayDataPlane(baseURL string, client *http.Client, signer *CapabilitySigner, audience, nodeID string) (*RelayDataPlane, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("node relay requires an absolute HTTPS base URL without credentials, query, or fragment")
	}
	if client == nil || client.Transport == nil {
		return nil, errors.New("node relay HTTP client is required")
	}
	if signer == nil {
		return nil, errors.New("node capability signer is required")
	}
	for name, value := range map[string]string{"audience": audience, "node id": nodeID} {
		if err := validateIdentity(name, value); err != nil {
			return nil, err
		}
	}
	copyClient := *client
	copyClient.Timeout = 0
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	copyURL := *parsed
	copyURL.Path = strings.TrimSuffix(copyURL.Path, "/")
	return &RelayDataPlane{baseURL: &copyURL, client: &copyClient, signer: signer, audience: audience, nodeID: nodeID}, nil
}

func (d *RelayDataPlane) Capabilities() Capabilities {
	if d == nil || d.baseURL == nil || d.client == nil || d.signer == nil {
		return Capabilities{}
	}
	return Capabilities{CommandStreaming: true, FileReadWrite: true, AuthenticatedPorts: true}
}

func (d *RelayDataPlane) Run(ctx context.Context, binding SandboxBinding, request backend.CommandRequest, emit func(backend.CommandEvent) error) error {
	if emit == nil {
		return errors.New("command event callback is required")
	}
	resolved, err := d.authorizedRoute(ctx, binding)
	if err != nil {
		return err
	}
	protocolRequest := commandProtocolRequest(request)
	canonical, err := canonicalRelayJSON(protocolRequest)
	if err != nil {
		return fmt.Errorf("encode node command request: %w", err)
	}
	duration := relayOperationDuration(ctx, relayClientDefaultCommandTime)
	token, err := d.issue(binding, resolved, CapabilityRunCommand, canonical, CapabilityBounds{
		MaxDurationMillis: duration.Milliseconds(),
		MaxResponseBytes:  relayClientMaxCommandOutput,
	})
	if err != nil {
		return err
	}
	httpRequest, err := d.request(ctx, http.MethodPost, "/v1/commands", bytes.NewReader(canonical), resolved.Route.RouteID, token)
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := d.client.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("call node command relay: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return relayResponseError(response)
	}
	reader := &relayBoundedReader{source: response.Body, remaining: relayClientMaxCommandResponse}
	decoder := json.NewDecoder(reader)
	var outputBytes int64
	exited := false
	for {
		var event backend.CommandEvent
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("decode node command stream: %w", err)
		}
		switch event.Type {
		case backend.CommandStarted:
		case backend.CommandStdout, backend.CommandStderr:
			outputBytes += int64(len(event.Data))
			if outputBytes > relayClientMaxCommandOutput {
				return errRelayBodyLimit
			}
		case backend.CommandExited:
			if exited {
				return errors.New("node command stream contains multiple exit events")
			}
			exited = true
		default:
			return errors.New("node command stream contains an unsupported event")
		}
		if err := emit(event); err != nil {
			return err
		}
	}
	if !exited {
		return errors.New("node command stream ended without an exit event")
	}
	return nil
}

func (d *RelayDataPlane) WriteFile(ctx context.Context, binding SandboxBinding, path string, source io.Reader) (backend.FileInfo, error) {
	if source == nil {
		return backend.FileInfo{}, errors.New("file source is required")
	}
	resolved, err := d.authorizedRoute(ctx, binding)
	if err != nil {
		return backend.FileInfo{}, err
	}
	data, err := io.ReadAll(io.LimitReader(source, relayClientMaxFileUpload+1))
	if err != nil {
		return backend.FileInfo{}, fmt.Errorf("read file for node relay: %w", err)
	}
	if len(data) > relayClientMaxFileUpload {
		return backend.FileInfo{}, errRelayBodyLimit
	}
	descriptor := fileWriteDescriptor(path, data)
	canonical, err := canonicalRelayJSON(descriptor)
	if err != nil {
		return backend.FileInfo{}, fmt.Errorf("encode node file descriptor: %w", err)
	}
	bound := int64(len(data))
	if bound == 0 {
		bound = 1
	}
	token, err := d.issue(binding, resolved, CapabilityWriteFile, canonical, CapabilityBounds{MaxRequestBytes: bound})
	if err != nil {
		return backend.FileInfo{}, err
	}
	endpoint := "/v1/files?path=" + url.QueryEscape(path)
	httpRequest, err := d.request(ctx, http.MethodPut, endpoint, bytes.NewReader(data), resolved.Route.RouteID, token)
	if err != nil {
		return backend.FileInfo{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/octet-stream")
	httpRequest.Header.Set(RelayContentSHAHeader, descriptor.SHA256)
	response, err := d.client.Do(httpRequest)
	if err != nil {
		return backend.FileInfo{}, fmt.Errorf("call node file write relay: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return backend.FileInfo{}, relayResponseError(response)
	}
	var info backend.FileInfo
	if err := decodeRelayJSON(response.Body, relayClientMaxRouteResponse, &info); err != nil {
		return backend.FileInfo{}, fmt.Errorf("decode node file write response: %w", err)
	}
	if info.Path != path || info.Size != int64(len(data)) {
		return backend.FileInfo{}, errors.New("node file write response did not match the authorized request")
	}
	return info, nil
}

func (d *RelayDataPlane) ReadFile(ctx context.Context, binding SandboxBinding, path string, destination io.Writer) (backend.FileInfo, error) {
	if destination == nil {
		return backend.FileInfo{}, errors.New("file destination is required")
	}
	resolved, err := d.authorizedRoute(ctx, binding)
	if err != nil {
		return backend.FileInfo{}, err
	}
	descriptor := relayFileDescriptor{Path: path}
	canonical, err := canonicalRelayJSON(descriptor)
	if err != nil {
		return backend.FileInfo{}, fmt.Errorf("encode node file descriptor: %w", err)
	}
	token, err := d.issue(binding, resolved, CapabilityReadFile, canonical, CapabilityBounds{MaxResponseBytes: relayClientMaxFileDownload})
	if err != nil {
		return backend.FileInfo{}, err
	}
	endpoint := "/v1/files?path=" + url.QueryEscape(path)
	httpRequest, err := d.request(ctx, http.MethodGet, endpoint, nil, resolved.Route.RouteID, token)
	if err != nil {
		return backend.FileInfo{}, err
	}
	response, err := d.client.Do(httpRequest)
	if err != nil {
		return backend.FileInfo{}, fmt.Errorf("call node file read relay: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return backend.FileInfo{}, relayResponseError(response)
	}
	responsePath := response.Header.Get(RelayFilePathHeader)
	if responsePath != path {
		return backend.FileInfo{}, errors.New("node file read response did not match the authorized path")
	}
	written, err := io.Copy(destination, &relayBoundedReader{source: response.Body, remaining: relayClientMaxFileDownload})
	if err != nil {
		return backend.FileInfo{}, fmt.Errorf("stream node file response: %w", err)
	}
	if encodedSize := response.Header.Get(RelayFileSizeHeader); encodedSize != "" {
		reported, parseErr := strconv.ParseInt(encodedSize, 10, 64)
		if parseErr != nil || reported < 0 || reported != written {
			return backend.FileInfo{}, errors.New("node file read response has an invalid size")
		}
	}
	return backend.FileInfo{Path: path, ContentType: response.Header.Get("Content-Type"), Size: written}, nil
}

func (d *RelayDataPlane) ValidatePort(port uint16) error {
	if d == nil || d.Capabilities().AuthenticatedPorts == false {
		return backend.ErrCapabilityUnavailable
	}
	if port == 0 {
		return errors.New("port must be between 1 and 65535")
	}
	return nil
}

func (d *RelayDataPlane) RoundTripPort(ctx context.Context, binding SandboxBinding, port uint16, incoming *http.Request) (*http.Response, error) {
	if err := d.ValidatePort(port); err != nil {
		return nil, err
	}
	if incoming == nil || incoming.URL == nil {
		return nil, errors.New("port request is required")
	}
	resolved, err := d.authorizedRoute(ctx, binding)
	if err != nil {
		return nil, err
	}
	var body []byte
	if incoming.Body != nil {
		body, err = io.ReadAll(io.LimitReader(incoming.Body, relayClientMaxPortRequest+1))
		if err != nil {
			return nil, fmt.Errorf("read port request for node relay: %w", err)
		}
		if len(body) > relayClientMaxPortRequest {
			return nil, errRelayBodyLimit
		}
	}
	escapedPath := incoming.URL.EscapedPath()
	if escapedPath == "" {
		escapedPath = "/"
	}
	requestHeaders := incoming.Header.Clone()
	if requestHeaders.Get("User-Agent") == "" {
		requestHeaders.Set("User-Agent", "Brezel-Relay/1")
	}
	if requestHeaders.Get("Accept-Encoding") == "" {
		requestHeaders.Set("Accept-Encoding", "identity")
	}
	requestHeaders.Del("Content-Length")
	if len(body) > 0 {
		requestHeaders.Set("Content-Length", strconv.Itoa(len(body)))
	}
	protocolRequest := portProtocolRequest(incoming.Method, escapedPath, incoming.URL.RawQuery, requestHeaders, body)
	canonical, err := canonicalRelayJSON(protocolRequest)
	if err != nil {
		return nil, fmt.Errorf("encode node port descriptor: %w", err)
	}
	requestBound := int64(len(body))
	if requestBound == 0 {
		requestBound = 1
	}
	duration := relayOperationDuration(ctx, relayClientDefaultCommandTime)
	token, err := d.issue(binding, resolved, CapabilityProxyPort, canonical, CapabilityBounds{
		Port:              uint32(port),
		MaxDurationMillis: duration.Milliseconds(),
		MaxRequestBytes:   requestBound,
		MaxResponseBytes:  relayClientMaxPortResponse,
	})
	if err != nil {
		return nil, err
	}
	endpointURL := &url.URL{Path: fmt.Sprintf("/v1/ports/%d%s", port, incoming.URL.Path), RawQuery: incoming.URL.RawQuery}
	if incoming.URL.RawPath != "" {
		endpointURL.RawPath = fmt.Sprintf("/v1/ports/%d%s", port, incoming.URL.RawPath)
	}
	endpoint := endpointURL.String()
	httpRequest, err := d.request(ctx, incoming.Method, endpoint, bytes.NewReader(body), resolved.Route.RouteID, token)
	if err != nil {
		return nil, err
	}
	for name, values := range canonicalPortHeaders(requestHeaders) {
		for _, value := range values {
			httpRequest.Header.Add(name, value)
		}
	}
	response, err := d.client.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("call node port relay: %w", err)
	}
	response.Body = &relayBoundedReadCloser{source: response.Body, reader: relayBoundedReader{source: response.Body, remaining: relayClientMaxPortResponse}}
	return response, nil
}

func (d *RelayDataPlane) authorizedRoute(ctx context.Context, binding SandboxBinding) (relayRouteResponse, error) {
	if err := binding.validate(); err != nil {
		return relayRouteResponse{}, err
	}
	if d == nil || d.baseURL == nil || d.client == nil || d.signer == nil {
		return relayRouteResponse{}, backend.ErrCapabilityUnavailable
	}
	if !relayRouteIDPattern.MatchString(binding.backendID) || binding.nodeGeneration == 0 {
		return relayRouteResponse{}, ErrInvalidBinding
	}
	routeContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := d.request(routeContext, http.MethodGet, "/v1/routes/"+url.PathEscape(binding.backendID), nil, binding.backendID, "")
	if err != nil {
		return relayRouteResponse{}, err
	}
	response, err := d.client.Do(request)
	if err != nil {
		return relayRouteResponse{}, fmt.Errorf("resolve node route: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return relayRouteResponse{}, relayResponseError(response)
	}
	var resolved relayRouteResponse
	if err := decodeRelayJSON(response.Body, relayClientMaxRouteResponse, &resolved); err != nil {
		return relayRouteResponse{}, fmt.Errorf("decode node route: %w", err)
	}
	if err := validateIdentity("node id", resolved.NodeID); err != nil || resolved.NodeID != d.nodeID {
		return relayRouteResponse{}, ErrStaleBinding
	}
	if err := validateIdentity("boot epoch", resolved.BootEpoch); err != nil {
		return relayRouteResponse{}, ErrStaleBinding
	}
	if resolved.Route.RouteID != binding.backendID || resolved.Route.ProjectID != binding.projectID || resolved.Route.SandboxID != binding.sandboxID || resolved.Route.Generation != binding.nodeGeneration || resolved.Route.State != nodeledger.StateReady {
		return relayRouteResponse{}, ErrStaleBinding
	}
	return resolved, nil
}

func (d *RelayDataPlane) issue(binding SandboxBinding, resolved relayRouteResponse, operation CapabilityOperation, canonical []byte, bounds CapabilityBounds) (string, error) {
	digest, err := RequestDigest(operation, canonical)
	if err != nil {
		return "", err
	}
	ttl := MaxCapabilityTTL
	remaining := binding.expiresAt.Sub(binding.now())
	if remaining < ttl {
		ttl = remaining.Truncate(time.Second)
	}
	if ttl < time.Second {
		return "", ErrStaleBinding
	}
	token, _, err := d.signer.Issue(CapabilityIssue{
		Audience:      d.audience,
		NodeID:        d.nodeID,
		BootEpoch:     resolved.BootEpoch,
		RouteID:       resolved.Route.RouteID,
		Generation:    resolved.Route.Generation,
		ProjectID:     resolved.Route.ProjectID,
		SandboxID:     resolved.Route.SandboxID,
		Operation:     operation,
		RequestDigest: digest,
		Bounds:        bounds,
		TTL:           ttl,
	})
	if err != nil {
		return "", fmt.Errorf("issue node operation capability: %w", err)
	}
	return token, nil
}

func (d *RelayDataPlane) request(ctx context.Context, method, endpoint string, body io.Reader, routeID, token string) (*http.Request, error) {
	base := *d.baseURL
	separator := ""
	if !strings.HasPrefix(endpoint, "/") {
		separator = "/"
	}
	parsedEndpoint, err := url.Parse(base.String() + separator + endpoint)
	if err != nil || parsedEndpoint.Scheme != base.Scheme || parsedEndpoint.Host != base.Host {
		return nil, errors.New("invalid node relay endpoint")
	}
	request, err := http.NewRequestWithContext(ctx, method, parsedEndpoint.String(), body)
	if err != nil {
		return nil, err
	}
	request.Header.Set(RelayRouteHeader, routeID)
	if token != "" {
		request.Header.Set("Authorization", RelayCapabilityScheme+" "+token)
	}
	return request, nil
}

func relayOperationDuration(ctx context.Context, fallback time.Duration) time.Duration {
	duration := fallback
	if deadline, ok := ctx.Deadline(); ok {
		duration = time.Until(deadline)
	}
	if duration < time.Millisecond {
		return time.Millisecond
	}
	if duration > relayClientMaxOperationTime {
		return relayClientMaxOperationTime
	}
	return duration
}

func relayResponseError(response *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	message := strings.TrimSpace(string(data))
	if message == "" {
		message = response.Status
	}
	switch response.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", backend.ErrNotFound, message)
	case http.StatusNotImplemented:
		return fmt.Errorf("%w: %s", backend.ErrCapabilityUnavailable, message)
	case http.StatusConflict, http.StatusGone:
		return fmt.Errorf("%w: %s", ErrStaleBinding, message)
	default:
		return fmt.Errorf("node relay returned %s: %s", response.Status, message)
	}
}

type relayBoundedReader struct {
	source    io.Reader
	remaining int64
	exhausted bool
}

func (r *relayBoundedReader) Read(destination []byte) (int, error) {
	if r.exhausted {
		return 0, errRelayBodyLimit
	}
	if r.remaining == 0 {
		var probe [1]byte
		n, err := r.source.Read(probe[:])
		if n > 0 {
			r.exhausted = true
			return 0, errRelayBodyLimit
		}
		return 0, err
	}
	if int64(len(destination)) > r.remaining {
		destination = destination[:r.remaining]
	}
	n, err := r.source.Read(destination)
	r.remaining -= int64(n)
	return n, err
}

type relayBoundedReadCloser struct {
	source io.Closer
	reader relayBoundedReader
}

func (r *relayBoundedReadCloser) Read(destination []byte) (int, error) {
	return r.reader.Read(destination)
}
func (r *relayBoundedReadCloser) Close() error { return r.source.Close() }

func relayContentDigest(data []byte) string {
	descriptor := fileWriteDescriptor("content", data)
	return descriptor.SHA256
}

func validRelayContentDigest(encoded string, data []byte) bool {
	want, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(want) != 32 {
		return false
	}
	got, err := base64.RawURLEncoding.Strict().DecodeString(relayContentDigest(data))
	return err == nil && bytes.Equal(want, got)
}

var _ DataPlane = (*RelayDataPlane)(nil)
