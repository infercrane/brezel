package conformance

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/infercrane/brezel/internal/receipt"
)

const maxResponseBytes = 2 << 20

type Config struct {
	BaseURL         string
	Token           string
	ProjectID       string
	OtherProjectID  string
	BackendTemplate string
	Target          string
	Execute         bool
	Client          *http.Client
}

type Step struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Detail     string `json:"detail,omitempty"`
}

type Report struct {
	Target        string    `json:"target"`
	Scope         string    `json:"scope"`
	Qualification string    `json:"qualification"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	Steps         []Step    `json:"steps"`
}

type Runner struct {
	base              *url.URL
	token             string
	project           string
	otherProject      string
	template          string
	target            string
	client            *http.Client
	report            Report
	sandboxID         string
	deleted           bool
	checkpointID      string
	checkpointDeleted bool
	workspaceID       string
	workspaceDeleted  bool
}

func New(config Config) (*Runner, error) {
	if !config.Execute {
		return nil, errors.New("conformance execution requires explicit Execute=true because it creates backend resources")
	}
	if len(config.Token) < 32 {
		return nil, errors.New("service token must be at least 32 characters")
	}
	if config.ProjectID == "" || config.BackendTemplate == "" || config.Target == "" {
		return nil, errors.New("project, backend template, and target are required")
	}
	if config.OtherProjectID == "" {
		config.OtherProjectID = config.ProjectID + "-isolation-check"
	}
	if config.OtherProjectID == config.ProjectID {
		return nil, errors.New("other project must differ from the primary project")
	}
	base, err := url.Parse(strings.TrimRight(config.BaseURL, "/"))
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return nil, errors.New("base URL must be an absolute HTTP(S) URL")
	}
	if base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("base URL must not contain credentials, query, or fragment")
	}
	if base.Scheme == "http" && !loopbackHost(base.Hostname()) {
		return nil, errors.New("plaintext conformance is allowed only for a loopback control API")
	}
	client := config.Client
	if client == nil {
		client = &http.Client{Timeout: 45 * time.Second}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Runner{
		base: base, token: config.Token, project: config.ProjectID,
		otherProject: config.OtherProjectID, template: config.BackendTemplate,
		target: config.Target, client: &copyClient,
	}, nil
}

func (r *Runner) Run(ctx context.Context) (report Report, runErr error) {
	r.report = Report{
		Target:    r.target,
		Scope:     "real guest command, file, authenticated HTTP port, and durable workspace paths; pause/resume persistence; tenant isolation; idempotency input binding; checkpoint; cleanup; and receipt signature",
		StartedAt: time.Now().UTC(), Qualification: "failed",
	}
	defer func() {
		if r.sandboxID != "" && !r.deleted {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			started := time.Now()
			cleanupErr := r.delete(cleanupCtx, "cleanup_after_failure")
			cleanupStep := Step{Name: "cleanup_after_failure", Status: "passed", DurationMS: time.Since(started).Milliseconds()}
			if cleanupErr != nil {
				cleanupStep.Status = "failed"
				cleanupStep.Detail = cleanupErr.Error()
			}
			r.report.Steps = append(r.report.Steps, cleanupStep)
		}
		if r.checkpointID != "" && !r.checkpointDeleted {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			started := time.Now()
			cleanupErr := r.deleteCheckpoint(cleanupCtx, "cleanup_checkpoint_after_failure")
			cleanupStep := Step{Name: "cleanup_checkpoint_after_failure", Status: "passed", DurationMS: time.Since(started).Milliseconds()}
			if cleanupErr != nil {
				cleanupStep.Status = "failed"
				cleanupStep.Detail = cleanupErr.Error()
			}
			r.report.Steps = append(r.report.Steps, cleanupStep)
		}
		if r.workspaceID != "" && !r.workspaceDeleted {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			started := time.Now()
			cleanupErr := r.deleteWorkspace(cleanupCtx, "cleanup_workspace_after_failure")
			cleanupStep := Step{Name: "cleanup_workspace_after_failure", Status: "passed", DurationMS: time.Since(started).Milliseconds()}
			if cleanupErr != nil {
				cleanupStep.Status = "failed"
				cleanupStep.Detail = cleanupErr.Error()
			}
			r.report.Steps = append(r.report.Steps, cleanupStep)
		}
		r.report.FinishedAt = time.Now().UTC()
		report = r.report
	}()

	if err := r.step(ctx, "readiness", func() error {
		return r.expect(ctx, http.MethodGet, "/readyz", r.project, "", nil, http.StatusOK, nil)
	}); err != nil {
		return r.report, err
	}
	if err := r.step(ctx, "capability_disclosure", func() error {
		var out struct {
			Qualification string `json:"qualification"`
			Implemented   struct {
				DurableWorkspaces bool `json:"durable_workspaces"`
			} `json:"implemented"`
		}
		if err := r.expect(ctx, http.MethodGet, "/v1/capabilities", r.project, "", nil, http.StatusOK, &out); err != nil {
			return err
		}
		if out.Qualification == "" {
			return errors.New("capability response omitted qualification state")
		}
		if !out.Implemented.DurableWorkspaces {
			return errors.New("runtime did not disclose durable workspace support")
		}
		return nil
	}); err != nil {
		return r.report, err
	}

	if err := r.step(ctx, "create_workspace", func() error {
		var out mutationEnvelope
		if err := r.expect(ctx, http.MethodPost, "/v1/workspaces", r.project, randomKey("conformance-workspace"), map[string]any{"name": "conformance-state"}, http.StatusAccepted, &out); err != nil {
			return err
		}
		r.workspaceID = out.Resource.ID
		if r.workspaceID == "" || out.Resource.State != "ready" {
			return fmt.Errorf("workspace was not confirmed ready; state=%q", out.Resource.State)
		}
		return nil
	}); err != nil {
		return r.report, err
	}
	if err := r.step(ctx, "cross_project_workspace_denial", func() error {
		return r.expect(ctx, http.MethodGet, "/v1/workspaces/"+url.PathEscape(r.workspaceID), r.otherProject, "", nil, http.StatusNotFound, nil)
	}); err != nil {
		return r.report, err
	}

	var publicKey ed25519.PublicKey
	if err := r.step(ctx, "receipt_key", func() error {
		var out struct {
			Algorithm string `json:"algorithm"`
			PublicKey string `json:"public_key"`
		}
		if err := r.expect(ctx, http.MethodGet, "/v1/receipt-public-key", r.project, "", nil, http.StatusOK, &out); err != nil {
			return err
		}
		decoded, err := base64.StdEncoding.DecodeString(out.PublicKey)
		if err != nil || out.Algorithm != "Ed25519" || len(decoded) != ed25519.PublicKeySize {
			return errors.New("receipt endpoint did not return a valid Ed25519 public key")
		}
		publicKey = decoded
		return nil
	}); err != nil {
		return r.report, err
	}

	var environmentRevision string
	createEnvironmentKey := randomKey("conformance-environment")
	if err := r.step(ctx, "create_environment", func() error {
		var out mutationEnvelope
		body := map[string]any{
			"name": "conformance", "template": r.template,
			"policy_revision": "conformance-v1",
		}
		if err := r.expect(ctx, http.MethodPost, "/v1/environments", r.project, createEnvironmentKey, body, http.StatusCreated, &out); err != nil {
			return err
		}
		environmentRevision = out.Resource.RevisionID
		if environmentRevision == "" {
			return errors.New("environment response omitted revision_id")
		}
		return nil
	}); err != nil {
		return r.report, err
	}

	createSandboxKey := randomKey("conformance-sandbox")
	createBody := map[string]any{
		"environment_revision": environmentRevision,
		"lifecycle": map[string]any{
			"standby_after_seconds": 0, "expires_after_seconds": 600,
			"standby_checkpoint_kind": "full_state", "auto_resume": false,
		},
		"network":          map[string]any{"allow_internet": false},
		"workspace_mounts": []map[string]string{{"workspace_id": r.workspaceID, "path": "/workspace"}},
	}
	if err := r.step(ctx, "create_sandbox", func() error {
		var out mutationEnvelope
		if err := r.expect(ctx, http.MethodPost, "/v1/sandboxes", r.project, createSandboxKey, createBody, http.StatusAccepted, &out); err != nil {
			return err
		}
		r.sandboxID = out.Resource.ID
		if r.sandboxID == "" || out.Resource.State != "running" {
			return fmt.Errorf("sandbox was not confirmed running; state=%q", out.Resource.State)
		}
		return nil
	}); err != nil {
		return r.report, err
	}

	if err := r.step(ctx, "idempotent_create", func() error {
		var out mutationEnvelope
		if err := r.expect(ctx, http.MethodPost, "/v1/sandboxes", r.project, createSandboxKey, createBody, http.StatusAccepted, &out); err != nil {
			return err
		}
		if out.Resource.ID != r.sandboxID {
			return errors.New("replayed create returned a different sandbox")
		}
		return nil
	}); err != nil {
		return r.report, err
	}
	if err := r.step(ctx, "idempotency_payload_binding", func() error {
		changed := map[string]any{
			"environment_revision": environmentRevision,
			"lifecycle": map[string]any{
				"standby_after_seconds": 0, "expires_after_seconds": 601,
				"standby_checkpoint_kind": "full_state", "auto_resume": false,
			},
			"network":          map[string]any{"allow_internet": false},
			"workspace_mounts": []map[string]string{{"workspace_id": r.workspaceID, "path": "/workspace"}},
		}
		return r.expect(ctx, http.MethodPost, "/v1/sandboxes", r.project, createSandboxKey, changed, http.StatusConflict, nil)
	}); err != nil {
		return r.report, err
	}

	if err := r.step(ctx, "cross_project_denial", func() error {
		return r.expect(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(r.sandboxID), r.otherProject, "", nil, http.StatusNotFound, nil)
	}); err != nil {
		return r.report, err
	}

	marker := randomKey("guest-state")
	guestPath := "/workspace/brezel-conformance.txt"
	if err := r.step(ctx, "guest_file_write_read", func() error {
		if err := r.writeFile(ctx, r.project, guestPath, []byte(marker), http.StatusOK); err != nil {
			return err
		}
		data, err := r.readFile(ctx, r.project, guestPath, http.StatusOK)
		if err != nil {
			return err
		}
		if string(data) != marker {
			return errors.New("guest file read did not match the uploaded bytes")
		}
		return nil
	}); err != nil {
		return r.report, err
	}
	if err := r.step(ctx, "guest_command_stream", func() error {
		return r.runCommand(ctx, r.project, []string{"/bin/sh", "-lc", "cat /workspace/brezel-conformance.txt"}, marker, http.StatusOK)
	}); err != nil {
		return r.report, err
	}
	if err := r.step(ctx, "cross_project_guest_denial", func() error {
		if _, err := r.readFile(ctx, r.otherProject, guestPath, http.StatusNotFound); err != nil {
			return err
		}
		return r.runCommand(ctx, r.otherProject, []string{"/bin/true"}, "", http.StatusNotFound)
	}); err != nil {
		return r.report, err
	}
	if err := r.step(ctx, "start_preview_server", func() error {
		serve := []string{"/bin/sh", "-lc", "mkdir -p /tmp/brezel-conformance-web && printf '%s' '" + marker + "' > /tmp/brezel-conformance-web/index.html && busybox httpd -p 8080 -h /tmp/brezel-conformance-web"}
		return r.runCommand(ctx, r.project, serve, "", http.StatusOK)
	}); err != nil {
		return r.report, err
	}
	var leasePath string
	if err := r.step(ctx, "create_port_lease", func() error {
		var lease struct {
			Path string `json:"path"`
		}
		if err := r.expect(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(r.sandboxID)+"/ports/8080/leases", r.project, "", map[string]any{"ttl_seconds": 60}, http.StatusCreated, &lease); err != nil {
			return err
		}
		if !strings.HasPrefix(lease.Path, "/p/") {
			return errors.New("port lease omitted an opaque preview path")
		}
		leasePath = lease.Path
		return nil
	}); err != nil {
		return r.report, err
	}
	requestPreview := func() error {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base.String()+leasePath, nil)
		if err != nil {
			return err
		}
		response, err := r.client.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
		if err != nil {
			return err
		}
		if response.StatusCode != http.StatusOK || string(data) != marker {
			return fmt.Errorf("authenticated port response=%d body=%q", response.StatusCode, compact(data))
		}
		return nil
	}
	if err := r.step(ctx, "authenticated_port_first_request", requestPreview); err != nil {
		return r.report, err
	}
	if err := r.step(ctx, "authenticated_port_warm_request", requestPreview); err != nil {
		return r.report, err
	}
	if err := r.step(ctx, "pause", func() error {
		var out mutationEnvelope
		if err := r.expect(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(r.sandboxID)+":pause", r.project, randomKey("pause"), nil, http.StatusAccepted, &out); err != nil {
			return err
		}
		if out.Resource.State != "standby" {
			return fmt.Errorf("pause state=%q, want standby", out.Resource.State)
		}
		return nil
	}); err != nil {
		return r.report, err
	}
	if err := r.step(ctx, "resume", func() error {
		var out mutationEnvelope
		if err := r.expect(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(r.sandboxID)+":resume", r.project, randomKey("resume"), nil, http.StatusAccepted, &out); err != nil {
			return err
		}
		if out.Resource.State != "running" {
			return fmt.Errorf("resume state=%q, want running", out.Resource.State)
		}
		return nil
	}); err != nil {
		return r.report, err
	}
	if err := r.step(ctx, "workspace_survives_resume", func() error {
		data, err := r.readFile(ctx, r.project, guestPath, http.StatusOK)
		if err != nil {
			return err
		}
		if string(data) != marker {
			return errors.New("guest workspace marker did not survive pause and resume")
		}
		return nil
	}); err != nil {
		return r.report, err
	}
	checkpointKey := randomKey("checkpoint")
	if err := r.step(ctx, "filesystem_checkpoint", func() error {
		var out struct {
			Resource struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
			} `json:"resource"`
		}
		body := map[string]any{"name": "conformance", "kind": "filesystem"}
		if err := r.expect(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(r.sandboxID)+"/checkpoints", r.project, checkpointKey, body, http.StatusAccepted, &out); err != nil {
			return err
		}
		if out.Resource.ID == "" || out.Resource.Kind != "filesystem" {
			return errors.New("checkpoint response did not confirm filesystem checkpoint")
		}
		r.checkpointID = out.Resource.ID
		return nil
	}); err != nil {
		return r.report, err
	}
	if err := r.step(ctx, "checkpoint_idempotency_payload_binding", func() error {
		body := map[string]any{"name": "changed", "kind": "filesystem"}
		return r.expect(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(r.sandboxID)+"/checkpoints", r.project, checkpointKey, body, http.StatusConflict, nil)
	}); err != nil {
		return r.report, err
	}
	if err := r.step(ctx, "ordered_events", func() error {
		var out struct {
			Events []struct {
				Sequence int64 `json:"sequence"`
			} `json:"events"`
		}
		if err := r.expect(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(r.sandboxID)+"/events", r.project, "", nil, http.StatusOK, &out); err != nil {
			return err
		}
		if len(out.Events) < 9 {
			return fmt.Errorf("event log contained %d events, want at least 9", len(out.Events))
		}
		for index := 1; index < len(out.Events); index++ {
			if out.Events[index].Sequence <= out.Events[index-1].Sequence {
				return errors.New("event sequence is not strictly increasing")
			}
		}
		return nil
	}); err != nil {
		return r.report, err
	}
	if err := r.step(ctx, "delete", func() error { return r.delete(ctx, "delete") }); err != nil {
		return r.report, err
	}
	if err := r.step(ctx, "verify_deleted_receipt", func() error {
		var envelope receipt.Envelope
		if err := r.expect(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(r.sandboxID)+"/receipt", r.project, "", nil, http.StatusOK, &envelope); err != nil {
			return err
		}
		if err := receipt.Verify(envelope, publicKey); err != nil {
			return fmt.Errorf("verify receipt: %w", err)
		}
		payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
		if err != nil {
			return err
		}
		var statement receipt.Statement
		if err := json.Unmarshal(payload, &statement); err != nil {
			return fmt.Errorf("decode receipt statement: %w", err)
		}
		if statement.Predicate.ProjectID != r.project || statement.Predicate.SandboxID != r.sandboxID || statement.Predicate.State != "deleted" {
			return errors.New("receipt identity or terminal state did not match the conformance resource")
		}
		return nil
	}); err != nil {
		return r.report, err
	}
	if err := r.step(ctx, "delete_checkpoint", func() error { return r.deleteCheckpoint(ctx, "delete_checkpoint") }); err != nil {
		return r.report, err
	}

	if err := r.step(ctx, "workspace_survives_sandbox_replacement", func() error {
		var out mutationEnvelope
		if err := r.expect(ctx, http.MethodPost, "/v1/sandboxes", r.project, randomKey("conformance-workspace-replacement"), createBody, http.StatusAccepted, &out); err != nil {
			return err
		}
		r.sandboxID, r.deleted = out.Resource.ID, false
		if r.sandboxID == "" || out.Resource.State != "running" {
			return fmt.Errorf("replacement sandbox was not confirmed running; state=%q", out.Resource.State)
		}
		data, err := r.readFile(ctx, r.project, guestPath, http.StatusOK)
		if err != nil {
			return err
		}
		if string(data) != marker {
			return errors.New("durable workspace marker did not survive sandbox replacement")
		}
		return nil
	}); err != nil {
		return r.report, err
	}
	if err := r.step(ctx, "delete_replacement", func() error { return r.delete(ctx, "delete_replacement") }); err != nil {
		return r.report, err
	}
	if err := r.step(ctx, "delete_workspace", func() error { return r.deleteWorkspace(ctx, "delete_workspace") }); err != nil {
		return r.report, err
	}

	r.report.Qualification = "sandbox_runtime_conformant"
	r.report.FinishedAt = time.Now().UTC()
	return r.report, nil
}

func (r *Runner) writeFile(ctx context.Context, project, path string, data []byte, wantStatus int) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, r.base.String()+"/v1/sandboxes/"+url.PathEscape(r.sandboxID)+"/files?path="+url.QueryEscape(path), bytes.NewReader(data))
	if err != nil {
		return err
	}
	r.authorize(request, project, "")
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := r.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if response.StatusCode != wantStatus {
		return fmt.Errorf("guest file write returned %d, want %d: %s", response.StatusCode, wantStatus, compact(body))
	}
	return nil
}

func (r *Runner) readFile(ctx context.Context, project, path string, wantStatus int) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base.String()+"/v1/sandboxes/"+url.PathEscape(r.sandboxID)+"/files?path="+url.QueryEscape(path), nil)
	if err != nil {
		return nil, err
	}
	r.authorize(request, project, "")
	response, err := r.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBytes {
		return nil, errors.New("guest file response exceeded conformance limit")
	}
	if response.StatusCode != wantStatus {
		return nil, fmt.Errorf("guest file read returned %d, want %d: %s", response.StatusCode, wantStatus, compact(body))
	}
	return body, nil
}

func (r *Runner) runCommand(ctx context.Context, project string, argv []string, wantOutput string, wantStatus int) error {
	body, _ := json.Marshal(map[string]any{"argv": argv, "timeout_seconds": 30})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base.String()+"/v1/sandboxes/"+url.PathEscape(r.sandboxID)+"/commands", bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.authorize(request, project, "")
	request.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxResponseBytes {
		return errors.New("guest command response exceeded conformance limit")
	}
	if response.StatusCode != wantStatus {
		return fmt.Errorf("guest command returned %d, want %d: %s", response.StatusCode, wantStatus, compact(data))
	}
	if wantStatus != http.StatusOK {
		return nil
	}
	var started, exited bool
	var output bytes.Buffer
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			Data     string `json:"data"`
			ExitCode int32  `json:"exit_code"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("decode command event: %w", err)
		}
		switch event.Type {
		case "started":
			started = true
		case "stdout":
			decoded, err := base64.StdEncoding.DecodeString(event.Data)
			if err != nil {
				return errors.New("stdout event was not valid base64")
			}
			output.Write(decoded)
		case "exited":
			if event.ExitCode != 0 {
				return fmt.Errorf("guest command exit code=%d", event.ExitCode)
			}
			exited = true
		case "error":
			return errors.New("guest command stream reported an error")
		}
	}
	if !started || !exited || (wantOutput != "" && output.String() != wantOutput) {
		return fmt.Errorf("guest command stream incomplete or output mismatch: started=%t exited=%t", started, exited)
	}
	return nil
}

func (r *Runner) authorize(request *http.Request, project, idempotency string) {
	request.Header.Set("Authorization", "Bearer "+r.token)
	request.Header.Set("X-Project-ID", project)
	if idempotency != "" {
		request.Header.Set("Idempotency-Key", idempotency)
	}
}

type mutationEnvelope struct {
	Resource struct {
		ID         string `json:"id"`
		RevisionID string `json:"revision_id"`
		State      string `json:"state"`
	} `json:"resource"`
}

type operationEnvelope struct {
	Operation struct {
		State string `json:"state"`
	} `json:"operation"`
}

func (r *Runner) delete(ctx context.Context, keyPrefix string) error {
	var out mutationEnvelope
	err := r.expect(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(r.sandboxID), r.project, randomKey(keyPrefix), nil, http.StatusAccepted, &out)
	if err == nil && out.Resource.State != "deleted" {
		err = fmt.Errorf("delete state=%q, want deleted", out.Resource.State)
	}
	if err == nil {
		r.deleted = true
	}
	return err
}

func (r *Runner) deleteWorkspace(ctx context.Context, keyPrefix string) error {
	var out mutationEnvelope
	err := r.expect(ctx, http.MethodDelete, "/v1/workspaces/"+url.PathEscape(r.workspaceID), r.project, randomKey(keyPrefix), nil, http.StatusAccepted, &out)
	if err == nil && out.Resource.State != "deleted" {
		err = fmt.Errorf("workspace delete state=%q, want deleted", out.Resource.State)
	}
	if err == nil {
		r.workspaceDeleted = true
	}
	return err
}

func (r *Runner) deleteCheckpoint(ctx context.Context, keyPrefix string) error {
	var out operationEnvelope
	err := r.expect(ctx, http.MethodDelete, "/v1/checkpoints/"+url.PathEscape(r.checkpointID), r.project, randomKey(keyPrefix), nil, http.StatusAccepted, &out)
	if err == nil && out.Operation.State != "succeeded" {
		err = fmt.Errorf("checkpoint delete operation=%q, want succeeded", out.Operation.State)
	}
	if err == nil {
		r.checkpointDeleted = true
	}
	return err
}

func (r *Runner) step(ctx context.Context, name string, action func() error) error {
	started := time.Now()
	err := action()
	step := Step{Name: name, Status: "passed", DurationMS: time.Since(started).Milliseconds()}
	if err != nil {
		step.Status = "failed"
		step.Detail = err.Error()
	}
	r.report.Steps = append(r.report.Steps, step)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (r *Runner) expect(ctx context.Context, method, path, project, idempotency string, body any, wantStatus int, out any) error {
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, r.base.String()+path, input)
	if err != nil {
		return err
	}
	r.authorize(request, project, idempotency)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := r.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxResponseBytes {
		return errors.New("control API response exceeded conformance limit")
	}
	if response.StatusCode != wantStatus {
		return fmt.Errorf("%s %s returned %d, want %d: %s", method, path, response.StatusCode, wantStatus, compact(data))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return nil
}

func randomKey(prefix string) string {
	data := make([]byte, 12)
	if _, err := rand.Read(data); err != nil {
		return prefix + "-fallback-" + fmt.Sprint(time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(data)
}

func compact(data []byte) string {
	value := strings.Join(strings.Fields(string(data)), " ")
	if len(value) > 256 {
		return value[:256] + "..."
	}
	return value
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
