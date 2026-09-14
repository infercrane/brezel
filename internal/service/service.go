package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/infercrane/brezel/internal/backend"
	"github.com/infercrane/brezel/internal/connector"
	"github.com/infercrane/brezel/internal/domain"
	"github.com/infercrane/brezel/internal/node"
	"github.com/infercrane/brezel/internal/receipt"
	"github.com/infercrane/brezel/internal/store"
	"github.com/infercrane/brezel/internal/telemetry"
)

var (
	ErrInvalid  = errors.New("invalid request")
	ErrNotFound = errors.New("resource not found")
	ErrConflict = errors.New("resource conflict")
	ErrDenied   = errors.New("request denied")
	ErrQuota    = errors.New("project quota exceeded")
	ErrBackend  = errors.New("backend failure")
)

var safeIdempotencyKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,199}$`)

const DefaultStandbyGraceSeconds int64 = 15

type Limits struct {
	MaxActiveSandboxesPerProject    int
	MaxWorkspacesPerProject         int
	MaxConcurrentGuestOpsPerProject int
	MaxEnvironmentsPerProject       int
	MaxConnectorsPerProject         int
}

var DefaultLimits = Limits{
	MaxActiveSandboxesPerProject:    64,
	MaxWorkspacesPerProject:         64,
	MaxConcurrentGuestOpsPerProject: 128,
	MaxEnvironmentsPerProject:       256,
	MaxConnectorsPerProject:         256,
}

type Service struct {
	mu                      sync.Mutex
	store                   store.Store
	backend                 backend.Backend
	dataPlane               node.DataPlane
	routeAdmin              NodeRouteAdministrator
	broker                  *connector.Broker
	signer                  *receipt.Signer
	now                     func() time.Time
	activeGuestOps          map[string]map[string]context.CancelFunc
	activeGuestOpsByProject map[string]int
	activeBackendMutations  map[string]struct{}
	recentActivity          map[string]time.Time
	limits                  Limits
	observer                telemetry.Observer
}

// NodeRouteAdministrator is the private lifecycle authority for a configured
// execution node. The public API never receives these requests or results.
type NodeRouteAdministrator interface {
	Resolve(context.Context, string) (node.RouteAdminResult, error)
	Bind(context.Context, node.RouteBindRequest) (node.RouteAdminResult, error)
	AttachReady(context.Context, node.RouteTransitionRequest) (node.RouteAdminResult, error)
	Drain(context.Context, node.RouteTransitionRequest) (node.RouteAdminResult, error)
	Standby(context.Context, node.RouteTransitionRequest) (node.RouteAdminResult, error)
	Rebind(context.Context, node.RouteRebindRequest) (node.RouteAdminResult, error)
	Release(context.Context, node.RouteTransitionRequest) (node.RouteAdminResult, error)
	Remove(context.Context, node.RouteTransitionRequest) (node.RouteRemoveResult, error)
}

type Option func(*Service)

func WithConnectorBroker(broker *connector.Broker) Option {
	return func(s *Service) { s.broker = broker }
}

func WithLimits(limits Limits) Option {
	return func(s *Service) { s.limits = limits }
}

// WithPhaseObserver records fixed, content-free operation phase durations.
func WithPhaseObserver(observer telemetry.Observer) Option {
	return func(s *Service) { s.observer = observer }
}

// WithNodeDataPlane replaces the in-process engine adapter. The runtime
// service remains the only caller and still performs project authorization,
// lifecycle fencing, expiry, and quota admission before creating a binding.
func WithNodeDataPlane(dataPlane node.DataPlane) Option {
	return func(s *Service) { s.dataPlane = dataPlane }
}

// WithNodeRouteAdministrator enables the durable node route lifecycle needed
// by RelayDataPlane. Supplying only one half fails service construction.
func WithNodeRouteAdministrator(administrator NodeRouteAdministrator) Option {
	return func(s *Service) { s.routeAdmin = administrator }
}

// WithClock supplies the lifecycle clock for embedded runtimes and
// deterministic tests. Production callers should normally use the default UTC
// wall clock.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

func New(st store.Store, be backend.Backend, signer *receipt.Signer, options ...Option) (*Service, error) {
	if st == nil || be == nil {
		return nil, errors.New("store and backend are required")
	}
	if !be.Capabilities().HostileCodeIsolation {
		return nil, errors.New("release backend must enforce hostile-code isolation")
	}
	s := &Service{
		store:                   st,
		backend:                 be,
		dataPlane:               node.NewBackendDataPlane(be),
		signer:                  signer,
		now:                     func() time.Time { return time.Now().UTC() },
		activeGuestOps:          make(map[string]map[string]context.CancelFunc),
		activeGuestOpsByProject: make(map[string]int),
		activeBackendMutations:  make(map[string]struct{}),
		recentActivity:          make(map[string]time.Time),
		limits:                  DefaultLimits,
	}
	for _, option := range options {
		option(s)
	}
	if s.now == nil {
		return nil, errors.New("runtime clock is required")
	}
	if s.dataPlane == nil {
		return nil, errors.New("node data plane is required")
	}
	if routed, ok := s.dataPlane.(interface{ RequiresNodeAssignment() bool }); ok && routed.RequiresNodeAssignment() && s.routeAdmin == nil {
		return nil, errors.New("remote node data plane requires a node route administrator")
	}
	if s.limits.MaxActiveSandboxesPerProject < 1 || s.limits.MaxWorkspacesPerProject < 1 || s.limits.MaxConcurrentGuestOpsPerProject < 1 || s.limits.MaxEnvironmentsPerProject < 1 || s.limits.MaxConnectorsPerProject < 1 {
		return nil, errors.New("all runtime limits must be positive")
	}
	return s, nil
}

// Ready checks durable control state and the release backend without mutating
// either. A backend that does not expose readiness is rejected rather than
// producing a false-positive production signal.
func (s *Service) Ready(ctx context.Context) error {
	state, ok := s.store.(interface{ Ready() error })
	if !ok {
		return errors.New("state store does not expose readiness")
	}
	if err := state.Ready(); err != nil {
		return fmt.Errorf("state store: %w", err)
	}
	runtime, ok := s.backend.(backend.ReadinessBackend)
	if !ok {
		return errors.New("backend does not expose readiness")
	}
	if err := runtime.Ready(ctx); err != nil {
		return fmt.Errorf("backend: %w", err)
	}
	if dataPlane, ok := s.dataPlane.(interface{ Ready(context.Context) error }); ok {
		if err := dataPlane.Ready(ctx); err != nil {
			return fmt.Errorf("node data plane: %w", err)
		}
	}
	return nil
}

func (s *Service) Capabilities() backend.Capabilities {
	capabilities := s.backend.Capabilities()
	dataPlane := s.dataPlane.Capabilities()
	capabilities.CommandStreaming = dataPlane.CommandStreaming
	capabilities.FileReadWrite = dataPlane.FileReadWrite
	capabilities.AuthenticatedPorts = dataPlane.AuthenticatedPorts
	capabilities.EndpointCredentialBroker = s.broker != nil
	return capabilities
}

func (s *Service) ReceiptPublicKey() (ed25519.PublicKey, string, bool) {
	if s.signer == nil {
		return nil, "", false
	}
	return s.signer.PublicKey(), s.signer.KeyID(), true
}

type CreateEnvironmentInput struct {
	Name           string `json:"name"`
	Template       string `json:"template"`
	ImageDigest    string `json:"image_digest,omitempty"`
	PolicyRevision string `json:"policy_revision,omitempty"`
}

type CreateConnectorInput struct {
	Name                string   `json:"name"`
	Destination         string   `json:"destination"`
	AllowedMethods      []string `json:"allowed_methods"`
	AllowedPaths        []string `json:"allowed_paths"`
	CredentialRef       string   `json:"credential_ref"`
	AllowPrivateNetwork bool     `json:"allow_private_network,omitempty"`
}

type CreateSandboxInput struct {
	EnvironmentRevision string                  `json:"environment_revision,omitempty"`
	CheckpointID        string                  `json:"checkpoint_id,omitempty"`
	Lifecycle           domain.Lifecycle        `json:"lifecycle"`
	Network             domain.NetworkPolicy    `json:"network"`
	ConnectorRevisions  []string                `json:"connector_revisions,omitempty"`
	WorkspaceMounts     []domain.WorkspaceMount `json:"workspace_mounts,omitempty"`
}

func (s *Service) CreateEnvironment(projectID, idempotencyKey string, in CreateEnvironmentInput) (domain.Environment, domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := requireMutation(projectID, idempotencyKey); err != nil {
		return domain.Environment{}, domain.Operation{}, err
	}
	now := s.now()
	env := domain.Environment{ProjectID: projectID, Name: in.Name, Backend: s.backend.Name(), BackendTemplate: in.Template, ImageDigest: in.ImageDigest, PolicyRevision: in.PolicyRevision, CreatedAt: now}
	if err := domain.ValidateEnvironment(env); err != nil {
		return env, domain.Operation{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	inputDigest, err := mutationDigest(in)
	if err != nil {
		return env, domain.Operation{}, err
	}
	revision, err := revisionID("envr", struct {
		ProjectID, Name, Backend, BackendTemplate, ImageDigest, PolicyRevision string
	}{env.ProjectID, env.Name, env.Backend, env.BackendTemplate, env.ImageDigest, env.PolicyRevision})
	if err != nil {
		return env, domain.Operation{}, err
	}
	env.RevisionID = revision
	op := succeededOperation(projectID, "create_environment", revision, idempotencyKey, now)
	err = s.store.Update(func(state *store.State) error {
		if existing, ok := idempotentOperation(*state, projectID, "create_environment", idempotencyKey); ok {
			if err := requireIdempotencyDigest(*state, projectID, "create_environment", idempotencyKey, inputDigest); err != nil {
				return err
			}
			op = existing
			env = state.Environments[store.ScopedKey(projectID, existing.ResourceID)]
			return nil
		}
		key := store.ScopedKey(projectID, revision)
		if existing, ok := state.Environments[key]; ok {
			// Environment revisions are content-addressed and immutable. A retry
			// through another client-generated idempotency key must converge on
			// the existing revision instead of turning a successful declaration
			// into a conflict or consuming environment quota.
			env = existing
			state.Operations[store.ScopedKey(projectID, op.ID)] = op
			state.Idempotency[store.IdempotencyKey(projectID, "create_environment", idempotencyKey)] = op.ID
			state.IdempotencyDigests[store.IdempotencyKey(projectID, "create_environment", idempotencyKey)] = inputDigest
			return nil
		}
		count := 0
		for _, existing := range state.Environments {
			if existing.ProjectID == projectID {
				count++
			}
		}
		if count >= s.limits.MaxEnvironmentsPerProject {
			return ErrQuota
		}
		state.Environments[key] = env
		state.Operations[store.ScopedKey(projectID, op.ID)] = op
		state.Idempotency[store.IdempotencyKey(projectID, "create_environment", idempotencyKey)] = op.ID
		state.IdempotencyDigests[store.IdempotencyKey(projectID, "create_environment", idempotencyKey)] = inputDigest
		return nil
	})
	if errors.Is(err, store.ErrConflict) {
		return env, op, ErrConflict
	}
	return env, op, err
}

func (s *Service) CreateConnector(projectID, idempotencyKey string, in CreateConnectorInput) (domain.Connector, domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := requireMutation(projectID, idempotencyKey); err != nil {
		return domain.Connector{}, domain.Operation{}, err
	}
	now := s.now()
	status := "registered_unenforced"
	if s.broker != nil {
		status = "broker_configured_unqualified"
	}
	connector := domain.Connector{ProjectID: projectID, Name: in.Name, Destination: in.Destination, AllowedMethods: in.AllowedMethods, AllowedPaths: in.AllowedPaths, CredentialRef: in.CredentialRef, AllowPrivateNetwork: in.AllowPrivateNetwork, Status: status, CreatedAt: now}
	if err := domain.ValidateConnector(connector); err != nil {
		return connector, domain.Operation{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	inputDigest, err := mutationDigest(in)
	if err != nil {
		return connector, domain.Operation{}, err
	}
	revision, err := revisionID("connr", struct {
		ProjectID, Name, Destination, CredentialRef string
		AllowedMethods, AllowedPaths                []string
		AllowPrivateNetwork                         bool
	}{projectID, in.Name, in.Destination, in.CredentialRef, in.AllowedMethods, in.AllowedPaths, in.AllowPrivateNetwork})
	if err != nil {
		return connector, domain.Operation{}, err
	}
	connector.RevisionID = revision
	op := succeededOperation(projectID, "create_connector", revision, idempotencyKey, now)
	err = s.store.Update(func(state *store.State) error {
		if existing, ok := idempotentOperation(*state, projectID, "create_connector", idempotencyKey); ok {
			if err := requireIdempotencyDigest(*state, projectID, "create_connector", idempotencyKey, inputDigest); err != nil {
				return err
			}
			op = existing
			connector = state.Connectors[store.ScopedKey(projectID, existing.ResourceID)]
			return nil
		}
		count := 0
		for _, existing := range state.Connectors {
			if existing.ProjectID == projectID {
				count++
			}
		}
		if count >= s.limits.MaxConnectorsPerProject {
			return ErrQuota
		}
		state.Connectors[store.ScopedKey(projectID, revision)] = connector
		state.Operations[store.ScopedKey(projectID, op.ID)] = op
		state.Idempotency[store.IdempotencyKey(projectID, "create_connector", idempotencyKey)] = op.ID
		state.IdempotencyDigests[store.IdempotencyKey(projectID, "create_connector", idempotencyKey)] = inputDigest
		return nil
	})
	return connector, op, err
}

func (s *Service) CreateSandbox(ctx context.Context, projectID, idempotencyKey string, in CreateSandboxInput) (domain.Sandbox, domain.Operation, error) {
	s.mu.Lock()
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()
	if err := requireMutation(projectID, idempotencyKey); err != nil {
		return domain.Sandbox{}, domain.Operation{}, err
	}
	if in.Lifecycle.StandbyCheckpoint == "" {
		in.Lifecycle.StandbyCheckpoint = domain.CheckpointFullState
	}
	if in.Lifecycle.StandbyAfterSeconds > 0 && in.Lifecycle.StandbyGraceSeconds == 0 {
		in.Lifecycle.StandbyGraceSeconds = DefaultStandbyGraceSeconds
	}
	if len(in.ConnectorRevisions) > 0 && s.broker != nil {
		in.Network.AllowOut = appendUnique(in.Network.AllowOut, s.broker.GatewayHost())
	}
	if err := domain.ValidateLifecycle(in.Lifecycle); err != nil {
		return domain.Sandbox{}, domain.Operation{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := domain.ValidateNetwork(in.Network); err != nil {
		return domain.Sandbox{}, domain.Operation{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := domain.ValidateWorkspaceMounts(in.WorkspaceMounts); err != nil {
		return domain.Sandbox{}, domain.Operation{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if (in.EnvironmentRevision == "") == (in.CheckpointID == "") {
		return domain.Sandbox{}, domain.Operation{}, fmt.Errorf("%w: exactly one of environment_revision or checkpoint_id is required", ErrInvalid)
	}
	inputDigest, err := mutationDigest(in)
	if err != nil {
		return domain.Sandbox{}, domain.Operation{}, err
	}
	var env domain.Environment
	var sourceCheckpoint domain.Checkpoint
	var workspaceMounts []backend.WorkspaceMount
	var existingOp domain.Operation
	err = s.store.View(func(state store.State) error {
		if op, ok := idempotentOperation(state, projectID, "create_sandbox", idempotencyKey); ok {
			if err := requireIdempotencyDigest(state, projectID, "create_sandbox", idempotencyKey, inputDigest); err != nil {
				return err
			}
			existingOp = op
			return nil
		}
		active := 0
		for _, existing := range state.Sandboxes {
			if existing.ProjectID == projectID && !terminal(existing.State) {
				active++
			}
		}
		if active >= s.limits.MaxActiveSandboxesPerProject {
			return ErrQuota
		}
		var ok bool
		if in.CheckpointID != "" {
			sourceCheckpoint, ok = state.Checkpoints[store.ScopedKey(projectID, in.CheckpointID)]
			if !ok {
				return store.ErrNotFound
			}
			if sourceCheckpoint.Kind != domain.CheckpointFilesystem || sourceCheckpoint.BackendRef == "" {
				return fmt.Errorf("%w: checkpoint cannot create a new sandbox", ErrDenied)
			}
			in.EnvironmentRevision = sourceCheckpoint.EnvironmentRevision
		}
		env, ok = state.Environments[store.ScopedKey(projectID, in.EnvironmentRevision)]
		if !ok {
			return store.ErrNotFound
		}
		for _, revision := range in.ConnectorRevisions {
			if _, ok := state.Connectors[store.ScopedKey(projectID, revision)]; !ok {
				return store.ErrNotFound
			}
		}
		for _, mount := range in.WorkspaceMounts {
			workspace, ok := state.Workspaces[store.ScopedKey(projectID, mount.WorkspaceID)]
			if !ok {
				return store.ErrNotFound
			}
			if workspace.State != domain.WorkspaceReady || workspace.BackendName == "" {
				return fmt.Errorf("%w: workspace %s is not ready", ErrConflict, mount.WorkspaceID)
			}
			for _, existing := range state.Sandboxes {
				if existing.ProjectID != projectID || terminal(existing.State) {
					continue
				}
				for _, attached := range existing.WorkspaceMounts {
					if attached.WorkspaceID == mount.WorkspaceID {
						return fmt.Errorf("%w: workspace %s is already attached", ErrConflict, mount.WorkspaceID)
					}
				}
			}
			workspaceMounts = append(workspaceMounts, backend.WorkspaceMount{Name: workspace.BackendName, Path: mount.Path})
		}
		return nil
	})
	if err != nil {
		return domain.Sandbox{}, domain.Operation{}, translateStore(err)
	}
	if existingOp.ID != "" {
		sandbox, err := s.getSandbox(projectID, existingOp.ResourceID)
		return sandbox, existingOp, err
	}
	if env.Backend != s.backend.Name() {
		return domain.Sandbox{}, domain.Operation{}, fmt.Errorf("%w: environment backend does not match configured backend", ErrInvalid)
	}
	if sourceCheckpoint.ID != "" && sourceCheckpoint.Backend != env.Backend {
		return domain.Sandbox{}, domain.Operation{}, fmt.Errorf("%w: checkpoint backend does not match environment", ErrInvalid)
	}
	if len(in.WorkspaceMounts) > 0 && !s.Capabilities().DurableWorkspaces {
		return domain.Sandbox{}, domain.Operation{}, fmt.Errorf("%w: backend cannot attach durable workspaces", ErrDenied)
	}
	if err := backend.Preflight(s.Capabilities(), in.Lifecycle, in.Network, in.ConnectorRevisions); err != nil {
		return domain.Sandbox{}, domain.Operation{}, fmt.Errorf("%w: %v", ErrDenied, err)
	}

	now := s.now()
	sandboxID, err := randomID("sbx")
	if err != nil {
		return domain.Sandbox{}, domain.Operation{}, err
	}
	opID, err := randomID("op")
	if err != nil {
		return domain.Sandbox{}, domain.Operation{}, err
	}
	sandbox := domain.Sandbox{
		ID: sandboxID, ProjectID: projectID, EnvironmentRevision: env.RevisionID, Backend: env.Backend,
		SourceCheckpointID: sourceCheckpoint.ID,
		State:              domain.SandboxPreparing, Lifecycle: in.Lifecycle, Network: in.Network,
		ConnectorRevisions: append([]string(nil), in.ConnectorRevisions...),
		WorkspaceMounts:    append([]domain.WorkspaceMount(nil), in.WorkspaceMounts...), CreatedAt: now, UpdatedAt: now,
		ExpiresAt: now.Add(time.Duration(in.Lifecycle.ExpiresAfterSeconds) * time.Second), Revision: 1,
	}
	markSandboxActive(&sandbox, now)
	op := domain.Operation{ID: opID, ProjectID: projectID, Kind: "create_sandbox", ResourceID: sandboxID, State: domain.OperationRunning, IdempotencyKey: idempotencyKey, CreatedAt: now, UpdatedAt: now}
	backendEnvironment := map[string]string{}
	if len(sandbox.ConnectorRevisions) > 0 {
		lease, leaseErr := s.broker.Issue(projectID, sandbox.ID, sandbox.ConnectorRevisions)
		if leaseErr != nil {
			return domain.Sandbox{}, domain.Operation{}, fmt.Errorf("%w: issue connector lease", ErrDenied)
		}
		backendEnvironment["BREZEL_CONNECTOR_GATEWAY_URL"] = s.broker.GatewayURL() + "/proxy"
		backendEnvironment["BREZEL_CONNECTOR_RENEW_URL"] = s.broker.GatewayURL() + "/leases/renew"
		backendEnvironment["BREZEL_CONNECTOR_LEASE"] = lease
	}
	persistIntentStarted := time.Now()
	err = s.store.Update(func(state *store.State) error {
		if existing, ok := idempotentOperation(*state, projectID, "create_sandbox", idempotencyKey); ok {
			if err := requireIdempotencyDigest(*state, projectID, "create_sandbox", idempotencyKey, inputDigest); err != nil {
				return err
			}
			op = existing
			sandbox = state.Sandboxes[store.ScopedKey(projectID, existing.ResourceID)]
			return nil
		}
		state.Sandboxes[store.ScopedKey(projectID, sandboxID)] = sandbox
		state.Operations[store.ScopedKey(projectID, opID)] = op
		state.Idempotency[store.IdempotencyKey(projectID, "create_sandbox", idempotencyKey)] = opID
		state.IdempotencyDigests[store.IdempotencyKey(projectID, "create_sandbox", idempotencyKey)] = inputDigest
		appendEvent(state, eventFor(sandbox, opID, "sandbox.preparing", now, nil))
		return nil
	})
	telemetry.Observe(s.observer, telemetry.OperationSandboxCreate, telemetry.PhasePersistIntent, persistIntentStarted, err)
	if err != nil {
		return sandbox, op, err
	}
	if op.ResourceID != sandboxID {
		return sandbox, op, nil
	}

	templateID := env.BackendTemplate
	if sourceCheckpoint.ID != "" {
		templateID = sourceCheckpoint.BackendRef
	}
	mutationKey := store.ScopedKey(projectID, sandbox.ID)
	s.activeBackendMutations[mutationKey] = struct{}{}
	// Admission, quota, idempotency, and workspace attachment are committed
	// before provisioning starts. Provisioning itself is independent per
	// sandbox and can take hundreds of milliseconds, so keeping the service's
	// global state lock here would serialize an otherwise safe host-level burst.
	s.mu.Unlock()
	locked = false
	backendStarted := time.Now()
	remote, backendErr := s.backend.Create(ctx, backend.CreateRequest{
		LocalSandboxID: sandbox.ID, ProjectID: projectID, TemplateID: templateID,
		Lifecycle: sandbox.Lifecycle, Network: sandbox.Network, Environment: backendEnvironment, WorkspaceMounts: workspaceMounts,
	})
	telemetry.Observe(s.observer, telemetry.OperationSandboxCreate, telemetry.PhaseBackendCall, backendStarted, backendErr)
	s.mu.Lock()
	locked = true
	if backendErr != nil {
		delete(s.activeBackendMutations, mutationKey)
		failure := &domain.Failure{Code: "backend_create_unconfirmed", Message: "sandbox backend did not confirm whether the resource was created", Retryable: true}
		now = s.now()
		sandbox.State, sandbox.Failure, sandbox.UpdatedAt, sandbox.Revision = domain.SandboxUnknown, failure, now, sandbox.Revision+1
		op.State, op.Failure, op.UpdatedAt = domain.OperationFailed, failure, now
		persistResultStarted := time.Now()
		persistErr := s.store.Update(func(state *store.State) error {
			state.Sandboxes[store.ScopedKey(projectID, sandbox.ID)] = sandbox
			state.Operations[store.ScopedKey(projectID, op.ID)] = op
			appendEvent(state, eventFor(sandbox, op.ID, "sandbox.unknown", now, map[string]any{"code": failure.Code}))
			return nil
		})
		telemetry.Observe(s.observer, telemetry.OperationSandboxCreate, telemetry.PhasePersistResult, persistResultStarted, persistErr)
		return sandbox, op, fmt.Errorf("%w: create sandbox", ErrBackend)
	}
	now = s.now()
	sandbox.BackendID, sandbox.State, sandbox.UpdatedAt, sandbox.Revision = remote.ID, remote.State, now, sandbox.Revision+1
	sandbox.Failure = nil
	markSandboxActive(&sandbox, now)
	// Route attachment is part of provisioning and remains fenced as a backend
	// mutation, but it must not hold the service-wide admission lock during mTLS
	// network I/O.
	s.mu.Unlock()
	locked = false
	routeStarted := time.Now()
	sandbox, routeErr := s.attachNodeRoute(ctx, sandbox)
	s.mu.Lock()
	locked = true
	delete(s.activeBackendMutations, mutationKey)
	if routeErr != nil {
		failure := &domain.Failure{Code: "node_route_attach_unconfirmed", Message: "execution node did not confirm the sandbox route", Retryable: true}
		now = s.now()
		sandbox.State, sandbox.Failure, sandbox.UpdatedAt, sandbox.Revision = domain.SandboxUnknown, failure, now, sandbox.Revision+1
		op.State, op.Failure, op.UpdatedAt = domain.OperationFailed, failure, now
		persistResultStarted := time.Now()
		persistErr := s.store.Update(func(state *store.State) error {
			state.Sandboxes[store.ScopedKey(projectID, sandbox.ID)] = sandbox
			state.Operations[store.ScopedKey(projectID, op.ID)] = op
			appendEvent(state, eventFor(sandbox, op.ID, "sandbox.unknown", now, map[string]any{"code": failure.Code}))
			return nil
		})
		telemetry.Observe(s.observer, telemetry.OperationSandboxCreate, telemetry.PhaseBackendCall, routeStarted, routeErr)
		telemetry.Observe(s.observer, telemetry.OperationSandboxCreate, telemetry.PhasePersistResult, persistResultStarted, persistErr)
		return sandbox, op, fmt.Errorf("%w: attach sandbox node route: %v", ErrBackend, routeErr)
	}
	now = s.now()
	sandbox.UpdatedAt = now
	op.State, op.UpdatedAt = domain.OperationSucceeded, now
	persistResultStarted := time.Now()
	err = s.store.Update(func(state *store.State) error {
		state.Sandboxes[store.ScopedKey(projectID, sandbox.ID)] = sandbox
		state.Operations[store.ScopedKey(projectID, op.ID)] = op
		appendEvent(state, eventFor(sandbox, op.ID, "sandbox.running", now, nil))
		return nil
	})
	telemetry.Observe(s.observer, telemetry.OperationSandboxCreate, telemetry.PhasePersistResult, persistResultStarted, err)
	return sandbox, op, err
}

func (s *Service) GetEnvironment(projectID, revision string) (domain.Environment, error) {
	if err := domain.ValidateProjectID(projectID); err != nil {
		return domain.Environment{}, ErrInvalid
	}
	var out domain.Environment
	err := s.store.View(func(state store.State) error {
		var ok bool
		out, ok = state.Environments[store.ScopedKey(projectID, revision)]
		if !ok {
			return store.ErrNotFound
		}
		return nil
	})
	return out, translateStore(err)
}

func (s *Service) GetConnector(projectID, revision string) (domain.Connector, error) {
	var out domain.Connector
	err := s.store.View(func(state store.State) error {
		var ok bool
		out, ok = state.Connectors[store.ScopedKey(projectID, revision)]
		if !ok {
			return store.ErrNotFound
		}
		return nil
	})
	return out, translateStore(err)
}

func (s *Service) GetSandbox(ctx context.Context, projectID, id string) (domain.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sandbox, err := s.getSandbox(projectID, id)
	if err != nil {
		return sandbox, err
	}
	if _, active := s.activeBackendMutations[store.ScopedKey(projectID, id)]; active {
		// Return the already-durable transitional view while this controller is
		// waiting for the backend. Inspecting here could overwrite that fence
		// with a stale pre-transition observation.
		return sandbox, nil
	}
	if terminal(sandbox.State) {
		return sandbox, nil
	}
	if sandbox.CleanupTarget == domain.SandboxDeleted || sandbox.CleanupTarget == domain.SandboxExpired {
		return s.reconcileCleanupLocked(ctx, sandbox)
	}
	if !s.now().Before(sandbox.ExpiresAt) {
		s.cancelActiveGuestOperationsLocked(projectID, id)
		return s.expireLocked(ctx, sandbox)
	}
	if sandbox.BackendID == "" {
		return sandbox, nil
	}
	if s.hasActiveGuestOperationsLocked(projectID, id) {
		// A guest-operation binding is fenced to this durable revision. Backend
		// observation waits until release so a read cannot invalidate a live
		// command, transfer, or preview stream mid-operation.
		return sandbox, nil
	}
	remote, err := s.backend.Inspect(ctx, sandbox.BackendID)
	if err != nil {
		if errors.Is(err, backend.ErrNotFound) {
			return s.markMissingLocked(sandbox)
		}
		return s.markUnknownLocked(sandbox, "backend_inspection_failed")
	}
	if remote.State != sandbox.State {
		now := s.now()
		sandbox.State, sandbox.UpdatedAt, sandbox.Revision, sandbox.Failure = remote.State, now, sandbox.Revision+1, nil
		_ = s.store.Update(func(state *store.State) error {
			state.Sandboxes[store.ScopedKey(projectID, id)] = sandbox
			appendEvent(state, eventFor(sandbox, "", "sandbox.observed", now, nil))
			return nil
		})
	}
	return sandbox, nil
}

// ListSandboxes returns the project's durable lifecycle view. Reconciliation is
// intentionally separate so listing cannot fan out into unbounded engine calls.
func (s *Service) ListSandboxes(projectID string, includeTerminal bool) ([]domain.Sandbox, error) {
	if err := domain.ValidateProjectID(projectID); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var out []domain.Sandbox
	err := s.store.View(func(state store.State) error {
		for _, sandbox := range state.Sandboxes {
			if sandbox.ProjectID != projectID || (!includeTerminal && terminal(sandbox.State)) {
				continue
			}
			out = append(out, sandbox)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (s *Service) GetOperation(projectID, id string) (domain.Operation, error) {
	var out domain.Operation
	err := s.store.View(func(state store.State) error {
		var ok bool
		out, ok = state.Operations[store.ScopedKey(projectID, id)]
		if !ok {
			return store.ErrNotFound
		}
		return nil
	})
	return out, translateStore(err)
}

func (s *Service) Events(projectID, sandboxID string) ([]domain.Event, error) {
	if _, err := s.getSandbox(projectID, sandboxID); err != nil {
		return nil, err
	}
	var out []domain.Event
	err := s.store.View(func(state store.State) error {
		for _, event := range state.Events {
			if event.ProjectID == projectID && event.ResourceID == sandboxID {
				out = append(out, event)
			}
		}
		return nil
	})
	return out, err
}

func (s *Service) Pause(ctx context.Context, projectID, sandboxID, idempotencyKey string) (domain.Sandbox, domain.Operation, error) {
	return s.lifecycleAction(ctx, projectID, sandboxID, idempotencyKey, "pause_sandbox", domain.SandboxRunning, domain.SandboxPausing, domain.SandboxStandby, func(sandbox domain.Sandbox) (domain.Sandbox, error) {
		updated, err := s.prepareNodeRoutePause(ctx, sandbox)
		if err != nil {
			return updated, err
		}
		if err := s.backend.Pause(ctx, updated.BackendID, updated.Lifecycle.StandbyCheckpoint); err != nil {
			return updated, err
		}
		return s.completeNodeRouteStandby(ctx, updated)
	})
}

func (s *Service) Resume(ctx context.Context, projectID, sandboxID, idempotencyKey string) (domain.Sandbox, domain.Operation, error) {
	return s.lifecycleAction(ctx, projectID, sandboxID, idempotencyKey, "resume_sandbox", domain.SandboxStandby, domain.SandboxResuming, domain.SandboxRunning, func(sandbox domain.Sandbox) (domain.Sandbox, error) {
		remaining := int64(sandbox.ExpiresAt.Sub(s.now()).Seconds())
		if remaining < 1 {
			return sandbox, fmt.Errorf("%w: sandbox expired", ErrConflict)
		}
		remote, err := s.backend.Resume(ctx, sandbox.BackendID, sandbox.Lifecycle.StandbyCheckpoint, remaining)
		if err != nil {
			return sandbox, err
		}
		sandbox.BackendID = remote.ID
		return s.resumeNodeRoute(ctx, sandbox)
	})
}

func (s *Service) Delete(ctx context.Context, projectID, sandboxID, idempotencyKey string) (domain.Sandbox, domain.Operation, error) {
	s.mu.Lock()
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()
	if err := requireMutation(projectID, idempotencyKey); err != nil {
		return domain.Sandbox{}, domain.Operation{}, err
	}
	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return sandbox, domain.Operation{}, err
	}
	kind := "delete_sandbox:" + sandboxID
	if existing, ok := s.lookupIdempotency(projectID, kind, idempotencyKey); ok {
		return sandbox, existing, nil
	}
	if _, active := s.activeBackendMutations[store.ScopedKey(projectID, sandboxID)]; active {
		return sandbox, domain.Operation{}, fmt.Errorf("%w: sandbox has an active backend mutation", ErrConflict)
	}
	if sandbox.State == domain.SandboxDeleted || sandbox.State == domain.SandboxExpired {
		return sandbox, domain.Operation{}, fmt.Errorf("%w: sandbox is already terminal", ErrConflict)
	}
	if s.hasActiveGuestOperationsLocked(projectID, sandboxID) {
		return sandbox, domain.Operation{}, fmt.Errorf("%w: sandbox has active guest operations", ErrConflict)
	}
	sandbox.CleanupTarget = domain.SandboxDeleted
	persistIntentStarted := time.Now()
	op, err := s.beginTransitionLocked(sandbox, idempotencyKey, kind, domain.SandboxDeleting)
	telemetry.Observe(s.observer, telemetry.OperationSandboxDelete, telemetry.PhasePersistIntent, persistIntentStarted, err)
	if err != nil {
		return sandbox, op, err
	}
	sandbox, _ = s.getSandbox(projectID, sandboxID)
	mutationKey := store.ScopedKey(projectID, sandboxID)
	s.activeBackendMutations[mutationKey] = struct{}{}
	s.mu.Unlock()
	locked = false
	backendStarted := time.Now()
	routedSandbox, backendErr := s.prepareNodeRoutePause(ctx, sandbox)
	if backendErr == nil {
		backendErr = s.backend.Delete(ctx, routedSandbox.BackendID)
	}
	if backendErr == nil || errors.Is(backendErr, backend.ErrNotFound) {
		routedSandbox, backendErr = s.removeNodeRoute(ctx, routedSandbox)
	}
	sandbox = routedSandbox
	telemetry.Observe(s.observer, telemetry.OperationSandboxDelete, telemetry.PhaseBackendCall, backendStarted, backendErr)
	s.mu.Lock()
	locked = true
	delete(s.activeBackendMutations, mutationKey)
	if backendErr != nil && !errors.Is(backendErr, backend.ErrNotFound) {
		persistResultStarted := time.Now()
		resultSandbox, resultOperation, resultErr := s.failTransitionLocked(sandbox, op, "backend_delete_failed")
		telemetry.Observe(s.observer, telemetry.OperationSandboxDelete, telemetry.PhasePersistResult, persistResultStarted, resultErr)
		return resultSandbox, resultOperation, resultErr
	}
	persistResultStarted := time.Now()
	resultSandbox, resultOperation, resultErr := s.completeTransitionLocked(sandbox, op, domain.SandboxDeleted)
	telemetry.Observe(s.observer, telemetry.OperationSandboxDelete, telemetry.PhasePersistResult, persistResultStarted, resultErr)
	return resultSandbox, resultOperation, resultErr
}

func (s *Service) Checkpoint(ctx context.Context, projectID, sandboxID, idempotencyKey, name string, kind domain.CheckpointKind) (domain.Checkpoint, domain.Operation, error) {
	s.mu.Lock()
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()
	if err := requireMutation(projectID, idempotencyKey); err != nil {
		return domain.Checkpoint{}, domain.Operation{}, err
	}
	if name == "" || strings.ContainsAny(name, "\r\n") {
		return domain.Checkpoint{}, domain.Operation{}, fmt.Errorf("%w: checkpoint name is required", ErrInvalid)
	}
	inputDigest, err := mutationDigest(struct {
		Name string                `json:"name"`
		Kind domain.CheckpointKind `json:"kind"`
	}{Name: name, Kind: kind})
	if err != nil {
		return domain.Checkpoint{}, domain.Operation{}, err
	}
	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return domain.Checkpoint{}, domain.Operation{}, err
	}
	if sandbox.State != domain.SandboxRunning {
		return domain.Checkpoint{}, domain.Operation{}, fmt.Errorf("%w: sandbox must be running", ErrConflict)
	}
	if s.hasActiveGuestOperationsLocked(projectID, sandboxID) {
		return domain.Checkpoint{}, domain.Operation{}, fmt.Errorf("%w: sandbox has active guest operations", ErrConflict)
	}
	capabilities := s.backend.Capabilities()
	if (kind == domain.CheckpointFilesystem && !capabilities.FilesystemCheckpoint) || (kind == domain.CheckpointFullState && !capabilities.FullStateCheckpoint) {
		return domain.Checkpoint{}, domain.Operation{}, fmt.Errorf("%w: requested checkpoint kind is not enforced by backend", ErrDenied)
	}
	kindKey := "checkpoint_sandbox:" + sandboxID
	if existing, ok, lookupErr := s.lookupIdempotencyInput(projectID, kindKey, idempotencyKey, inputDigest); lookupErr != nil {
		return domain.Checkpoint{}, domain.Operation{}, lookupErr
	} else if ok {
		var checkpoint domain.Checkpoint
		_ = s.store.View(func(state store.State) error {
			checkpoint = state.Checkpoints[store.ScopedKey(projectID, existing.ResourceID)]
			return nil
		})
		return checkpoint, existing, nil
	}
	mutationKey := store.ScopedKey(projectID, sandboxID)
	if _, active := s.activeBackendMutations[mutationKey]; active {
		return domain.Checkpoint{}, domain.Operation{}, fmt.Errorf("%w: sandbox has an active backend mutation", ErrConflict)
	}
	opID, _ := randomID("op")
	now := s.now()
	op := domain.Operation{ID: opID, ProjectID: projectID, Kind: kindKey, ResourceID: sandboxID, State: domain.OperationRunning, IdempotencyKey: idempotencyKey, CreatedAt: now, UpdatedAt: now}
	persistIntentStarted := time.Now()
	if err := s.store.Update(func(state *store.State) error {
		state.Operations[store.ScopedKey(projectID, op.ID)] = op
		idempotencyIndex := store.IdempotencyKey(projectID, kindKey, idempotencyKey)
		state.Idempotency[idempotencyIndex] = op.ID
		state.IdempotencyDigests[idempotencyIndex] = inputDigest
		return nil
	}); err != nil {
		telemetry.Observe(s.observer, telemetry.OperationSandboxCheckpoint, telemetry.PhasePersistIntent, persistIntentStarted, err)
		return domain.Checkpoint{}, op, err
	}
	telemetry.Observe(s.observer, telemetry.OperationSandboxCheckpoint, telemetry.PhasePersistIntent, persistIntentStarted, nil)
	s.activeBackendMutations[mutationKey] = struct{}{}
	s.mu.Unlock()
	locked = false
	backendStarted := time.Now()
	remote, err := s.backend.Checkpoint(ctx, sandbox.BackendID, kind, name)
	telemetry.Observe(s.observer, telemetry.OperationSandboxCheckpoint, telemetry.PhaseBackendCall, backendStarted, err)
	s.mu.Lock()
	locked = true
	delete(s.activeBackendMutations, mutationKey)
	if err != nil {
		failure := &domain.Failure{Code: "backend_checkpoint_failed", Message: "sandbox backend could not create the checkpoint", Retryable: true}
		op.State, op.Failure, op.UpdatedAt = domain.OperationFailed, failure, s.now()
		persistResultStarted := time.Now()
		persistErr := s.store.Update(func(state *store.State) error { state.Operations[store.ScopedKey(projectID, op.ID)] = op; return nil })
		telemetry.Observe(s.observer, telemetry.OperationSandboxCheckpoint, telemetry.PhasePersistResult, persistResultStarted, persistErr)
		return domain.Checkpoint{}, op, fmt.Errorf("%w: create checkpoint", ErrBackend)
	}
	checkpointID, _ := randomID("chk")
	checkpoint := domain.Checkpoint{ID: checkpointID, ProjectID: projectID, SourceSandboxID: sandboxID, EnvironmentRevision: sandbox.EnvironmentRevision, Backend: sandbox.Backend, BackendRef: remote.Ref, Kind: remote.Kind, CreatedAt: s.now()}
	op.ResourceID, op.State, op.UpdatedAt = checkpointID, domain.OperationSucceeded, s.now()
	persistResultStarted := time.Now()
	persistErr := s.store.Update(func(state *store.State) error {
		state.Checkpoints[store.ScopedKey(projectID, checkpointID)] = checkpoint
		state.Operations[store.ScopedKey(projectID, op.ID)] = op
		appendEvent(state, eventFor(sandbox, op.ID, "checkpoint.created", op.UpdatedAt, map[string]any{"checkpoint_id": checkpointID, "kind": kind}))
		return nil
	})
	telemetry.Observe(s.observer, telemetry.OperationSandboxCheckpoint, telemetry.PhasePersistResult, persistResultStarted, persistErr)
	return checkpoint, op, persistErr
}

func (s *Service) DeleteCheckpoint(ctx context.Context, projectID, checkpointID, idempotencyKey string) (domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := requireMutation(projectID, idempotencyKey); err != nil {
		return domain.Operation{}, err
	}
	kind := "delete_checkpoint:" + checkpointID
	if existing, ok := s.lookupIdempotency(projectID, kind, idempotencyKey); ok {
		return existing, nil
	}
	checkpoint, err := s.getCheckpoint(projectID, checkpointID)
	if err != nil {
		return domain.Operation{}, err
	}
	if s.checkpointInUseLocked(projectID, checkpointID, checkpoint.SourceSandboxID) {
		return domain.Operation{}, fmt.Errorf("%w: checkpoint is the source of an active sandbox", ErrConflict)
	}

	now := s.now()
	opID, err := randomID("op")
	if err != nil {
		return domain.Operation{}, err
	}
	op := domain.Operation{
		ID: opID, ProjectID: projectID, Kind: kind, ResourceID: checkpointID,
		State: domain.OperationRunning, IdempotencyKey: idempotencyKey, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.Update(func(state *store.State) error {
		state.Operations[store.ScopedKey(projectID, op.ID)] = op
		state.Idempotency[store.IdempotencyKey(projectID, kind, idempotencyKey)] = op.ID
		return nil
	}); err != nil {
		return op, err
	}

	if err := s.backend.DeleteCheckpoint(ctx, checkpoint.BackendRef); err != nil && !errors.Is(err, backend.ErrNotFound) {
		failure := &domain.Failure{Code: "backend_checkpoint_delete_unconfirmed", Message: "sandbox engine did not confirm checkpoint cleanup", Retryable: true}
		op.State, op.Failure, op.UpdatedAt = domain.OperationFailed, failure, s.now()
		_ = s.store.Update(func(state *store.State) error {
			state.Operations[store.ScopedKey(projectID, op.ID)] = op
			return nil
		})
		return op, fmt.Errorf("%w: delete checkpoint", ErrBackend)
	}

	op.State, op.UpdatedAt = domain.OperationSucceeded, s.now()
	err = s.store.Update(func(state *store.State) error {
		delete(state.Checkpoints, store.ScopedKey(projectID, checkpointID))
		state.Operations[store.ScopedKey(projectID, op.ID)] = op
		if sandbox, ok := state.Sandboxes[store.ScopedKey(projectID, checkpoint.SourceSandboxID)]; ok {
			appendEvent(state, eventFor(sandbox, op.ID, "checkpoint.deleted", op.UpdatedAt, map[string]any{"checkpoint_id": checkpointID}))
		}
		return nil
	})
	return op, err
}

func (s *Service) getCheckpoint(projectID, checkpointID string) (domain.Checkpoint, error) {
	var checkpoint domain.Checkpoint
	err := s.store.View(func(state store.State) error {
		var ok bool
		checkpoint, ok = state.Checkpoints[store.ScopedKey(projectID, checkpointID)]
		if !ok {
			return store.ErrNotFound
		}
		return nil
	})
	return checkpoint, translateStore(err)
}

func (s *Service) checkpointInUseLocked(projectID, checkpointID, sourceSandboxID string) bool {
	inUse := false
	_ = s.store.View(func(state store.State) error {
		for _, sandbox := range state.Sandboxes {
			if sandbox.ProjectID == projectID && !terminal(sandbox.State) && (sandbox.ID == sourceSandboxID || sandbox.SourceCheckpointID == checkpointID) {
				inUse = true
				break
			}
		}
		return nil
	})
	return inUse
}

func (s *Service) Receipt(projectID, sandboxID string) (receipt.Envelope, error) {
	if s.signer == nil {
		return receipt.Envelope{}, fmt.Errorf("%w: receipt signer is not configured", ErrDenied)
	}
	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return receipt.Envelope{}, err
	}
	env, err := s.GetEnvironment(projectID, sandbox.EnvironmentRevision)
	if err != nil {
		return receipt.Envelope{}, err
	}
	events, err := s.Events(projectID, sandboxID)
	if err != nil {
		return receipt.Envelope{}, err
	}
	eventsDigest, _ := receipt.DigestJSON(events)
	networkDigest, _ := receipt.DigestJSON(sandbox.Network)
	workspaceMountsDigest := ""
	if len(sandbox.WorkspaceMounts) > 0 {
		workspaceMountsDigest, _ = receipt.DigestJSON(sandbox.WorkspaceMounts)
	}
	sandboxDigest, _ := receipt.DigestJSON(sandbox)
	backendDigest := ""
	if sandbox.BackendID != "" {
		backendDigest = receipt.DigestString(sandbox.BackendID)
	}
	statement := receipt.Statement{
		Type:          "https://in-toto.io/Statement/v1",
		Subject:       []receipt.Subject{{Name: "sandbox:" + sandbox.ID, Digest: map[string]string{"sha256": strings.TrimPrefix(sandboxDigest, "sha256:")}}},
		PredicateType: "https://runtime.dev/attestations/sandbox-lifecycle/v1",
		Predicate: receipt.Predicate{
			ProjectID: projectID, SandboxID: sandbox.ID, SandboxRevision: sandbox.Revision, State: sandbox.State,
			EnvironmentRevision: env.RevisionID, SourceCheckpointID: sandbox.SourceCheckpointID,
			ImageDigest: env.ImageDigest, PolicyRevision: env.PolicyRevision,
			Backend: sandbox.Backend, BackendIDDigest: backendDigest, Lifecycle: sandbox.Lifecycle,
			NetworkPolicyDigest: networkDigest, WorkspaceMountsDigest: workspaceMountsDigest, EventLogDigest: eventsDigest,
			InputIdentity: "none:lifecycle-receipt", OutputIdentity: "none:lifecycle-receipt",
			CleanupIdentity: fmt.Sprintf("state:%s@revision:%d", sandbox.State, sandbox.Revision),
			Assurance:       "signed control-plane claim; not hardware attestation",
		},
	}
	return s.signer.Sign(statement)
}

func (s *Service) Reconcile(ctx context.Context) error {
	if err := s.reconcileBackendState(ctx); err != nil {
		return err
	}
	return s.reconcileAutoStandby(ctx)
}

func (s *Service) reconcileBackendState(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reconcileWorkspacesLocked(ctx); err != nil {
		return err
	}
	var sandboxes []domain.Sandbox
	if err := s.store.View(func(state store.State) error {
		for _, sandbox := range state.Sandboxes {
			if !terminal(sandbox.State) {
				sandboxes = append(sandboxes, sandbox)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, sandbox := range sandboxes {
		if _, active := s.activeBackendMutations[store.ScopedKey(sandbox.ProjectID, sandbox.ID)]; active {
			// This process has already durably admitted the create and is waiting
			// for its backend result. A controller restart clears this in-memory
			// guard, allowing the normal Find-based recovery path to take over.
			continue
		}
		if sandbox.CleanupTarget == domain.SandboxDeleted || sandbox.CleanupTarget == domain.SandboxExpired {
			_, _ = s.reconcileCleanupLocked(ctx, sandbox)
			continue
		}
		if !s.now().Before(sandbox.ExpiresAt) {
			s.cancelActiveGuestOperationsLocked(sandbox.ProjectID, sandbox.ID)
			_, _ = s.expireLocked(ctx, sandbox)
			continue
		}
		if s.hasActiveGuestOperationsLocked(sandbox.ProjectID, sandbox.ID) {
			// Inspect can advance the durable revision. A node data-plane binding
			// admitted for the current revision must remain valid for the whole
			// active stream, so ordinary observation waits for its lease to end.
			continue
		}
		var remote backend.Sandbox
		var err error
		if sandbox.BackendID == "" {
			remote, err = s.backend.Find(ctx, sandbox.ID, sandbox.ProjectID)
		} else {
			remote, err = s.backend.Inspect(ctx, sandbox.BackendID)
		}
		if err != nil {
			if errors.Is(err, backend.ErrNotFound) {
				_, _ = s.markMissingLocked(sandbox)
			} else {
				_, _ = s.markUnknownLocked(sandbox, "backend_reconcile_failed")
			}
			continue
		}
		now := s.now()
		sandbox.BackendID = remote.ID
		routed, routeErr := s.reconcileNodeRoute(ctx, sandbox, remote.State)
		sandbox = routed
		if routeErr != nil {
			_, _ = s.markUnknownLocked(sandbox, "node_route_reconcile_failed")
			continue
		}
		sandbox.State, sandbox.UpdatedAt, sandbox.Revision, sandbox.Failure = remote.State, now, sandbox.Revision+1, nil
		_ = s.store.Update(func(state *store.State) error {
			state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
			switch sandbox.State {
			case domain.SandboxRunning:
				completeLatestOperation(state, sandbox.ProjectID, sandbox.ID, "create_sandbox", now)
				completeLatestOperation(state, sandbox.ProjectID, sandbox.ID, "resume_sandbox:", now)
			case domain.SandboxStandby:
				completeLatestOperation(state, sandbox.ProjectID, sandbox.ID, "pause_sandbox:", now)
				completeLatestOperation(state, sandbox.ProjectID, sandbox.ID, "auto_pause_sandbox:", now)
			}
			appendEvent(state, eventFor(sandbox, "", "sandbox.reconciled", now, nil))
			return nil
		})
	}
	return nil
}

// reconcileAutoStandby applies the product-owned inactivity policy after
// backend state has been reconciled. Eligibility is rechecked under the same
// lock that admits guest operations and lifecycle mutations. The durable
// pausing state and in-memory backend-mutation fence are established before the
// service releases that lock and asks the engine to pause.
func (s *Service) reconcileAutoStandby(ctx context.Context) error {
	var candidates []struct{ projectID, sandboxID string }
	if err := s.store.View(func(state store.State) error {
		for _, sandbox := range state.Sandboxes {
			if sandbox.State == domain.SandboxRunning && sandbox.Lifecycle.StandbyAfterSeconds > 0 {
				candidates = append(candidates, struct{ projectID, sandboxID string }{sandbox.ProjectID, sandbox.ID})
			}
		}
		return nil
	}); err != nil {
		return err
	}
	var reconcileErr error
	for _, candidate := range candidates {
		if err := s.pauseIfIdle(ctx, candidate.projectID, candidate.sandboxID); err != nil {
			reconcileErr = errors.Join(reconcileErr, err)
		}
	}
	return reconcileErr
}

func (s *Service) pauseIfIdle(ctx context.Context, projectID, sandboxID string) error {
	s.mu.Lock()
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()
	key := store.ScopedKey(projectID, sandboxID)
	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return err
	}
	if sandbox.State != domain.SandboxRunning || sandbox.Lifecycle.StandbyAfterSeconds == 0 {
		return nil
	}
	if _, active := s.activeBackendMutations[key]; active || s.hasActiveGuestOperationsLocked(projectID, sandboxID) {
		return nil
	}
	lastActive := sandbox.LastActiveAt
	if recent := s.recentActivity[key]; recent.After(lastActive) {
		lastActive = recent
	}
	if lastActive.IsZero() {
		// State created by an older release has no trustworthy activity
		// baseline. Establish one instead of pausing it immediately on upgrade.
		_, err := s.recordActivityLocked(projectID, sandboxID, s.now())
		return err
	}
	eligibleAt := standbyEligibleAt(sandbox.Lifecycle, lastActive)
	if s.now().Before(eligibleAt) {
		return nil
	}
	idempotencyKey := fmt.Sprintf("automatic-standby:%d:%d", lastActive.UnixNano(), sandbox.Revision)
	kind := "auto_pause_sandbox:" + sandboxID
	op, err := s.beginTransitionLocked(sandbox, idempotencyKey, kind, domain.SandboxPausing)
	if err != nil {
		return err
	}
	sandbox, err = s.getSandbox(projectID, sandboxID)
	if err != nil {
		return err
	}
	s.activeBackendMutations[key] = struct{}{}
	s.mu.Unlock()
	locked = false
	sandbox, backendErr := s.prepareNodeRoutePause(ctx, sandbox)
	if backendErr == nil {
		backendErr = s.backend.Pause(ctx, sandbox.BackendID, sandbox.Lifecycle.StandbyCheckpoint)
	}
	if backendErr == nil {
		sandbox, backendErr = s.completeNodeRouteStandby(ctx, sandbox)
	}
	s.mu.Lock()
	locked = true
	delete(s.activeBackendMutations, key)
	if backendErr != nil {
		_, _, transitionErr := s.failTransitionLocked(sandbox, op, "backend_auto_pause_sandbox_failed")
		return transitionErr
	}
	_, _, err = s.completeTransitionLocked(sandbox, op, domain.SandboxStandby)
	return err
}

func (s *Service) lifecycleAction(ctx context.Context, projectID, sandboxID, idempotencyKey, kind string, from, transitional, target domain.SandboxState, call func(domain.Sandbox) (domain.Sandbox, error)) (domain.Sandbox, domain.Operation, error) {
	operation := lifecycleTelemetryOperation(kind)
	s.mu.Lock()
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()
	if err := requireMutation(projectID, idempotencyKey); err != nil {
		return domain.Sandbox{}, domain.Operation{}, err
	}
	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return sandbox, domain.Operation{}, err
	}
	idempotencyKind := kind + ":" + sandboxID
	if existing, ok := s.lookupIdempotency(projectID, idempotencyKind, idempotencyKey); ok {
		return sandbox, existing, nil
	}
	if _, active := s.activeBackendMutations[store.ScopedKey(projectID, sandboxID)]; active {
		return sandbox, domain.Operation{}, fmt.Errorf("%w: sandbox has an active backend mutation", ErrConflict)
	}
	if sandbox.State != from {
		return sandbox, domain.Operation{}, fmt.Errorf("%w: sandbox is %s, expected %s", ErrConflict, sandbox.State, from)
	}
	if s.hasActiveGuestOperationsLocked(projectID, sandboxID) {
		return sandbox, domain.Operation{}, fmt.Errorf("%w: sandbox has active guest operations", ErrConflict)
	}
	persistIntentStarted := time.Now()
	op, err := s.beginTransitionLocked(sandbox, idempotencyKey, idempotencyKind, transitional)
	telemetry.Observe(s.observer, operation, telemetry.PhasePersistIntent, persistIntentStarted, err)
	if err != nil {
		return sandbox, op, err
	}
	sandbox, _ = s.getSandbox(projectID, sandboxID)
	mutationKey := store.ScopedKey(projectID, sandboxID)
	s.activeBackendMutations[mutationKey] = struct{}{}
	s.mu.Unlock()
	locked = false
	backendStarted := time.Now()
	sandbox, backendErr := call(sandbox)
	telemetry.Observe(s.observer, operation, telemetry.PhaseBackendCall, backendStarted, backendErr)
	s.mu.Lock()
	locked = true
	delete(s.activeBackendMutations, mutationKey)
	if backendErr != nil {
		persistResultStarted := time.Now()
		resultSandbox, resultOperation, resultErr := s.failTransitionLocked(sandbox, op, "backend_"+kind+"_failed")
		telemetry.Observe(s.observer, operation, telemetry.PhasePersistResult, persistResultStarted, resultErr)
		return resultSandbox, resultOperation, resultErr
	}
	persistResultStarted := time.Now()
	resultSandbox, resultOperation, resultErr := s.completeTransitionLocked(sandbox, op, target)
	telemetry.Observe(s.observer, operation, telemetry.PhasePersistResult, persistResultStarted, resultErr)
	return resultSandbox, resultOperation, resultErr
}

func lifecycleTelemetryOperation(kind string) telemetry.Operation {
	switch kind {
	case "pause_sandbox":
		return telemetry.OperationSandboxPause
	case "resume_sandbox":
		return telemetry.OperationSandboxResume
	default:
		// lifecycleAction has no open-ended caller. An unknown operation is
		// intentionally dropped by the registry rather than becoming a label.
		return ""
	}
}

func (s *Service) beginTransitionLocked(sandbox domain.Sandbox, idempotencyKey, kind string, target domain.SandboxState) (domain.Operation, error) {
	if !domain.CanTransition(sandbox.State, target) {
		return domain.Operation{}, fmt.Errorf("%w: invalid lifecycle transition", ErrConflict)
	}
	now := s.now()
	opID, err := randomID("op")
	if err != nil {
		return domain.Operation{}, err
	}
	op := domain.Operation{ID: opID, ProjectID: sandbox.ProjectID, Kind: kind, ResourceID: sandbox.ID, State: domain.OperationRunning, IdempotencyKey: idempotencyKey, CreatedAt: now, UpdatedAt: now}
	sandbox.State, sandbox.UpdatedAt, sandbox.Revision, sandbox.Failure = target, now, sandbox.Revision+1, nil
	err = s.store.Update(func(state *store.State) error {
		state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		state.Operations[store.ScopedKey(sandbox.ProjectID, op.ID)] = op
		state.Idempotency[store.IdempotencyKey(sandbox.ProjectID, kind, idempotencyKey)] = op.ID
		appendEvent(state, eventFor(sandbox, op.ID, "sandbox."+string(target), now, nil))
		return nil
	})
	return op, err
}

func (s *Service) completeTransitionLocked(sandbox domain.Sandbox, op domain.Operation, target domain.SandboxState) (domain.Sandbox, domain.Operation, error) {
	if !domain.CanTransition(sandbox.State, target) {
		return sandbox, op, fmt.Errorf("%w: invalid lifecycle transition", ErrConflict)
	}
	now := s.now()
	sandbox.State, sandbox.UpdatedAt, sandbox.Revision, sandbox.Failure = target, now, sandbox.Revision+1, nil
	if target == domain.SandboxRunning {
		markSandboxActive(&sandbox, now)
		s.recentActivity[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = now
	}
	op.State, op.UpdatedAt, op.Failure = domain.OperationSucceeded, now, nil
	err := s.store.Update(func(state *store.State) error {
		state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		state.Operations[store.ScopedKey(sandbox.ProjectID, op.ID)] = op
		appendEvent(state, eventFor(sandbox, op.ID, "sandbox."+string(target), now, nil))
		return nil
	})
	return sandbox, op, err
}

func standbyEligibleAt(lifecycle domain.Lifecycle, lastActive time.Time) time.Time {
	return lastActive.Add(time.Duration(lifecycle.StandbyAfterSeconds+lifecycle.StandbyGraceSeconds) * time.Second)
}

func markSandboxActive(sandbox *domain.Sandbox, at time.Time) {
	sandbox.LastActiveAt = at
	if sandbox.Lifecycle.StandbyAfterSeconds > 0 {
		sandbox.StandbyEligibleAt = standbyEligibleAt(sandbox.Lifecycle, at)
	} else {
		sandbox.StandbyEligibleAt = time.Time{}
	}
}

// recordActivityLocked advances activity monotonically and updates only the
// current durable sandbox. It never writes a stale caller snapshot over a
// concurrent lifecycle transition.
func (s *Service) recordActivityLocked(projectID, sandboxID string, at time.Time) (domain.Sandbox, error) {
	key := store.ScopedKey(projectID, sandboxID)
	if recorder, ok := s.store.(store.SandboxActivityRecorder); ok {
		sandbox, err := recorder.RecordSandboxActivity(projectID, sandboxID, at)
		if err == nil {
			s.recentActivity[key] = sandbox.LastActiveAt
		}
		return sandbox, translateStore(err)
	}
	var sandbox domain.Sandbox
	err := s.store.Update(func(state *store.State) error {
		current, ok := state.Sandboxes[key]
		if !ok {
			return store.ErrNotFound
		}
		if current.State != domain.SandboxRunning && current.State != domain.SandboxStandby {
			sandbox = current
			return nil
		}
		if at.Before(current.LastActiveAt) {
			at = current.LastActiveAt
		}
		current.UpdatedAt = at
		current.Revision++
		markSandboxActive(&current, at)
		state.Sandboxes[key] = current
		sandbox = current
		return nil
	})
	if err == nil {
		s.recentActivity[key] = sandbox.LastActiveAt
	}
	return sandbox, translateStore(err)
}

func (s *Service) failTransitionLocked(sandbox domain.Sandbox, op domain.Operation, code string) (domain.Sandbox, domain.Operation, error) {
	now := s.now()
	failure := &domain.Failure{Code: code, Message: "sandbox backend did not confirm the requested transition", Retryable: true}
	sandbox.State, sandbox.UpdatedAt, sandbox.Revision, sandbox.Failure = domain.SandboxUnknown, now, sandbox.Revision+1, failure
	op.State, op.UpdatedAt, op.Failure = domain.OperationFailed, now, failure
	_ = s.store.Update(func(state *store.State) error {
		state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		state.Operations[store.ScopedKey(sandbox.ProjectID, op.ID)] = op
		appendEvent(state, eventFor(sandbox, op.ID, "sandbox.unknown", now, map[string]any{"code": code}))
		return nil
	})
	return sandbox, op, fmt.Errorf("%w: lifecycle transition", ErrBackend)
}

func (s *Service) expireLocked(ctx context.Context, sandbox domain.Sandbox) (domain.Sandbox, error) {
	sandbox.CleanupTarget = domain.SandboxExpired
	if sandbox.State != domain.SandboxDeleting {
		if !domain.CanTransition(sandbox.State, domain.SandboxDeleting) {
			return sandbox, fmt.Errorf("%w: cannot expire sandbox from %s", ErrConflict, sandbox.State)
		}
		now := s.now()
		sandbox.State, sandbox.UpdatedAt, sandbox.Revision = domain.SandboxDeleting, now, sandbox.Revision+1
		_ = s.store.Update(func(state *store.State) error {
			state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
			appendEvent(state, eventFor(sandbox, "", "sandbox.expiring", now, nil))
			return nil
		})
	}
	return s.reconcileCleanupLocked(ctx, sandbox)
}

func (s *Service) reconcileCleanupLocked(ctx context.Context, sandbox domain.Sandbox) (domain.Sandbox, error) {
	target := sandbox.CleanupTarget
	if target != domain.SandboxDeleted && target != domain.SandboxExpired {
		return sandbox, fmt.Errorf("%w: invalid cleanup target", ErrConflict)
	}
	if sandbox.BackendID == "" {
		remote, err := s.backend.Find(ctx, sandbox.ID, sandbox.ProjectID)
		switch {
		case err == nil:
			sandbox.BackendID = remote.ID
		case errors.Is(err, backend.ErrNotFound):
			// A tenant-bound recovery lookup confirmed that no backend resource
			// exists, so cleanup can finish without inventing success.
		default:
			return s.markUnknownLocked(sandbox, "backend_cleanup_recovery_failed")
		}
	}
	if sandbox.BackendID != "" {
		if err := s.backend.Delete(ctx, sandbox.BackendID); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return s.markUnknownLocked(sandbox, "backend_cleanup_failed")
		}
	}
	routed, routeErr := s.removeNodeRoute(ctx, sandbox)
	if routeErr != nil {
		return s.markUnknownLocked(routed, "node_route_cleanup_failed")
	}
	sandbox = routed
	now := s.now()
	sandbox.State, sandbox.UpdatedAt, sandbox.Revision, sandbox.Failure = target, now, sandbox.Revision+1, nil
	err := s.store.Update(func(state *store.State) error {
		state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		if target == domain.SandboxDeleted {
			completeLatestOperation(state, sandbox.ProjectID, sandbox.ID, "delete_sandbox:", now)
		}
		appendEvent(state, eventFor(sandbox, "", "sandbox."+string(target), now, nil))
		return nil
	})
	return sandbox, err
}

func (s *Service) markMissingLocked(sandbox domain.Sandbox) (domain.Sandbox, error) {
	now := s.now()
	sandbox.State, sandbox.UpdatedAt, sandbox.Revision = domain.SandboxFailed, now, sandbox.Revision+1
	sandbox.Failure = &domain.Failure{Code: "backend_resource_missing", Message: "backend confirmed the sandbox resource is absent", Retryable: false}
	err := s.store.Update(func(state *store.State) error {
		state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		appendEvent(state, eventFor(sandbox, "", "sandbox.failed", now, map[string]any{"code": sandbox.Failure.Code}))
		return nil
	})
	return sandbox, err
}

func (s *Service) markUnknownLocked(sandbox domain.Sandbox, code string) (domain.Sandbox, error) {
	now := s.now()
	sandbox.State, sandbox.UpdatedAt, sandbox.Revision = domain.SandboxUnknown, now, sandbox.Revision+1
	sandbox.Failure = &domain.Failure{Code: code, Message: "backend state could not be confirmed", Retryable: true}
	err := s.store.Update(func(state *store.State) error {
		state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		appendEvent(state, eventFor(sandbox, "", "sandbox.unknown", now, map[string]any{"code": code}))
		return nil
	})
	return sandbox, err
}

func (s *Service) getSandbox(projectID, id string) (domain.Sandbox, error) {
	if reader, ok := s.store.(store.SandboxReader); ok {
		sandbox, err := reader.GetSandbox(projectID, id)
		return sandbox, translateStore(err)
	}
	var out domain.Sandbox
	err := s.store.View(func(state store.State) error {
		var ok bool
		out, ok = state.Sandboxes[store.ScopedKey(projectID, id)]
		if !ok {
			return store.ErrNotFound
		}
		return nil
	})
	return out, translateStore(err)
}

func (s *Service) lookupIdempotency(projectID, kind, key string) (domain.Operation, bool) {
	var operation domain.Operation
	_ = s.store.View(func(state store.State) error {
		operation, _ = idempotentOperation(state, projectID, kind, key)
		return nil
	})
	return operation, operation.ID != ""
}

func (s *Service) lookupIdempotencyInput(projectID, kind, key, digest string) (domain.Operation, bool, error) {
	var operation domain.Operation
	var lookupErr error
	err := s.store.View(func(state store.State) error {
		operation, _ = idempotentOperation(state, projectID, kind, key)
		if operation.ID != "" {
			lookupErr = requireIdempotencyDigest(state, projectID, kind, key, digest)
		}
		return nil
	})
	if err != nil {
		return domain.Operation{}, false, err
	}
	if lookupErr != nil {
		return domain.Operation{}, false, lookupErr
	}
	return operation, operation.ID != "", nil
}

func idempotentOperation(state store.State, projectID, kind, key string) (domain.Operation, bool) {
	id, ok := state.Idempotency[store.IdempotencyKey(projectID, kind, key)]
	if !ok {
		return domain.Operation{}, false
	}
	op, ok := state.Operations[store.ScopedKey(projectID, id)]
	return op, ok
}

func mutationDigest(input any) (string, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("encode idempotency input: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func requireIdempotencyDigest(state store.State, projectID, kind, key, digest string) error {
	stored := state.IdempotencyDigests[store.IdempotencyKey(projectID, kind, key)]
	if stored != "" && stored != digest {
		return fmt.Errorf("%w: Idempotency-Key was already used with a different request", ErrConflict)
	}
	return nil
}

func requireMutation(projectID, idempotencyKey string) error {
	if err := domain.ValidateProjectID(projectID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if !safeIdempotencyKey.MatchString(idempotencyKey) {
		return fmt.Errorf("%w: Idempotency-Key must contain 8-200 safe characters", ErrInvalid)
	}
	return nil
}

func translateStore(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, store.ErrConflict) {
		return ErrConflict
	}
	return err
}

func succeededOperation(projectID, kind, resourceID, idempotencyKey string, now time.Time) domain.Operation {
	id, _ := randomID("op")
	return domain.Operation{ID: id, ProjectID: projectID, Kind: kind, ResourceID: resourceID, State: domain.OperationSucceeded, IdempotencyKey: idempotencyKey, CreatedAt: now, UpdatedAt: now}
}

func eventFor(sandbox domain.Sandbox, operationID, eventType string, at time.Time, details map[string]any) domain.Event {
	id, _ := randomID("evt")
	return domain.Event{ID: id, ProjectID: sandbox.ProjectID, ResourceID: sandbox.ID, OperationID: operationID, Type: eventType, State: sandbox.State, At: at, Details: details}
}

func appendEvent(state *store.State, event domain.Event) {
	for _, existing := range state.Events {
		if existing.ProjectID == event.ProjectID && existing.ResourceID == event.ResourceID && existing.Sequence >= event.Sequence {
			event.Sequence = existing.Sequence + 1
		}
	}
	if event.Sequence == 0 {
		event.Sequence = 1
	}
	state.Events = append(state.Events, event)
}

func completeLatestOperation(state *store.State, projectID, resourceID, kindPrefix string, now time.Time) {
	var selectedKey string
	var selected domain.Operation
	for key, operation := range state.Operations {
		if operation.ProjectID != projectID || operation.ResourceID != resourceID || !strings.HasPrefix(operation.Kind, kindPrefix) {
			continue
		}
		if selectedKey == "" || selected.UpdatedAt.Before(operation.UpdatedAt) {
			selectedKey, selected = key, operation
		}
	}
	if selectedKey != "" {
		selected.State, selected.UpdatedAt, selected.Failure = domain.OperationSucceeded, now, nil
		state.Operations[selectedKey] = selected
	}
}

func terminal(state domain.SandboxState) bool {
	return state == domain.SandboxDeleted || state == domain.SandboxExpired || state == domain.SandboxFailed
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func randomID(prefix string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(raw), nil
}

func revisionID(prefix string, value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := receipt.DigestString(string(data))
	return prefix + "_" + strings.TrimPrefix(digest, "sha256:")[:24], nil
}
