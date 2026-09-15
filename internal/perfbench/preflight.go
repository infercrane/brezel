package perfbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/infercrane/brezel/internal/domain"
)

// ProjectPreflightConfig identifies the authenticated project whose existing
// live resources could invalidate a benchmark matrix.
type ProjectPreflightConfig struct {
	BaseURL   string
	Token     string
	ProjectID string
	Client    *http.Client
}

// ProjectPreflightReport is content-minimal evidence that the project had no
// non-terminal load-bearing resources before the benchmark created anything.
type ProjectPreflightReport struct {
	SchemaVersion      int       `json:"schema_version"`
	CheckedAt          time.Time `json:"checked_at"`
	ProjectID          string    `json:"project_id"`
	ActiveSandboxes    int       `json:"active_sandboxes"`
	ActiveWorkspaces   int       `json:"active_workspaces"`
	SandboxRequestID   string    `json:"sandbox_request_id,omitempty"`
	WorkspaceRequestID string    `json:"workspace_request_id,omitempty"`
	Empty              bool      `json:"empty"`
}

// InspectEmptyProject reads only project-scoped list endpoints. It returns the
// observed report even when the project is non-empty so a rejected benchmark
// can retain evidence without recording resource identities.
func InspectEmptyProject(ctx context.Context, config ProjectPreflightConfig) (ProjectPreflightReport, error) {
	report := ProjectPreflightReport{SchemaVersion: 1, CheckedAt: time.Now().UTC(), ProjectID: config.ProjectID}
	if len(config.Token) < 32 || strings.ContainsAny(config.Token, "\r\n") {
		return report, errors.New("service token must be at least 32 characters and contain no newlines")
	}
	if err := domain.ValidateProjectID(config.ProjectID); err != nil {
		return report, fmt.Errorf("invalid benchmark project: %w", err)
	}
	base, err := benchmarkBaseURL(config.BaseURL)
	if err != nil {
		return report, err
	}
	client := newAPIClient(Config{Token: config.Token, ProjectID: config.ProjectID, Client: config.Client}, base)

	var sandboxes struct {
		Sandboxes json.RawMessage `json:"sandboxes"`
	}
	report.SandboxRequestID, err = client.json(ctx, http.MethodGet, "/v1/sandboxes?include_terminal=false", "", nil, http.StatusOK, &sandboxes)
	if err != nil {
		return report, fmt.Errorf("inspect benchmark sandboxes: %w", err)
	}
	report.ActiveSandboxes, err = countExplicitArray(sandboxes.Sandboxes, "sandboxes")
	if err != nil {
		return report, err
	}

	var workspaces struct {
		Workspaces json.RawMessage `json:"workspaces"`
	}
	report.WorkspaceRequestID, err = client.json(ctx, http.MethodGet, "/v1/workspaces?include_terminal=false", "", nil, http.StatusOK, &workspaces)
	if err != nil {
		return report, fmt.Errorf("inspect benchmark workspaces: %w", err)
	}
	report.ActiveWorkspaces, err = countExplicitArray(workspaces.Workspaces, "workspaces")
	if err != nil {
		return report, err
	}
	report.Empty = report.ActiveSandboxes == 0 && report.ActiveWorkspaces == 0
	if !report.Empty {
		return report, fmt.Errorf("benchmark project is not empty: %d active sandboxes and %d active workspaces", report.ActiveSandboxes, report.ActiveWorkspaces)
	}
	return report, nil
}

func countExplicitArray(raw json.RawMessage, field string) (int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, fmt.Errorf("inspect benchmark %s: response omitted an explicit %s array", field, field)
	}
	var resources []json.RawMessage
	if err := json.Unmarshal(raw, &resources); err != nil {
		return 0, fmt.Errorf("inspect benchmark %s: response field is not an array", field)
	}
	return len(resources), nil
}

func benchmarkBaseURL(value string) (*url.URL, error) {
	base, err := url.Parse(strings.TrimRight(value, "/"))
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return nil, errors.New("base URL must be an absolute HTTP(S) URL")
	}
	if base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("base URL must not contain credentials, query, or fragment")
	}
	if base.Scheme == "http" && !loopbackHost(base.Hostname()) {
		return nil, errors.New("plaintext benchmark traffic is allowed only for a loopback control API")
	}
	return base, nil
}
