package store

import "github.com/infercrane/brezel/internal/domain"

// IdempotencyLookup is the exact durable claim for one project, operation
// kind, and caller-supplied request key. The digest is kept beside the lookup
// so callers can reject reuse with different input without scanning unrelated
// operations.
type IdempotencyLookup struct {
	Operation domain.Operation
	Digest    string
	Found     bool
}

// IdempotencyReader is an optional read-optimized store extension. FileStore
// and custom Store implementations retain the existing whole-state fallback.
type IdempotencyReader interface {
	LookupIdempotency(projectID, kind, key string) (IdempotencyLookup, error)
}

// SandboxAdmissionQuery names the complete read set needed before a sandbox
// create or filesystem-checkpoint restore can reserve capacity. It is a typed,
// deliberately narrow contract rather than an arbitrary store filter.
type SandboxAdmissionQuery struct {
	ProjectID           string
	IdempotencyKind     string
	IdempotencyKey      string
	EnvironmentRevision string
	CheckpointID        string
	ConnectorRevisions  []string
	WorkspaceIDs        []string
}

// SandboxAdmissionSnapshot is one transactionally consistent admission view.
// Missing requested dependencies are omitted from their maps and interpreted
// by the service as not found.
type SandboxAdmissionSnapshot struct {
	Idempotency          IdempotencyLookup
	ActiveForProject     int
	ActiveTotal          int
	Environment          domain.Environment
	EnvironmentFound     bool
	Checkpoint           domain.Checkpoint
	CheckpointFound      bool
	Connectors           map[string]domain.Connector
	Workspaces           map[string]domain.Workspace
	AttachedWorkspaceIDs map[string]bool
}

// SandboxAdmissionReader avoids materializing the complete durable ledger for
// the latency-critical create/restore admission decision.
type SandboxAdmissionReader interface {
	ReadSandboxAdmission(SandboxAdmissionQuery) (SandboxAdmissionSnapshot, error)
}

// WorkspaceAdmissionQuery names the bounded read set needed before creating
// one durable workspace. ActiveForProject is backed by a derived counter for
// stores that implement WorkspaceAdmissionReader.
type WorkspaceAdmissionQuery struct {
	ProjectID       string
	IdempotencyKind string
	IdempotencyKey  string
}

type WorkspaceAdmissionSnapshot struct {
	Idempotency      IdempotencyLookup
	ActiveForProject int
}

type WorkspaceAdmissionReader interface {
	ReadWorkspaceAdmission(WorkspaceAdmissionQuery) (WorkspaceAdmissionSnapshot, error)
}

// WorkspaceReader and WorkspaceAttachmentReader keep exact workspace reads
// explicit. They intentionally do not expose SQL predicates or arbitrary
// filtering to the service layer.
type WorkspaceReader interface {
	GetWorkspace(projectID, workspaceID string) (domain.Workspace, error)
}

type WorkspaceAttachmentReader interface {
	WorkspaceAttached(projectID, workspaceID string) (bool, error)
}

// EnvironmentAdmissionQuery covers the content-addressed environment create
// path: replay, exact revision reuse, and per-project quota.
type EnvironmentAdmissionQuery struct {
	ProjectID       string
	IdempotencyKind string
	IdempotencyKey  string
	RevisionID      string
}

type EnvironmentAdmissionSnapshot struct {
	Idempotency      IdempotencyLookup
	Environment      domain.Environment
	EnvironmentFound bool
	CountForProject  int
}

type EnvironmentAdmissionReader interface {
	ReadEnvironmentAdmission(EnvironmentAdmissionQuery) (EnvironmentAdmissionSnapshot, error)
}

type EnvironmentReader interface {
	GetEnvironment(projectID, revisionID string) (domain.Environment, error)
}
