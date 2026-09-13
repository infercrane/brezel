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
	"time"

	"github.com/infercrane/sandbox-runtime-lab/internal/backend"
	"github.com/infercrane/sandbox-runtime-lab/internal/domain"
)

type Client struct {
	baseURL    *url.URL
	apiKey     string
	httpClient *http.Client
}

// AuditedRevision is the upstream E2B Runtime revision whose public API contract
// this adapter was checked against. Qualification of a deployed environment is
// separate and must be established by conformance tests.
const AuditedRevision = "767ceb4b2ec0e598f512767c8da9e5e6618da368"

func New(baseURL, apiKey string, httpClient *http.Client) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("E2B API key is required")
	}
	if strings.ContainsAny(apiKey, "\r\n") {
		return nil, errors.New("E2B API key contains invalid characters")
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("E2B API URL must be an absolute http(s) URL")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if u.Scheme == "http" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1" {
		return nil, errors.New("E2B API URL must use HTTPS unless it is loopback")
	}
	clientCopy := *httpClient
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: u, apiKey: apiKey, httpClient: &clientCopy}, nil
}

func (c *Client) Name() string { return "e2b" }

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
	}
}

type sandboxResponse struct {
	SandboxID string `json:"sandboxID"`
	State     string `json:"state"`
}

func (c *Client) Create(ctx context.Context, in backend.CreateRequest) (backend.Sandbox, error) {
	timeout := in.Lifecycle.ExpiresAfterSeconds
	autoPause := in.Lifecycle.StandbyAfterSeconds > 0
	if autoPause {
		timeout = in.Lifecycle.StandbyAfterSeconds
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
		"network": map[string]any{
			"allowPublicTraffic": false,
			"allowOut":           in.Network.AllowOut,
			"denyOut":            in.Network.DenyOut,
		},
		"envVars": in.Environment,
	}
	var out sandboxResponse
	if err := c.do(ctx, http.MethodPost, "/sandboxes", body, &out, http.StatusCreated); err != nil {
		return backend.Sandbox{}, err
	}
	if out.SandboxID == "" {
		return backend.Sandbox{}, errors.New("E2B returned an empty sandbox id")
	}
	return backend.Sandbox{ID: out.SandboxID, State: domain.SandboxRunning}, nil
}

func (c *Client) Inspect(ctx context.Context, id string) (backend.Sandbox, error) {
	var out sandboxResponse
	if err := c.do(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id), nil, &out, http.StatusOK); err != nil {
		return backend.Sandbox{}, err
	}
	state := domain.SandboxUnknown
	switch out.State {
	case "running":
		state = domain.SandboxRunning
	case "paused":
		state = domain.SandboxStandby
	}
	return backend.Sandbox{ID: out.SandboxID, State: state}, nil
}

func (c *Client) Find(ctx context.Context, localID, projectID string) (backend.Sandbox, error) {
	u := c.baseURL.ResolveReference(&url.URL{Path: "/v2/sandboxes"})
	query := u.Query()
	query.Set("metadata", "runtime.sandbox_id="+localID+"&runtime.project_id="+projectID)
	u.RawQuery = query.Encode()
	var out []struct {
		SandboxID string            `json:"sandboxID"`
		State     string            `json:"state"`
		Metadata  map[string]string `json:"metadata"`
	}
	if err := c.doURL(ctx, http.MethodGet, u, nil, &out, http.StatusOK); err != nil {
		return backend.Sandbox{}, err
	}
	for _, candidate := range out {
		if candidate.Metadata["runtime.sandbox_id"] == localID && candidate.Metadata["runtime.project_id"] == projectID {
			state := domain.SandboxUnknown
			switch candidate.State {
			case "running":
				state = domain.SandboxRunning
			case "paused":
				state = domain.SandboxStandby
			}
			return backend.Sandbox{ID: candidate.SandboxID, State: state}, nil
		}
	}
	return backend.Sandbox{}, backend.ErrNotFound
}

func (c *Client) Pause(ctx context.Context, id string, kind domain.CheckpointKind) error {
	body := map[string]bool{"memory": kind == domain.CheckpointFullState}
	return c.do(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/pause", body, nil, http.StatusNoContent)
}

func (c *Client) Resume(ctx context.Context, id string, kind domain.CheckpointKind, ttlSeconds int64) (backend.Sandbox, error) {
	body := map[string]any{"memory": kind == domain.CheckpointFullState, "timeout": ttlSeconds}
	var out sandboxResponse
	if err := c.do(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/resume", body, &out, http.StatusCreated); err != nil {
		return backend.Sandbox{}, err
	}
	return backend.Sandbox{ID: out.SandboxID, State: domain.SandboxRunning}, nil
}

func (c *Client) Delete(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(id), nil, nil, http.StatusNoContent)
}

func (c *Client) Checkpoint(ctx context.Context, id string, kind domain.CheckpointKind, name string) (backend.Checkpoint, error) {
	if kind != domain.CheckpointFilesystem {
		return backend.Checkpoint{}, backend.ErrCapabilityUnavailable
	}
	var out struct {
		SnapshotID string `json:"snapshotID"`
	}
	if err := c.do(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/snapshots", map[string]string{"name": name}, &out, http.StatusCreated); err != nil {
		return backend.Checkpoint{}, err
	}
	if out.SnapshotID == "" {
		return backend.Checkpoint{}, errors.New("E2B returned an empty snapshot id")
	}
	return backend.Checkpoint{Ref: out.SnapshotID, Kind: kind}, nil
}

type remoteError struct {
	Code   int
	Status string
	Body   string
}

func (e *remoteError) Error() string {
	return fmt.Sprintf("E2B API returned %s: %s", e.Status, e.Body)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any, want int) error {
	u := c.baseURL.ResolveReference(&url.URL{Path: path})
	return c.doURL(ctx, method, u, body, out, want)
}

func (c *Client) doURL(ctx context.Context, method string, u *url.URL, body any, out any, want int) error {
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
	if resp.StatusCode != want {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<10))
		return &remoteError{Code: resp.StatusCode, Status: resp.Status, Body: strings.TrimSpace(string(data))}
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode E2B response: %w", err)
	}
	return nil
}
