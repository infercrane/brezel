package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/infercrane/sandbox-runtime-lab/internal/backend"
	"github.com/infercrane/sandbox-runtime-lab/internal/domain"
	"github.com/infercrane/sandbox-runtime-lab/internal/telemetry"
)

type Client struct {
	baseURL           *url.URL
	apiKey            string
	httpClient        *http.Client
	guestHTTPClient   *http.Client
	guestURLTemplate  string
	durableWorkspaces bool
	liveMu            sync.Mutex
	liveUntil         map[string]time.Time
	guestMu           sync.Mutex
	guestCredentials  map[string]guestCredential
	portMu            sync.Mutex
	portCredentials   map[string]portCredential
	observer          telemetry.Observer
}

type Option func(*Client) error

// WithGuestURLTemplate configures the guest-agent ingress used by self-hosted or test
// installations. {sandbox_id} and {port} are replaced per request. Remote
// plaintext endpoints are rejected.
func WithGuestURLTemplate(value string) Option {
	return func(c *Client) error {
		value = strings.TrimSpace(value)
		if value == "" {
			return errors.New("guest URL template cannot be empty")
		}
		candidate := strings.NewReplacer("{sandbox_id}", "sandbox-test", "{port}", "49983").Replace(value)
		u, err := url.Parse(candidate)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
			return errors.New("guest URL template must be an absolute http(s) origin without credentials, query, or fragment")
		}
		if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
			return errors.New("guest URL template must use HTTPS unless it is loopback")
		}
		c.guestURLTemplate = value
		return nil
	}
}

// WithDurableWorkspaces advertises the engine volume path only after the
// operator has configured durable storage for this exact engine deployment.
// It defaults to false so pointing the API at a stock engine cannot silently
// claim a capability that its deployment has disabled.
func WithDurableWorkspaces(enabled bool) Option {
	return func(c *Client) error {
		c.durableWorkspaces = enabled
		return nil
	}
}

// WithPhaseObserver records fixed, content-free adapter phase durations.
// The observer contract cannot carry sandbox, project, path, or request data.
func WithPhaseObserver(observer telemetry.Observer) Option {
	return func(c *Client) error {
		c.observer = observer
		return nil
	}
}

// AuditedRevision is the upstream runtime revision whose protocol contract this
// internal substrate adapter was checked against. Deployment qualification is
// separate and must be established by conformance tests.
const AuditedRevision = "767ceb4b2ec0e598f512767c8da9e5e6618da368"

func New(baseURL, apiKey string, httpClient *http.Client, options ...Option) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("microVM engine token is required")
	}
	if strings.ContainsAny(apiKey, "\r\n") {
		return nil, errors.New("microVM engine token contains invalid characters")
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("microVM engine URL must be an absolute http(s) URL")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return nil, errors.New("microVM engine URL must use HTTPS unless it is loopback")
	}
	clientCopy := *httpClient
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	guestCopy := clientCopy
	guestCopy.Timeout = 0
	client := &Client{
		baseURL: u, apiKey: apiKey, httpClient: &clientCopy, guestHTTPClient: &guestCopy,
		liveUntil: make(map[string]time.Time), guestCredentials: make(map[string]guestCredential),
		portCredentials: make(map[string]portCredential),
	}
	for _, option := range options {
		if err := option(client); err != nil {
			return nil, err
		}
	}
	return client, nil
}

func isLoopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func (c *Client) Name() string { return backend.DefaultName }

// Ready checks the pinned engine API through its authenticated health route.
// It deliberately does not create a sandbox or mutate engine state.
func (c *Client) Ready(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/health", nil, nil, http.StatusOK)
}

func (c *Client) Capabilities() backend.Capabilities {
	return backend.Capabilities{
		HostileCodeIsolation: true,
		DenyByDefaultEgress:  true,
		FilesystemStandby:    true,
		FullStateStandby:     true,
		FilesystemCheckpoint: true,
		// The independent snapshot route does not expose a full-memory
		// guarantee, so full-state checkpoints remain unavailable here.
		FullStateCheckpoint:      false,
		AutoResume:               true,
		EndpointCredentialBroker: false,
		CommandStreaming:         true,
		FileReadWrite:            true,
		AuthenticatedPorts:       true,
		DurableWorkspaces:        c.durableWorkspaces,
	}
}

type sandboxResponse struct {
	SandboxID          string  `json:"sandboxID"`
	State              string  `json:"state"`
	Domain             *string `json:"domain"`
	EnvdAccessToken    *string `json:"envdAccessToken"`
	TrafficAccessToken *string `json:"trafficAccessToken"`
}

func (c *Client) Create(ctx context.Context, in backend.CreateRequest) (backend.Sandbox, error) {
	timeout := in.Lifecycle.ExpiresAfterSeconds
	// Automatic standby is initiated by the product lifecycle service after its
	// durable activity deadline and grace period pass. Delegating a second timer
	// to the engine would allow it to pause before the product grace period or
	// race an admitted guest operation.
	autoPause := false
	network := map[string]any{
		"allowPublicTraffic": false,
	}
	// The pinned engine schema treats allowOut and denyOut as optional but
	// non-null. Go nil slices would otherwise become JSON null and make an
	// otherwise valid deny-by-default request fail schema validation.
	if len(in.Network.AllowOut) > 0 {
		network["allowOut"] = in.Network.AllowOut
	}
	if len(in.Network.DenyOut) > 0 {
		network["denyOut"] = in.Network.DenyOut
	}
	body := map[string]any{
		"templateID":            in.TemplateID,
		"timeout":               timeout,
		"autoPause":             autoPause,
		"autoPauseMemory":       in.Lifecycle.StandbyCheckpoint != domain.CheckpointFilesystem,
		"autoResume":            map[string]bool{"enabled": in.Lifecycle.AutoResume},
		"secure":                true,
		"allow_internet_access": in.Network.AllowInternet,
		"metadata": map[string]string{
			"runtime.sandbox_id": in.LocalSandboxID,
			"runtime.project_id": in.ProjectID,
		},
		"network": network,
		"envVars": in.Environment,
	}
	if len(in.WorkspaceMounts) > 0 {
		body["volumeMounts"] = in.WorkspaceMounts
	}
	var out sandboxResponse
	if err := c.do(ctx, http.MethodPost, "/sandboxes", body, &out, http.StatusCreated); err != nil {
		return backend.Sandbox{}, err
	}
	if out.SandboxID == "" {
		return backend.Sandbox{}, errors.New("microVM engine returned an empty sandbox id")
	}
	if err := requireSecureGuestAccess(out.EnvdAccessToken); err != nil {
		return backend.Sandbox{}, err
	}
	c.rememberLive(out.SandboxID)
	c.rememberGuestCredential(out)
	c.rememberPortCredential(out)
	return backend.Sandbox{ID: out.SandboxID, State: domain.SandboxRunning}, nil
}

func (c *Client) Inspect(ctx context.Context, id string) (backend.Sandbox, error) {
	var out sandboxResponse
	if err := c.do(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id), nil, &out, http.StatusOK); err != nil {
		return backend.Sandbox{}, err
	}
	return c.observedSandbox(ctx, id, out)
}

func (c *Client) Find(ctx context.Context, localID, projectID string) (backend.Sandbox, error) {
	u := c.baseURL.ResolveReference(&url.URL{Path: "/v2/sandboxes"})
	query := u.Query()
	query.Set("metadata", "runtime.sandbox_id="+localID+"&runtime.project_id="+projectID)
	u.RawQuery = query.Encode()
	var out []struct {
		SandboxID          string            `json:"sandboxID"`
		State              string            `json:"state"`
		Domain             *string           `json:"domain"`
		Metadata           map[string]string `json:"metadata"`
		EnvdAccessToken    *string           `json:"envdAccessToken"`
		TrafficAccessToken *string           `json:"trafficAccessToken"`
	}
	if err := c.doURL(ctx, http.MethodGet, u, nil, &out, http.StatusOK); err != nil {
		return backend.Sandbox{}, err
	}
	for _, candidate := range out {
		if candidate.Metadata["runtime.sandbox_id"] == localID && candidate.Metadata["runtime.project_id"] == projectID {
			return c.observedSandbox(ctx, candidate.SandboxID, sandboxResponse{
				SandboxID: candidate.SandboxID, State: candidate.State, Domain: candidate.Domain,
				EnvdAccessToken: candidate.EnvdAccessToken, TrafficAccessToken: candidate.TrafficAccessToken,
			})
		}
	}
	return backend.Sandbox{}, backend.ErrNotFound
}

// observedSandbox translates engine state only after checking the properties
// that make that state usable. The engine persists control metadata separately
// from a node's live microVM registry, so a host restart can leave a row marked
// running even though no guest exists. A non-mutating envd health probe prevents
// that stale row from becoming a product running state. Inspection deliberately
// does not use the engine's connect route because connect extends the sandbox
// keepalive and may resume a paused sandbox.
func (c *Client) observedSandbox(ctx context.Context, id string, out sandboxResponse) (backend.Sandbox, error) {
	if !safeSandboxID.MatchString(id) || out.SandboxID != id {
		return backend.Sandbox{}, errors.New("microVM engine returned an invalid or mismatched sandbox id")
	}
	if err := requireSecureGuestAccess(out.EnvdAccessToken); err != nil {
		return backend.Sandbox{}, err
	}
	state := domain.SandboxUnknown
	switch out.State {
	case "running":
		if !c.recentlyLive(out.SandboxID) {
			if err := c.probeGuestHealth(ctx, out); err != nil {
				c.forgetGuestState(out.SandboxID)
				return backend.Sandbox{}, err
			}
			c.rememberLive(out.SandboxID)
		}
		state = domain.SandboxRunning
	case "paused":
		c.forgetGuestState(out.SandboxID)
		state = domain.SandboxStandby
	default:
		c.forgetGuestState(out.SandboxID)
	}
	return backend.Sandbox{ID: out.SandboxID, State: state}, nil
}

const guestHealthTimeout = 2 * time.Second

// guestLiveTTL is a short liveness lease, not a durable state. It removes
// duplicate guest probes from bursts of control and guest operations. The map
// exists only in this adapter process, so controller or host restart always
// forces the first running-state observation to probe the guest again.
const guestLiveTTL = time.Second

func (c *Client) rememberLive(sandboxID string) {
	if !safeSandboxID.MatchString(sandboxID) {
		return
	}
	c.liveMu.Lock()
	c.liveUntil[sandboxID] = time.Now().Add(guestLiveTTL)
	c.liveMu.Unlock()
}

func (c *Client) recentlyLive(sandboxID string) bool {
	now := time.Now()
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	until, ok := c.liveUntil[sandboxID]
	if !ok || !now.Before(until) {
		delete(c.liveUntil, sandboxID)
		return false
	}
	return true
}

func (c *Client) forgetLive(sandboxID string) {
	c.liveMu.Lock()
	delete(c.liveUntil, sandboxID)
	c.liveMu.Unlock()
}

func (c *Client) probeGuestHealth(ctx context.Context, detail sandboxResponse) (resultErr error) {
	started := time.Now()
	defer func() {
		telemetry.Observe(c.observer, telemetry.OperationSandboxObserve, telemetry.PhaseGuestHealth, started, resultErr)
	}()
	baseURL, err := c.resolveGuestURL(detail.SandboxID, detail.Domain, envdPort)
	if err != nil {
		return err
	}
	probeURL := *baseURL
	probeURL.Path = "/health"
	probeURL.RawPath = ""
	probeURL.RawQuery = ""
	probeCtx, cancel := context.WithTimeout(ctx, guestHealthTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, probeURL.String(), nil)
	if err != nil {
		return err
	}
	connection := guestConnection{
		baseURL: baseURL, accessToken: strings.TrimSpace(*detail.EnvdAccessToken),
		sandboxID: detail.SandboxID, port: envdPort,
	}
	if detail.TrafficAccessToken != nil {
		connection.trafficToken = strings.TrimSpace(*detail.TrafficAccessToken)
	}
	connection.setHeaders(request.Header)
	response, err := c.guestHTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("microVM guest liveness probe: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 32<<10))
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("microVM guest liveness probe returned %s", response.Status)
	}
	return nil
}

func (c *Client) Pause(ctx context.Context, id string, kind domain.CheckpointKind) error {
	// Revoke every process-local proof and credential before a lifecycle
	// mutation. A timeout makes the remote state unknown, so retaining a cache
	// entry after the request starts could authorize a stale guest operation.
	c.forgetGuestState(id)
	defer c.forgetGuestState(id)
	body := map[string]bool{"memory": kind == domain.CheckpointFullState}
	if err := c.do(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/pause", body, nil, http.StatusNoContent); err != nil {
		return err
	}
	return nil
}

func (c *Client) Resume(ctx context.Context, id string, kind domain.CheckpointKind, ttlSeconds int64) (backend.Sandbox, error) {
	// A resume response establishes a new guest credential lease. Never carry
	// one across the standby boundary or an ambiguous resume attempt.
	c.forgetGuestState(id)
	body := map[string]any{"memory": kind == domain.CheckpointFullState, "timeout": ttlSeconds}
	var out sandboxResponse
	if err := c.do(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/resume", body, &out, http.StatusCreated); err != nil {
		return backend.Sandbox{}, err
	}
	if out.SandboxID == "" {
		return backend.Sandbox{}, errors.New("microVM engine returned an empty sandbox id after resume")
	}
	if out.SandboxID != id {
		c.forgetGuestState(out.SandboxID)
		return backend.Sandbox{}, errors.New("microVM engine returned a mismatched sandbox id after resume")
	}
	if err := requireSecureGuestAccess(out.EnvdAccessToken); err != nil {
		return backend.Sandbox{}, err
	}
	c.rememberLive(out.SandboxID)
	c.rememberGuestCredential(out)
	c.rememberPortCredential(out)
	return backend.Sandbox{ID: out.SandboxID, State: domain.SandboxRunning}, nil
}

func requireSecureGuestAccess(token *string) error {
	if token == nil || strings.TrimSpace(*token) == "" {
		return errors.New("microVM engine did not confirm secured guest-management access")
	}
	return nil
}

func (c *Client) Delete(ctx context.Context, id string) error {
	// Delete wins over guest traffic. Invalidate before contacting the engine so
	// a transport failure cannot leave an apparently usable local credential.
	c.forgetGuestState(id)
	defer c.forgetGuestState(id)
	if err := c.do(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(id), nil, nil, http.StatusNoContent); err != nil {
		return err
	}
	return nil
}

func (c *Client) Checkpoint(ctx context.Context, id string, kind domain.CheckpointKind, _ string) (backend.Checkpoint, error) {
	if kind != domain.CheckpointFilesystem {
		return backend.Checkpoint{}, backend.ErrCapabilityUnavailable
	}
	var out struct {
		SnapshotID string `json:"snapshotID"`
	}
	// A named upstream snapshot is an alias. Reusing that alias still creates a
	// new immutable build, but deletion can only address the latest alias target.
	// Keep the customer-facing name in product state and retain the engine's raw
	// immutable reference so every checkpoint can be physically reclaimed.
	if err := c.do(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/snapshots", map[string]string{}, &out, http.StatusCreated); err != nil {
		return backend.Checkpoint{}, err
	}
	if !validCheckpointRef(out.SnapshotID) {
		return backend.Checkpoint{}, errors.New("microVM engine returned an invalid snapshot id")
	}
	return backend.Checkpoint{Ref: out.SnapshotID, Kind: kind}, nil
}

func (c *Client) DeleteCheckpoint(ctx context.Context, ref string) error {
	if !validCheckpointRef(ref) {
		return errors.New("invalid checkpoint backend reference")
	}
	return c.do(ctx, http.MethodDelete, "/templates/"+url.PathEscape(ref), nil, nil, http.StatusNoContent)
}

func validCheckpointRef(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 200 || strings.ContainsAny(value, "/\\\r\n\t") {
		return false
	}
	for _, part := range strings.Split(value, ":") {
		if part == "" {
			return false
		}
		for _, r := range part {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.') {
				return false
			}
		}
	}
	return true
}

type remoteError struct {
	Code   int
	Status string
	Body   string
}

func (e *remoteError) Error() string {
	return fmt.Sprintf("microVM engine returned %s: %s", e.Status, e.Body)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any, want ...int) error {
	u := c.baseURL.ResolveReference(&url.URL{Path: path})
	return c.doURL(ctx, method, u, body, out, want...)
}

func (c *Client) doURL(ctx context.Context, method string, u *url.URL, body any, out any, want ...int) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return backend.ErrNotFound
	}
	accepted := false
	for _, status := range want {
		if resp.StatusCode == status {
			accepted = true
			break
		}
	}
	if !accepted {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
		return &remoteError{Code: resp.StatusCode, Status: resp.Status, Body: strings.TrimSpace(string(data))}
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode microVM engine response: %w", err)
	}
	return nil
}
