package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/infercrane/sandbox-runtime-lab/internal/backend"
	"github.com/infercrane/sandbox-runtime-lab/internal/connector"
	"github.com/infercrane/sandbox-runtime-lab/internal/domain"
	"github.com/infercrane/sandbox-runtime-lab/internal/receipt"
	"github.com/infercrane/sandbox-runtime-lab/internal/store"
)

var (
	ErrInvalid  = errors.New("invalid request")
	ErrNotFound = errors.New("resource not found")
	ErrConflict = errors.New("resource conflict")
	ErrDenied   = errors.New("request denied")
	ErrBackend  = errors.New("backend failure")
)

type Service struct {
	mu      sync.Mutex
	store   store.Store
	backend backend.Backend
	broker  *connector.Broker
	signer  *receipt.Signer
	now     func() time.Time
}

type Option func(*Service)

func WithConnectorBroker(broker *connector.Broker) Option {
	return func(s *Service) { s.broker = broker }
}

func New(st store.Store, be backend.Backend, signer *receipt.Signer, options ...Option) (*Service, error) {
	if st == nil || be == nil {
		return nil, errors.New("store and backend are required")
	}
	if !be.Capabilities().HostileCodeIsolation {
		return nil, errors.New("release backend must enforce hostile-code isolation")
	}
	s := &Service{store: st, backend: be, signer: signer, now: func() time.Time { return time.Now().UTC() }}
	for _, option := range options {
		option(s)
	}
	return s, nil
}

func (s *Service) Capabilities() backend.Capabilities {
	capabilities := s.backend.Capabilities()
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
	Name            string `json:"name"`
	Backend         string `json:"backend"`
	BackendTemplate string `json:"backend_template"`
	ImageDigest     string `json:"image_digest,omitempty"`
	PolicyRevision  string `json:"policy_revision,omitempty"`
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
	EnvironmentRevision string               `json:"environment_revision"`
	Lifecycle           domain.Lifecycle     `json:"lifecycle"`
	Network             domain.NetworkPolicy `json:"network"`
	ConnectorRevisions  []string             `json:"connector_revisions,omitempty"`
}

func (s *Service) CreateEnvironment(projectID, idempotencyKey string, in CreateEnvironmentInput) (domain.Environment, domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := requireMutation(projectID, idempotencyKey); err != nil {
		return domain.Environment{}, domain.Operation{}, err
	}
	now := s.now()
	env := domain.Environment{ProjectID: projectID, Name: in.Name, Backend: in.Backend, BackendTemplate: in.BackendTemplate, ImageDigest: in.ImageDigest, PolicyRevision: in.PolicyRevision, CreatedAt: now}
	if err := domain.ValidateEnvironment(env); err != nil {
		return env, domain.Operation{}, fmt.Errorf("%w: %v", ErrInvalid, err)
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
			op = existing
			env = state.Environments[store.ScopedKey(projectID, existing.ResourceID)]
			return nil
		}
		key := store.ScopedKey(projectID, revision)
		if _, ok := state.Environments[key]; ok {
			return store.ErrConflict
		}
		state.Environments[key] = env
		state.Operations[store.ScopedKey(projectID, op.ID)] = op
		state.Idempotency[store.IdempotencyKey(projectID, "create_environment", idempotencyKey)] = op.ID
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
			op = existing
			connector = state.Connectors[store.ScopedKey(projectID, existing.ResourceID)]
			return nil
		}
		state.Connectors[store.ScopedKey(projectID, revision)] = connector
		state.Operations[store.ScopedKey(projectID, op.ID)] = op
		state.Idempotency[store.IdempotencyKey(projectID, "create_connector", idempotencyKey)] = op.ID
		return nil
	})
	return connector, op, err
}

func (s *Service) CreateSandbox(ctx context.Context, projectID, idempotencyKey string, in CreateSandboxInput) (domain.Sandbox, domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := requireMutation(projectID, idempotencyKey); err != nil {
		return domain.Sandbox{}, domain.Operation{}, err
	}
	if in.Lifecycle.StandbyCheckpoint == "" {
		in.Lifecycle.StandbyCheckpoint = domain.CheckpointFullState
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
	var env domain.Environment
	var existingOp domain.Operation
	err := s.store.View(func(state store.State) error {
		if op, ok := idempotentOperation(state, projectID, "create_sandbox", idempotencyKey); ok {
			existingOp = op
			return nil
		}
		var ok bool
		env, ok = state.Environments[store.ScopedKey(projectID, in.EnvironmentRevision)]
		if !ok {
			return store.ErrNotFound
		}
		for _, revision := range in.ConnectorRevisions {
			if _, ok := state.Connectors[store.ScopedKey(projectID, revision)]; !ok {
				return store.ErrNotFound
			}
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
		State: domain.SandboxPreparing, Lifecycle: in.Lifecycle, Network: in.Network,
		ConnectorRevisions: append([]string(nil), in.ConnectorRevisions...), CreatedAt: now, UpdatedAt: now,
		ExpiresAt: now.Add(time.Duration(in.Lifecycle.ExpiresAfterSeconds) * time.Second), Revision: 1,
	}
	op := domain.Operation{ID: opID, ProjectID: projectID, Kind: "create_sandbox", ResourceID: sandboxID, State: domain.OperationRunning, IdempotencyKey: idempotencyKey, CreatedAt: now, UpdatedAt: now}
	backendEnvironment := map[string]string{}
	if len(sandbox.ConnectorRevisions) > 0 {
		lease, leaseErr := s.broker.Issue(projectID, sandbox.ID, sandbox.ConnectorRevisions)
		if leaseErr != nil {
			return domain.Sandbox{}, domain.Operation{}, fmt.Errorf("%w: issue connector lease", ErrDenied)
		}
		backendEnvironment["RUNTIME_CONNECTOR_GATEWAY_URL"] = s.broker.GatewayURL() + "/proxy"
		backendEnvironment["RUNTIME_CONNECTOR_RENEW_URL"] = s.broker.GatewayURL() + "/leases/renew"
		backendEnvironment["RUNTIME_CONNECTOR_LEASE"] = lease
	}
	err = s.store.Update(func(state *store.State) error {
		if existing, ok := idempotentOperation(*state, projectID, "create_sandbox", idempotencyKey); ok {
			op = existing
			sandbox = state.Sandboxes[store.ScopedKey(projectID, existing.ResourceID)]
			return nil
		}
		state.Sandboxes[store.ScopedKey(projectID, sandboxID)] = sandbox
		state.Operations[store.ScopedKey(projectID, opID)] = op
		state.Idempotency[store.IdempotencyKey(projectID, "create_sandbox", idempotencyKey)] = opID
		appendEvent(state, eventFor(sandbox, opID, "sandbox.preparing", now, nil))
		return nil
	})
	if err != nil {
		return sandbox, op, err
	}
	if op.ResourceID != sandboxID {
		return sandbox, op, nil
	}

	remote, backendErr := s.backend.Create(ctx, backend.CreateRequest{
		LocalSandboxID: sandbox.ID, ProjectID: projectID, TemplateID: env.BackendTemplate,
		Lifecycle: sandbox.Lifecycle, Network: sandbox.Network, Environment: backendEnvironment,
	})
	if backendErr != nil {
		failure := &domain.Failure{Code: "backend_create_unconfirmed", Message: "sandbox backend did not confirm whether the resource was created", Retryable: true}
		now = s.now()
		sandbox.State, sandbox.Failure, sandbox.UpdatedAt, sandbox.Revision = domain.SandboxUnknown, failure, now, sandbox.Revision+1
		op.State, op.Failure, op.UpdatedAt = domain.OperationFailed, failure, now
		_ = s.store.Update(func(state *store.State) error {
			state.Sandboxes[store.ScopedKey(projectID, sandbox.ID)] = sandbox
			state.Operations[store.ScopedKey(projectID, op.ID)] = op
			appendEvent(state, eventFor(sandbox, op.ID, "sandbox.unknown", now, map[string]any{"code": failure.Code}))
			return nil
		})
		return sandbox, op, fmt.Errorf("%w: create sandbox", ErrBackend)
	}
	now = s.now()
	sandbox.BackendID, sandbox.State, sandbox.UpdatedAt, sandbox.Revision = remote.ID, remote.State, now, sandbox.Revision+1
	sandbox.Failure = nil
	op.State, op.UpdatedAt = domain.OperationSucceeded, now
	err = s.store.Update(func(state *store.State) error {
		state.Sandboxes[store.ScopedKey(projectID, sandbox.ID)] = sandbox
		state.Operations[store.ScopedKey(projectID, op.ID)] = op
		appendEvent(state, eventFor(sandbox, op.ID, "sandbox.running", now, nil))
		return nil
	})
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
	if terminal(sandbox.State) {
		return sandbox, nil
	}
	if sandbox.CleanupTarget == domain.SandboxDeleted || sandbox.CleanupTarget == domain.SandboxExpired {
		return s.reconcileCleanupLocked(ctx, sandbox)
	}
	if !s.now().Before(sandbox.ExpiresAt) {
		return s.expireLocked(ctx, sandbox)
	}
	if sandbox.BackendID == "" {
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
	return s.lifecycleAction(ctx, projectID, sandboxID, idempotencyKey, "pause_sandbox", domain.SandboxRunning, domain.SandboxPausing, domain.SandboxStandby, func(sandbox domain.Sandbox) error {
		return s.backend.Pause(ctx, sandbox.BackendID, sandbox.Lifecycle.StandbyCheckpoint)
	})
}

func (s *Service) Resume(ctx context.Context, projectID, sandboxID, idempotencyKey string) (domain.Sandbox, domain.Operation, error) {
	return s.lifecycleAction(ctx, projectID, sandboxID, idempotencyKey, "resume_sandbox", domain.SandboxStandby, domain.SandboxResuming, domain.SandboxRunning, func(sandbox domain.Sandbox) error {
		remaining := int64(sandbox.ExpiresAt.Sub(s.now()).Seconds())
		if remaining < 1 {
			return fmt.Errorf("%w: sandbox expired", ErrConflict)
		}
		_, err := s.backend.Resume(ctx, sandbox.BackendID, sandbox.Lifecycle.StandbyCheckpoint, remaining)
		return err
	})
}

func (s *Service) Delete(ctx context.Context, projectID, sandboxID, idempotencyKey string) (domain.Sandbox, domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	if sandbox.State == domain.SandboxDeleted || sandbox.State == domain.SandboxExpired {
		return sandbox, domain.Operation{}, fmt.Errorf("%w: sandbox is already terminal", ErrConflict)
	}
	sandbox.CleanupTarget = domain.SandboxDeleted
	op, err := s.beginTransitionLocked(sandbox, idempotencyKey, kind, domain.SandboxDeleting)
	if err != nil {
		return sandbox, op, err
	}
	sandbox, _ = s.getSandbox(projectID, sandboxID)
	backendErr := s.backend.Delete(ctx, sandbox.BackendID)
	if backendErr != nil && !errors.Is(backendErr, backend.ErrNotFound) {
		return s.failTransitionLocked(sandbox, op, "backend_delete_failed")
	}
	return s.completeTransitionLocked(sandbox, op, domain.SandboxDeleted)
}

func (s *Service) Checkpoint(ctx context.Context, projectID, sandboxID, idempotencyKey, name string, kind domain.CheckpointKind) (domain.Checkpoint, domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := requireMutation(projectID, idempotencyKey); err != nil {
		return domain.Checkpoint{}, domain.Operation{}, err
	}
	if name == "" || strings.ContainsAny(name, "\r\n") {
		return domain.Checkpoint{}, domain.Operation{}, fmt.Errorf("%w: checkpoint name is required", ErrInvalid)
	}
	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return domain.Checkpoint{}, domain.Operation{}, err
	}
	if sandbox.State != domain.SandboxRunning {
		return domain.Checkpoint{}, domain.Operation{}, fmt.Errorf("%w: sandbox must be running", ErrConflict)
	}
	capabilities := s.backend.Capabilities()
	if (kind == domain.CheckpointFilesystem && !capabilities.FilesystemCheckpoint) || (kind == domain.CheckpointFullState && !capabilities.FullStateCheckpoint) {
		return domain.Checkpoint{}, domain.Operation{}, fmt.Errorf("%w: requested checkpoint kind is not enforced by backend", ErrDenied)
	}
	kindKey := "checkpoint_sandbox:" + sandboxID
	if existing, ok := s.lookupIdempotency(projectID, kindKey, idempotencyKey); ok {
		var checkpoint domain.Checkpoint
		_ = s.store.View(func(state store.State) error {
			checkpoint = state.Checkpoints[store.ScopedKey(projectID, existing.ResourceID)]
			return nil
		})
		return checkpoint, existing, nil
	}
	opID, _ := randomID("op")
	now := s.now()
	op := domain.Operation{ID: opID, ProjectID: projectID, Kind: kindKey, ResourceID: sandboxID, State: domain.OperationRunning, IdempotencyKey: idempotencyKey, CreatedAt: now, UpdatedAt: now}
	if err := s.store.Update(func(state *store.State) error {
		state.Operations[store.ScopedKey(projectID, op.ID)] = op
		state.Idempotency[store.IdempotencyKey(projectID, kindKey, idempotencyKey)] = op.ID
		return nil
	}); err != nil {
		return domain.Checkpoint{}, op, err
	}
	remote, err := s.backend.Checkpoint(ctx, sandbox.BackendID, kind, name)
	if err != nil {
		failure := &domain.Failure{Code: "backend_checkpoint_failed", Message: "sandbox backend could not create the checkpoint", Retryable: true}
		op.State, op.Failure, op.UpdatedAt = domain.OperationFailed, failure, s.now()
		_ = s.store.Update(func(state *store.State) error { state.Operations[store.ScopedKey(projectID, op.ID)] = op; return nil })
		return domain.Checkpoint{}, op, fmt.Errorf("%w: create checkpoint", ErrBackend)
	}
	checkpointID, _ := randomID("chk")
	checkpoint := domain.Checkpoint{ID: checkpointID, ProjectID: projectID, SourceSandboxID: sandboxID, EnvironmentRevision: sandbox.EnvironmentRevision, Backend: sandbox.Backend, BackendRef: remote.Ref, Kind: remote.Kind, CreatedAt: s.now()}
	op.ResourceID, op.State, op.UpdatedAt = checkpointID, domain.OperationSucceeded, s.now()
	_ = s.store.Update(func(state *store.State) error {
		state.Checkpoints[store.ScopedKey(projectID, checkpointID)] = checkpoint
		state.Operations[store.ScopedKey(projectID, op.ID)] = op
		appendEvent(state, eventFor(sandbox, op.ID, "checkpoint.created", op.UpdatedAt, map[string]any{"checkpoint_id": checkpointID, "kind": kind}))
		return nil
	})
	return checkpoint, op, nil
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
			EnvironmentRevision: env.RevisionID, ImageDigest: env.ImageDigest, PolicyRevision: env.PolicyRevision,
			Backend: sandbox.Backend, BackendIDDigest: backendDigest, Lifecycle: sandbox.Lifecycle,
			NetworkPolicyDigest: networkDigest, EventLogDigest: eventsDigest,
			InputIdentity: "none:lifecycle-receipt", OutputIdentity: "none:lifecycle-receipt",
			CleanupIdentity: fmt.Sprintf("state:%s@revision:%d", sandbox.State, sandbox.Revision),
			Assurance:       "signed control-plane claim; not hardware attestation",
		},
	}
	return s.signer.Sign(statement)
}

func (s *Service) Reconcile(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
		if sandbox.CleanupTarget == domain.SandboxDeleted || sandbox.CleanupTarget == domain.SandboxExpired {
			_, _ = s.reconcileCleanupLocked(ctx, sandbox)
			continue
		}
		if !s.now().Before(sandbox.ExpiresAt) {
			_, _ = s.expireLocked(ctx, sandbox)
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
		sandbox.BackendID, sandbox.State, sandbox.UpdatedAt, sandbox.Revision, sandbox.Failure = remote.ID, remote.State, now, sandbox.Revision+1, nil
		_ = s.store.Update(func(state *store.State) error {
			state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
			if sandbox.State == domain.SandboxRunning {
				completeLatestOperation(state, sandbox.ProjectID, sandbox.ID, "create_sandbox", now)
			}
			appendEvent(state, eventFor(sandbox, "", "sandbox.reconciled", now, nil))
			return nil
		})
	}
	return nil
}

func (s *Service) lifecycleAction(ctx context.Context, projectID, sandboxID, idempotencyKey, kind string, from, transitional, target domain.SandboxState, call func(domain.Sandbox) error) (domain.Sandbox, domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	if sandbox.State != from {
		return sandbox, domain.Operation{}, fmt.Errorf("%w: sandbox is %s, expected %s", ErrConflict, sandbox.State, from)
	}
	op, err := s.beginTransitionLocked(sandbox, idempotencyKey, idempotencyKind, transitional)
	if err != nil {
		return sandbox, op, err
	}
	sandbox, _ = s.getSandbox(projectID, sandboxID)
	if err := call(sandbox); err != nil {
		return s.failTransitionLocked(sandbox, op, "backend_"+kind+"_failed")
	}
	return s.completeTransitionLocked(sandbox, op, target)
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
	op.State, op.UpdatedAt, op.Failure = domain.OperationSucceeded, now, nil
	err := s.store.Update(func(state *store.State) error {
		state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		state.Operations[store.ScopedKey(sandbox.ProjectID, op.ID)] = op
		appendEvent(state, eventFor(sandbox, op.ID, "sandbox."+string(target), now, nil))
		return nil
	})
	return sandbox, op, err
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

func idempotentOperation(state store.State, projectID, kind, key string) (domain.Operation, bool) {
	id, ok := state.Idempotency[store.IdempotencyKey(projectID, kind, key)]
	if !ok {
		return domain.Operation{}, false
	}
	op, ok := state.Operations[store.ScopedKey(projectID, id)]
	return op, ok
}

func requireMutation(projectID, idempotencyKey string) error {
	if err := domain.ValidateProjectID(projectID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 200 || strings.ContainsAny(idempotencyKey, "\r\n") {
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
