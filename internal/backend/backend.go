package backend

import (
	"context"
	"errors"

	"github.com/infercrane/sandbox-runtime-lab/internal/domain"
)

var (
	ErrNotFound              = errors.New("backend resource not found")
	ErrCapabilityUnavailable = errors.New("backend capability unavailable")
)

type Capabilities struct {
	HostileCodeIsolation     bool `json:"hostile_code_isolation"`
	DenyByDefaultEgress      bool `json:"deny_by_default_egress"`
	FilesystemStandby        bool `json:"filesystem_standby"`
	FullStateStandby         bool `json:"full_state_standby"`
	FilesystemCheckpoint     bool `json:"filesystem_checkpoint"`
	FullStateCheckpoint      bool `json:"full_state_checkpoint"`
	AutoResume               bool `json:"auto_resume"`
	EndpointCredentialBroker bool `json:"endpoint_credential_broker"`
}

type CreateRequest struct {
	LocalSandboxID string
	ProjectID      string
	TemplateID     string
	Lifecycle      domain.Lifecycle
	Network        domain.NetworkPolicy
	Environment    map[string]string
}

type Sandbox struct {
	ID    string
	State domain.SandboxState
}

type Checkpoint struct {
	Ref  string
	Kind domain.CheckpointKind
}

type Backend interface {
	Name() string
	Capabilities() Capabilities
	Create(context.Context, CreateRequest) (Sandbox, error)
	Find(context.Context, string, string) (Sandbox, error)
	Inspect(context.Context, string) (Sandbox, error)
	Pause(context.Context, string, domain.CheckpointKind) error
	Resume(context.Context, string, domain.CheckpointKind, int64) (Sandbox, error)
	Delete(context.Context, string) error
	Checkpoint(context.Context, string, domain.CheckpointKind, string) (Checkpoint, error)
}

func Preflight(c Capabilities, lifecycle domain.Lifecycle, network domain.NetworkPolicy, connectorRevisions []string) error {
	if !c.HostileCodeIsolation {
		return errors.New("backend cannot enforce the hostile-code isolation profile")
	}
	if !network.AllowInternet && !c.DenyByDefaultEgress {
		return errors.New("backend cannot enforce deny-by-default egress")
	}
	if lifecycle.StandbyAfterSeconds > 0 {
		switch lifecycle.StandbyCheckpoint {
		case domain.CheckpointFilesystem:
			if !c.FilesystemStandby {
				return errors.New("backend cannot enforce filesystem-only standby")
			}
		case domain.CheckpointFullState:
			if !c.FullStateStandby {
				return errors.New("backend cannot enforce full-state standby")
			}
		}
		if lifecycle.AutoResume && !c.AutoResume {
			return errors.New("backend cannot enforce automatic resume")
		}
	}
	if len(connectorRevisions) > 0 && !c.EndpointCredentialBroker {
		return errors.New("backend cannot enforce endpoint-bound credential brokering")
	}
	return nil
}
