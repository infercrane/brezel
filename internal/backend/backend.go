package backend

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/infercrane/brezel/internal/domain"
)

// DefaultName is the product-facing identity of the bundled hostile-code
// execution engine. Substrate implementations must not leak through the API.
const DefaultName = "microvm"

var (
	ErrNotFound              = errors.New("backend resource not found")
	ErrCapabilityUnavailable = errors.New("backend capability unavailable")
	// ErrCapacityUnavailable is returned only when the backend has confirmed
	// that it did not create a resource. Callers may safely retry elsewhere or
	// report deterministic capacity exhaustion instead of an unknown state.
	ErrCapacityUnavailable = errors.New("backend capacity unavailable")
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
	CommandStreaming         bool `json:"command_streaming"`
	FileReadWrite            bool `json:"file_read_write"`
	AuthenticatedPorts       bool `json:"authenticated_ports"`
	DurableWorkspaces        bool `json:"durable_workspaces"`
}

type CommandRequest struct {
	Argv []string
	Cwd  string
	Env  map[string]string
}

type CommandEventType string

const (
	CommandStarted CommandEventType = "started"
	CommandStdout  CommandEventType = "stdout"
	CommandStderr  CommandEventType = "stderr"
	CommandExited  CommandEventType = "exited"
)

type CommandEvent struct {
	ExecutionID string           `json:"execution_id,omitempty"`
	Type        CommandEventType `json:"type"`
	PID         uint32           `json:"pid,omitempty"`
	Data        []byte           `json:"data,omitempty"`
	ExitCode    int32            `json:"exit_code,omitempty"`
	Exited      bool             `json:"exited,omitempty"`
	Status      string           `json:"status,omitempty"`
	Error       string           `json:"error,omitempty"`
}

type FileInfo struct {
	Path        string `json:"path"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size"`
}

// GuestRuntime is the engine-side guest primitive consumed by the product-owned
// node data-plane adapter. It is not called directly by the runtime service.
// Implementations must authenticate the guest-management endpoint without
// returning its credential to callers. File and process content is streamed
// through the caller and is not durable control state.
type GuestRuntime interface {
	Run(context.Context, string, CommandRequest, func(CommandEvent) error) error
	WriteFile(context.Context, string, string, io.Reader) (FileInfo, error)
	ReadFile(context.Context, string, string, io.Writer) (FileInfo, error)
}

// PortRuntime is the engine-side HTTP primitive consumed by the product-owned
// node adapter. The implementation injects substrate routing credentials only
// on the internal hop and never returns them to the caller.
type PortRuntime interface {
	ValidatePort(uint16) error
	RoundTripPort(context.Context, string, uint16, *http.Request) (*http.Response, error)
}

type CreateRequest struct {
	LocalSandboxID  string
	ProjectID       string
	TemplateID      string
	Lifecycle       domain.Lifecycle
	Network         domain.NetworkPolicy
	Environment     map[string]string
	WorkspaceMounts []WorkspaceMount
}

type WorkspaceMount struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type WorkspaceCreateRequest struct {
	LocalWorkspaceID string
	ProjectID        string
	Name             string
}

type Workspace struct {
	ID   string
	Name string
}

type WorkspaceRuntime interface {
	CreateWorkspace(context.Context, WorkspaceCreateRequest) (Workspace, error)
	FindWorkspace(context.Context, string) (Workspace, error)
	DeleteWorkspace(context.Context, string) error
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
	DeleteCheckpoint(context.Context, string) error
}

// ReadinessBackend is implemented by release backends that can prove their
// control endpoint is reachable. Readiness failures must not be treated as a
// sandbox state transition.
type ReadinessBackend interface {
	Ready(context.Context) error
}

// TimeoutRuntime resets a running sandbox's hard engine deadline. Warm
// capacity requires this primitive so a claimed slot receives the customer's
// TTL instead of retaining the longer pool housekeeping lifetime.
type TimeoutRuntime interface {
	SetTimeout(context.Context, string, int64) error
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
