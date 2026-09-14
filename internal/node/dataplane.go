// Package node defines the product-owned data-plane boundary between the
// authorized runtime service and the execution engine running a sandbox.
package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/infercrane/brezel/internal/backend"
)

// Capabilities are the guest operations that an attached node data plane can
// enforce. They are the intersection of the engine's declared capabilities
// and the interfaces it actually implements.
type Capabilities struct {
	CommandStreaming   bool
	FileReadWrite      bool
	AuthenticatedPorts bool
}

var (
	ErrInvalidBinding = errors.New("invalid sandbox binding")
	ErrStaleBinding   = errors.New("stale sandbox binding")
	ErrRevokedBinding = errors.New("revoked sandbox binding")
)

// BindingVerifier compares the handoff with the service's current durable
// sandbox identity. It must not perform network I/O or trust guest state.
type BindingVerifier func(projectID, sandboxID, backendID string, revision int64) bool

// SandboxBinding is an in-process, content-free authorization handoff. It is
// not a bearer credential and is not valid outside this process. The runtime
// service creates it only after project ownership, lifecycle state, expiry,
// mutation fencing, and quota admission have succeeded.
//
// Fields deliberately remain private so substrate identifiers cannot leak into
// public API objects or logs through ordinary serialization.
type SandboxBinding struct {
	projectID   string
	sandboxID   string
	backendID   string
	operationID string
	revision    int64
	expiresAt   time.Time
	now         func() time.Time
	verify      BindingVerifier
	revoked     *atomic.Bool
	authorized  bool
}

// BindAuthorizedSandbox creates the handoff used for one admitted guest
// operation. Callers must complete product authorization before calling it.
type BindingOption func(*SandboxBinding)

// WithBindingClock keeps handoff expiry on the same clock as the lifecycle
// service. It is primarily useful for deterministic lifecycle tests.
func WithBindingClock(now func() time.Time) BindingOption {
	return func(binding *SandboxBinding) { binding.now = now }
}

func BindAuthorizedSandbox(projectID, sandboxID, backendID, operationID string, revision int64, expiresAt time.Time, verify BindingVerifier, options ...BindingOption) (SandboxBinding, error) {
	for name, value := range map[string]string{
		"project id":   projectID,
		"sandbox id":   sandboxID,
		"backend id":   backendID,
		"operation id": operationID,
	} {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") {
			return SandboxBinding{}, fmt.Errorf("invalid %s", name)
		}
	}
	if revision < 1 {
		return SandboxBinding{}, errors.New("invalid sandbox revision")
	}
	if verify == nil {
		return SandboxBinding{}, errors.New("sandbox binding verifier is required")
	}
	binding := SandboxBinding{
		projectID:   projectID,
		sandboxID:   sandboxID,
		backendID:   backendID,
		operationID: operationID,
		revision:    revision,
		expiresAt:   expiresAt,
		now:         time.Now,
		verify:      verify,
		revoked:     &atomic.Bool{},
		authorized:  true,
	}
	for _, option := range options {
		if option != nil {
			option(&binding)
		}
	}
	if binding.now == nil || expiresAt.IsZero() || !binding.now().Before(expiresAt) {
		return SandboxBinding{}, errors.New("invalid sandbox binding expiry")
	}
	return binding, nil
}

// String is intentionally content-minimal. In particular, it excludes the
// substrate identifier used to address the engine.
func (b SandboxBinding) String() string {
	if !b.authorized {
		return "node sandbox binding (invalid)"
	}
	return fmt.Sprintf("node sandbox binding project=%q sandbox=%q operation=%q revision=%d", b.projectID, b.sandboxID, b.operationID, b.revision)
}

func (b SandboxBinding) validate() error {
	if !b.authorized || strings.TrimSpace(b.projectID) == "" || strings.TrimSpace(b.sandboxID) == "" || strings.TrimSpace(b.backendID) == "" || strings.TrimSpace(b.operationID) == "" || b.revision < 1 || b.verify == nil || b.revoked == nil || b.now == nil || b.expiresAt.IsZero() {
		return ErrInvalidBinding
	}
	if b.revoked.Load() {
		return ErrRevokedBinding
	}
	if !b.now().Before(b.expiresAt) || !b.verify(b.projectID, b.sandboxID, b.backendID, b.revision) {
		return ErrStaleBinding
	}
	return nil
}

// Revoke prevents any subsequent use of this handoff. Copies share the same
// revocation state.
func (b SandboxBinding) Revoke() {
	if b.revoked != nil {
		b.revoked.Store(true)
	}
}

// DataPlane is the product-owned node-facing path for commands, file transfer,
// and authenticated application traffic. Implementations must never return a
// guest-management or substrate routing credential to the caller.
type DataPlane interface {
	ownedNodeDataPlane()
	Capabilities() Capabilities
	Run(context.Context, SandboxBinding, backend.CommandRequest, func(backend.CommandEvent) error) error
	WriteFile(context.Context, SandboxBinding, string, io.Reader) (backend.FileInfo, error)
	ReadFile(context.Context, SandboxBinding, string, io.Writer) (backend.FileInfo, error)
	ValidatePort(uint16) error
	RoundTripPort(context.Context, SandboxBinding, uint16, *http.Request) (*http.Response, error)
}

// BackendDataPlane adapts the initial in-process engine to the owned node
// boundary. E2B-specific routing and guest credentials remain encapsulated by
// its backend implementation.
type BackendDataPlane struct {
	guest backend.GuestRuntime
	ports backend.PortRuntime
	caps  Capabilities
}

func (*BackendDataPlane) ownedNodeDataPlane() {}

func NewBackendDataPlane(engine backend.Backend) *BackendDataPlane {
	if engine == nil {
		return &BackendDataPlane{}
	}
	declared := engine.Capabilities()
	guest, hasGuest := engine.(backend.GuestRuntime)
	ports, hasPorts := engine.(backend.PortRuntime)
	return &BackendDataPlane{
		guest: guest,
		ports: ports,
		caps: Capabilities{
			CommandStreaming:   declared.CommandStreaming && hasGuest,
			FileReadWrite:      declared.FileReadWrite && hasGuest,
			AuthenticatedPorts: declared.AuthenticatedPorts && hasPorts,
		},
	}
}

func (d *BackendDataPlane) Capabilities() Capabilities {
	if d == nil {
		return Capabilities{}
	}
	return d.caps
}

func (d *BackendDataPlane) Run(ctx context.Context, binding SandboxBinding, request backend.CommandRequest, emit func(backend.CommandEvent) error) error {
	if err := binding.validate(); err != nil {
		return err
	}
	if d == nil || d.guest == nil || !d.caps.CommandStreaming {
		return backend.ErrCapabilityUnavailable
	}
	return d.guest.Run(ctx, binding.backendID, request, emit)
}

func (d *BackendDataPlane) WriteFile(ctx context.Context, binding SandboxBinding, path string, source io.Reader) (backend.FileInfo, error) {
	if err := binding.validate(); err != nil {
		return backend.FileInfo{}, err
	}
	if d == nil || d.guest == nil || !d.caps.FileReadWrite {
		return backend.FileInfo{}, backend.ErrCapabilityUnavailable
	}
	return d.guest.WriteFile(ctx, binding.backendID, path, source)
}

func (d *BackendDataPlane) ReadFile(ctx context.Context, binding SandboxBinding, path string, destination io.Writer) (backend.FileInfo, error) {
	if err := binding.validate(); err != nil {
		return backend.FileInfo{}, err
	}
	if d == nil || d.guest == nil || !d.caps.FileReadWrite {
		return backend.FileInfo{}, backend.ErrCapabilityUnavailable
	}
	return d.guest.ReadFile(ctx, binding.backendID, path, destination)
}

func (d *BackendDataPlane) ValidatePort(port uint16) error {
	if d == nil || d.ports == nil || !d.caps.AuthenticatedPorts {
		return backend.ErrCapabilityUnavailable
	}
	return d.ports.ValidatePort(port)
}

func (d *BackendDataPlane) RoundTripPort(ctx context.Context, binding SandboxBinding, port uint16, request *http.Request) (*http.Response, error) {
	if err := binding.validate(); err != nil {
		return nil, err
	}
	if d == nil || d.ports == nil || !d.caps.AuthenticatedPorts {
		return nil, backend.ErrCapabilityUnavailable
	}
	return d.ports.RoundTripPort(ctx, binding.backendID, port, request)
}

var _ DataPlane = (*BackendDataPlane)(nil)
