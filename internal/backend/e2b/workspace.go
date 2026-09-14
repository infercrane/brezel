package e2b

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/infercrane/brezel/internal/backend"
)

type volumeResponse struct {
	VolumeID string `json:"volumeID"`
	Name     string `json:"name"`
	Token    string `json:"token,omitempty"`
}

func (c *Client) CreateWorkspace(ctx context.Context, in backend.WorkspaceCreateRequest) (backend.Workspace, error) {
	if in.LocalWorkspaceID == "" || in.ProjectID == "" || strings.ContainsAny(in.LocalWorkspaceID+in.ProjectID, "\r\n") {
		return backend.Workspace{}, errors.New("workspace identity is invalid")
	}
	// The product-generated local ID is globally unique inside this engine and
	// does not expose the customer's display name to the substrate.
	engineName := in.LocalWorkspaceID
	var out volumeResponse
	if err := c.do(ctx, http.MethodPost, "/volumes", map[string]string{"name": engineName}, &out, http.StatusCreated); err != nil {
		return backend.Workspace{}, err
	}
	if out.VolumeID == "" || out.Name != engineName {
		return backend.Workspace{}, errors.New("microVM engine returned an invalid workspace identity")
	}
	return backend.Workspace{ID: out.VolumeID, Name: out.Name}, nil
}

func (c *Client) FindWorkspace(ctx context.Context, engineName string) (backend.Workspace, error) {
	if engineName == "" || strings.ContainsAny(engineName, "\r\n") {
		return backend.Workspace{}, errors.New("workspace name is invalid")
	}
	var out []volumeResponse
	if err := c.do(ctx, http.MethodGet, "/volumes", nil, &out, http.StatusOK); err != nil {
		return backend.Workspace{}, err
	}
	for _, candidate := range out {
		if candidate.Name == engineName && candidate.VolumeID != "" {
			return backend.Workspace{ID: candidate.VolumeID, Name: candidate.Name}, nil
		}
	}
	return backend.Workspace{}, backend.ErrNotFound
}

func (c *Client) DeleteWorkspace(ctx context.Context, id string) error {
	if id == "" || strings.ContainsAny(id, "\r\n") {
		return errors.New("workspace id is invalid")
	}
	return c.do(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(id), nil, nil, http.StatusNoContent)
}

var _ backend.WorkspaceRuntime = (*Client)(nil)
