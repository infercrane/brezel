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

	"github.com/infercrane/sandbox-runtime-lab/internal/receipt"
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
	base         *url.URL
	token        string
	project      string
	otherProject string
	template     string
	target       string
	client       *http.Client
	report       Report
	sandboxID    string
	deleted      bool
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
		Scope:     "control API lifecycle, tenant isolation, filesystem checkpoint, cleanup, and receipt signature",
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
		}
		if err := r.expect(ctx, http.MethodGet, "/v1/capabilities", r.project, "", nil, http.StatusOK, &out); err != nil {
			return err
		}
		if out.Qualification == "" {
			return errors.New("capability response omitted qualification state")
		}
		return nil
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
			"name": "conformance", "backend": "e2b", "backend_template": r.template,
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
		"network": map[string]any{"allow_internet": false},
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

	if err := r.step(ctx, "cross_project_denial", func() error {
		return r.expect(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(r.sandboxID), r.otherProject, "", nil, http.StatusNotFound, nil)
	}); err != nil {
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
	if err := r.step(ctx, "filesystem_checkpoint", func() error {
		var out struct {
			Resource struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
			} `json:"resource"`
		}
		body := map[string]any{"name": "conformance", "kind": "filesystem"}
		if err := r.expect(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(r.sandboxID)+"/checkpoints", r.project, randomKey("checkpoint"), body, http.StatusAccepted, &out); err != nil {
			return err
		}
		if out.Resource.ID == "" || out.Resource.Kind != "filesystem" {
			return errors.New("checkpoint response did not confirm filesystem checkpoint")
		}
		return nil
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
		if len(out.Events) < 5 {
			return fmt.Errorf("event log contained %d events, want at least 5", len(out.Events))
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

	r.report.Qualification = "control_plane_conformant"
	r.report.FinishedAt = time.Now().UTC()
	return r.report, nil
}

type mutationEnvelope struct {
	Resource struct {
		ID         string `json:"id"`
		RevisionID string `json:"revision_id"`
		State      string `json:"state"`
	} `json:"resource"`
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
	request.Header.Set("Authorization", "Bearer "+r.token)
	request.Header.Set("X-Project-ID", project)
	if idempotency != "" {
		request.Header.Set("Idempotency-Key", idempotency)
	}
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
