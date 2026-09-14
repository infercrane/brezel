package perfbench

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxResponseBytes = 2 << 20
	maxFileBytes     = 64 << 20
)

type benchmarkError struct {
	category string
	detail   string
	status   int
}

func (e *benchmarkError) Error() string { return e.detail }

func failure(category, format string, args ...any) error {
	return &benchmarkError{category: category, detail: fmt.Sprintf(format, args...)}
}

func errorFields(err error) (string, string) {
	if err == nil {
		return "", ""
	}
	var target *benchmarkError
	if errors.As(err, &target) {
		return target.category, target.detail
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", "operation exceeded its configured timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled", "operation was canceled"
	}
	return "internal", "benchmark client failed without a classified result"
}

type apiClient struct {
	base    *url.URL
	token   string
	project string
	http    *http.Client
}

type mutationEnvelope struct {
	Resource struct {
		ID                 string `json:"id"`
		RevisionID         string `json:"revision_id"`
		State              string `json:"state"`
		Kind               string `json:"kind"`
		SourceSandboxID    string `json:"source_sandbox_id"`
		SourceCheckpointID string `json:"source_checkpoint_id"`
	} `json:"resource"`
	Operation struct {
		State string `json:"state"`
	} `json:"operation"`
}

func newAPIClient(config Config, base *url.URL) *apiClient {
	client := config.Client
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = max(100, config.Runs*2)
		transport.MaxIdleConnsPerHost = max(100, config.Runs*2)
		transport.MaxConnsPerHost = max(100, config.Runs*2)
		client = &http.Client{Transport: transport}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &apiClient{base: base, token: config.Token, project: config.ProjectID, http: &copyClient}
}

func (c *apiClient) createEnvironment(ctx context.Context, template, runtimeRevision string) (string, string, error) {
	digest := sha256.Sum256([]byte(c.project + "\x00" + template + "\x00" + runtimeRevision))
	idempotency := "perfbench-environment-" + hex.EncodeToString(digest[:12])
	body := map[string]any{
		"name":            "sandbox-benchmark",
		"template":        template,
		"policy_revision": "perfbench-v1",
	}
	var out mutationEnvelope
	requestID, err := c.json(ctx, http.MethodPost, "/v1/environments", idempotency, body, http.StatusCreated, &out)
	if err != nil {
		return "", requestID, err
	}
	if out.Resource.RevisionID == "" {
		return "", requestID, failure("protocol_state", "environment response omitted revision_id")
	}
	return out.Resource.RevisionID, requestID, nil
}

func (c *apiClient) createSandbox(ctx context.Context, environmentRevision string) (string, string, error) {
	body := map[string]any{
		"environment_revision": environmentRevision,
		"lifecycle":            benchmarkLifecycle(),
		"network":              map[string]any{"allow_internet": false},
	}
	return c.createSandboxRequest(ctx, body, "")
}

func (c *apiClient) createSandboxFromCheckpoint(ctx context.Context, checkpointID string) (string, string, error) {
	body := map[string]any{
		"checkpoint_id": checkpointID,
		"lifecycle":     benchmarkLifecycle(),
		"network":       map[string]any{"allow_internet": false},
	}
	return c.createSandboxRequest(ctx, body, checkpointID)
}

func (c *apiClient) createSandboxWithWorkspace(ctx context.Context, environmentRevision, workspaceID string) (string, string, error) {
	body := map[string]any{
		"environment_revision": environmentRevision,
		"lifecycle":            benchmarkLifecycle(),
		"network":              map[string]any{"allow_internet": false},
		"workspace_mounts": []map[string]string{{
			"workspace_id": workspaceID,
			"path":         "/workspace",
		}},
	}
	return c.createSandboxRequest(ctx, body, "")
}

func benchmarkLifecycle() map[string]any {
	return map[string]any{
		"standby_after_seconds":   0,
		"expires_after_seconds":   3600,
		"standby_checkpoint_kind": "full_state",
		"auto_resume":             false,
	}
}

func (c *apiClient) createSandboxRequest(ctx context.Context, body map[string]any, checkpointID string) (string, string, error) {
	var out mutationEnvelope
	requestID, err := c.json(ctx, http.MethodPost, "/v1/sandboxes", randomID("perfbench-create"), body, http.StatusAccepted, &out)
	if err != nil {
		return "", requestID, err
	}
	if out.Resource.ID == "" || out.Resource.State != "running" {
		return out.Resource.ID, requestID, failure("protocol_state", "create did not confirm a running sandbox")
	}
	if checkpointID != "" && out.Resource.SourceCheckpointID != checkpointID {
		return out.Resource.ID, requestID, failure("protocol_state", "restore did not bind the requested checkpoint")
	}
	return out.Resource.ID, requestID, nil
}

func (c *apiClient) createWorkspace(ctx context.Context) (string, string, error) {
	body := map[string]any{"name": randomID("perfbench-workspace")}
	var out mutationEnvelope
	requestID, err := c.json(ctx, http.MethodPost, "/v1/workspaces", randomID("perfbench-workspace-create"), body, http.StatusAccepted, &out)
	if err != nil {
		return out.Resource.ID, requestID, err
	}
	if out.Resource.ID == "" || out.Resource.State != "ready" {
		return out.Resource.ID, requestID, failure("protocol_state", "workspace create did not confirm ready state")
	}
	return out.Resource.ID, requestID, nil
}

func (c *apiClient) createFilesystemCheckpoint(ctx context.Context, sandboxID string) (string, string, error) {
	body := map[string]any{"name": randomID("perfbench-checkpoint"), "kind": "filesystem"}
	path := "/v1/sandboxes/" + url.PathEscape(sandboxID) + "/checkpoints"
	var out mutationEnvelope
	requestID, err := c.json(ctx, http.MethodPost, path, randomID("perfbench-checkpoint-create"), body, http.StatusAccepted, &out)
	if err != nil {
		return out.Resource.ID, requestID, err
	}
	if out.Resource.ID == "" || out.Resource.Kind != "filesystem" || out.Resource.SourceSandboxID != sandboxID {
		return out.Resource.ID, requestID, failure("protocol_state", "checkpoint response did not confirm filesystem kind and source sandbox")
	}
	return out.Resource.ID, requestID, nil
}

func (c *apiClient) action(ctx context.Context, sandboxID, action, wantState string) (string, error) {
	var out mutationEnvelope
	path := "/v1/sandboxes/" + url.PathEscape(sandboxID) + ":" + action
	requestID, err := c.json(ctx, http.MethodPost, path, randomID("perfbench-"+action), nil, http.StatusAccepted, &out)
	if err != nil {
		return requestID, err
	}
	if out.Resource.ID == "" || out.Resource.State != wantState {
		return requestID, failure("protocol_state", "%s did not confirm sandbox state %s", action, wantState)
	}
	return requestID, nil
}

func (c *apiClient) deleteSandbox(ctx context.Context, sandboxID, idempotency string) (string, bool, error) {
	var out mutationEnvelope
	path := "/v1/sandboxes/" + url.PathEscape(sandboxID)
	requestID, err := c.json(ctx, http.MethodDelete, path, idempotency, nil, http.StatusAccepted, &out)
	if err != nil {
		var target *benchmarkError
		if errors.As(err, &target) && target.status == http.StatusNotFound {
			return requestID, true, nil
		}
		return requestID, false, err
	}
	if out.Resource.ID == "" || out.Resource.State != "deleted" {
		return requestID, false, failure("cleanup_state", "delete did not confirm a deleted sandbox")
	}
	return requestID, false, nil
}

func (c *apiClient) deleteCheckpoint(ctx context.Context, checkpointID, idempotency string) (string, bool, error) {
	var out mutationEnvelope
	path := "/v1/checkpoints/" + url.PathEscape(checkpointID)
	requestID, err := c.json(ctx, http.MethodDelete, path, idempotency, nil, http.StatusAccepted, &out)
	if err != nil {
		var target *benchmarkError
		if errors.As(err, &target) && target.status == http.StatusNotFound {
			return requestID, true, nil
		}
		return requestID, false, err
	}
	if out.Operation.State != "succeeded" {
		return requestID, false, failure("cleanup_state", "checkpoint delete did not confirm a succeeded operation")
	}
	return requestID, false, nil
}

func (c *apiClient) deleteWorkspace(ctx context.Context, workspaceID, idempotency string) (string, bool, error) {
	var out mutationEnvelope
	path := "/v1/workspaces/" + url.PathEscape(workspaceID)
	requestID, err := c.json(ctx, http.MethodDelete, path, idempotency, nil, http.StatusAccepted, &out)
	if err != nil {
		var target *benchmarkError
		if errors.As(err, &target) && target.status == http.StatusNotFound {
			return requestID, true, nil
		}
		return requestID, false, err
	}
	if out.Resource.ID == "" || out.Resource.State != "deleted" {
		return requestID, false, failure("cleanup_state", "workspace delete did not confirm deleted state")
	}
	return requestID, false, nil
}

func (c *apiClient) runNonce(ctx context.Context, sandboxID, nonce string) (string, error) {
	return c.runExpected(ctx, sandboxID, []string{"/bin/sh", "-lc", `printf '%s' "$1"`, "perfbench", nonce}, nonce)
}

func (c *apiClient) runExpected(ctx context.Context, sandboxID string, argv []string, expected string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"argv":            argv,
		"timeout_seconds": 30,
	})
	if err != nil {
		return "", failure("encode", "encode command request")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base.String()+"/v1/sandboxes/"+url.PathEscape(sandboxID)+"/commands", bytes.NewReader(body))
	if err != nil {
		return "", failure("encode", "build command request")
	}
	c.authorize(request, "")
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return "", transportError(ctx, err)
	}
	defer response.Body.Close()
	requestID := response.Header.Get("X-Request-ID")
	if response.StatusCode != http.StatusOK {
		drainBounded(response.Body)
		return requestID, statusError(response.StatusCode, "command")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return requestID, transportError(ctx, err)
	}
	if len(data) > maxResponseBytes {
		return requestID, failure("response_too_large", "command response exceeded %d bytes", maxResponseBytes)
	}
	var started, exited bool
	var stdout bytes.Buffer
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			Data     string `json:"data"`
			ExitCode int32  `json:"exit_code"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return requestID, failure("decode", "decode command stream event")
		}
		switch event.Type {
		case "started":
			if started || exited || stdout.Len() != 0 {
				return requestID, failure("command_stream", "command stream reported started out of order")
			}
			started = true
		case "stdout":
			if !started || exited {
				return requestID, failure("command_stream", "command stdout arrived outside the running interval")
			}
			decoded, err := base64.StdEncoding.DecodeString(event.Data)
			if err != nil {
				return requestID, failure("decode", "command stdout was not valid base64")
			}
			stdout.Write(decoded)
		case "stderr":
			if !started || exited {
				return requestID, failure("command_stream", "command stderr arrived outside the running interval")
			}
			if _, err := base64.StdEncoding.DecodeString(event.Data); err != nil {
				return requestID, failure("decode", "command stderr was not valid base64")
			}
		case "exited":
			if !started || exited {
				return requestID, failure("command_stream", "command exit arrived out of order")
			}
			if event.ExitCode != 0 {
				return requestID, failure("command_exit", "command exited with code %d", event.ExitCode)
			}
			exited = true
		case "error":
			return requestID, failure("command_stream", "command stream reported an error")
		default:
			return requestID, failure("command_stream", "command stream contained an unknown event type")
		}
	}
	if !started || !exited {
		return requestID, failure("command_stream", "command stream did not confirm both start and exit")
	}
	if stdout.String() != expected {
		return requestID, failure("output_mismatch", "command output did not match the generated nonce")
	}
	return requestID, nil
}

func (c *apiClient) writeFile(ctx context.Context, sandboxID, path string, data []byte) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, c.fileURL(sandboxID, path), bytes.NewReader(data))
	if err != nil {
		return "", failure("encode", "build file write request")
	}
	c.authorize(request, "")
	request.Header.Set("Content-Type", "application/octet-stream")
	request.ContentLength = int64(len(data))
	response, err := c.http.Do(request)
	if err != nil {
		return "", transportError(ctx, err)
	}
	defer response.Body.Close()
	requestID := response.Header.Get("X-Request-ID")
	if response.StatusCode != http.StatusOK {
		drainBounded(response.Body)
		return requestID, statusError(response.StatusCode, "file write")
	}
	drainBounded(response.Body)
	return requestID, nil
}

func (c *apiClient) readExactFile(ctx context.Context, sandboxID, path string, expected []byte) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.fileURL(sandboxID, path), nil)
	if err != nil {
		return "", failure("encode", "build file read request")
	}
	c.authorize(request, "")
	response, err := c.http.Do(request)
	if err != nil {
		return "", transportError(ctx, err)
	}
	defer response.Body.Close()
	requestID := response.Header.Get("X-Request-ID")
	if response.StatusCode != http.StatusOK {
		drainBounded(response.Body)
		return requestID, statusError(response.StatusCode, "file read")
	}
	if len(expected) > maxFileBytes {
		return requestID, failure("response_too_large", "expected file exceeds %d bytes", maxFileBytes)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(len(expected))+1))
	if err != nil {
		return requestID, transportError(ctx, err)
	}
	if !bytes.Equal(data, expected) {
		return requestID, failure("output_mismatch", "file content did not match generated benchmark bytes")
	}
	return requestID, nil
}

func (c *apiClient) fileURL(sandboxID, path string) string {
	query := url.Values{"path": []string{path}}
	return c.base.String() + "/v1/sandboxes/" + url.PathEscape(sandboxID) + "/files?" + query.Encode()
}

func (c *apiClient) createPortLease(ctx context.Context, sandboxID string, port uint16) (string, string, error) {
	path := fmt.Sprintf("/v1/sandboxes/%s/ports/%d/leases", url.PathEscape(sandboxID), port)
	var out struct {
		Path string `json:"path"`
	}
	requestID, err := c.json(ctx, http.MethodPost, path, "", map[string]any{"ttl_seconds": 900}, http.StatusCreated, &out)
	if err != nil {
		return "", requestID, err
	}
	reference, err := url.Parse(out.Path)
	if err != nil || reference.IsAbs() || !strings.HasPrefix(reference.Path, "/p/") || reference.RawQuery != "" || reference.Fragment != "" {
		return "", requestID, failure("protocol_state", "port lease response omitted a valid opaque relative preview path")
	}
	return c.base.ResolveReference(reference).String(), requestID, nil
}

func (c *apiClient) previewExact(ctx context.Context, previewURL string, expected []byte) (string, time.Duration, time.Duration, error) {
	started := time.Now()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, previewURL, nil)
	if err != nil {
		return "", 0, 0, failure("encode", "build preview request")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return "", 0, time.Since(started), transportError(ctx, err)
	}
	defer response.Body.Close()
	requestID := response.Header.Get("X-Request-ID")
	if response.StatusCode != http.StatusOK {
		drainBounded(response.Body)
		return requestID, 0, time.Since(started), statusError(response.StatusCode, "preview")
	}
	first := make([]byte, 1)
	count, err := io.ReadFull(response.Body, first)
	firstByte := time.Since(started)
	if err != nil || count != 1 {
		return requestID, 0, time.Since(started), failure("preview_body", "preview response did not contain a first byte")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	complete := time.Since(started)
	if err != nil {
		return requestID, firstByte, complete, transportError(ctx, err)
	}
	data = append(first, data...)
	if len(data) > maxResponseBytes {
		return requestID, firstByte, complete, failure("response_too_large", "preview response exceeded %d bytes", maxResponseBytes)
	}
	if !bytes.Equal(data, expected) {
		return requestID, firstByte, complete, failure("output_mismatch", "preview body did not match generated benchmark bytes")
	}
	return requestID, firstByte, complete, nil
}

func (c *apiClient) json(ctx context.Context, method, path, idempotency string, body any, wantStatus int, out any) (string, error) {
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return "", failure("encode", "encode %s request", method)
		}
		input = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, input)
	if err != nil {
		return "", failure("encode", "build %s request", method)
	}
	c.authorize(request, idempotency)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return "", transportError(ctx, err)
	}
	defer response.Body.Close()
	requestID := response.Header.Get("X-Request-ID")
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return requestID, transportError(ctx, err)
	}
	if len(data) > maxResponseBytes {
		return requestID, failure("response_too_large", "%s response exceeded %d bytes", method, maxResponseBytes)
	}
	if response.StatusCode != wantStatus {
		return requestID, statusError(response.StatusCode, method)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return requestID, failure("decode", "decode %s response", method)
		}
	}
	return requestID, nil
}

func (c *apiClient) authorize(request *http.Request, idempotency string) {
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("X-Project-ID", c.project)
	if idempotency != "" {
		request.Header.Set("Idempotency-Key", idempotency)
	}
}

func transportError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return failure("timeout", "request exceeded its configured timeout")
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return failure("canceled", "request was canceled")
	}
	return failure("transport", "control API request failed: %T", err)
}

func statusError(status int, operation string) error {
	category := "http_4xx"
	switch {
	case status == http.StatusTooManyRequests:
		category = "http_429"
	case status >= 500:
		category = "http_5xx"
	case status >= 300 && status < 400:
		category = "redirect_rejected"
	}
	return &benchmarkError{category: category, detail: fmt.Sprintf("%s request returned HTTP %d", operation, status), status: status}
}

func drainBounded(reader io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(reader, maxResponseBytes))
}

func randomID(prefix string) string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err == nil {
		return prefix + "-" + hex.EncodeToString(value)
	}
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}
