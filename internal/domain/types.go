package domain

import "time"

type SandboxState string

const (
	SandboxRequested SandboxState = "requested"
	SandboxPreparing SandboxState = "preparing"
	SandboxRunning   SandboxState = "running"
	SandboxPausing   SandboxState = "pausing"
	SandboxStandby   SandboxState = "standby"
	SandboxResuming  SandboxState = "resuming"
	SandboxDeleting  SandboxState = "deleting"
	SandboxDeleted   SandboxState = "deleted"
	SandboxExpired   SandboxState = "expired"
	SandboxFailed    SandboxState = "failed"
	SandboxUnknown   SandboxState = "unknown"
)

type OperationState string

const (
	OperationPending   OperationState = "pending"
	OperationRunning   OperationState = "running"
	OperationSucceeded OperationState = "succeeded"
	OperationFailed    OperationState = "failed"
)

type CheckpointKind string

const (
	CheckpointFilesystem CheckpointKind = "filesystem"
	CheckpointFullState  CheckpointKind = "full_state"
)

type Environment struct {
	RevisionID      string    `json:"revision_id"`
	ProjectID       string    `json:"project_id"`
	Name            string    `json:"name"`
	Backend         string    `json:"backend"`
	BackendTemplate string    `json:"backend_template"`
	ImageDigest     string    `json:"image_digest,omitempty"`
	PolicyRevision  string    `json:"policy_revision,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

type Lifecycle struct {
	StandbyAfterSeconds int64          `json:"standby_after_seconds,omitempty"`
	ExpiresAfterSeconds int64          `json:"expires_after_seconds"`
	StandbyCheckpoint   CheckpointKind `json:"standby_checkpoint_kind,omitempty"`
	AutoResume          bool           `json:"auto_resume,omitempty"`
}

type NetworkPolicy struct {
	AllowInternet bool     `json:"allow_internet"`
	AllowOut      []string `json:"allow_out,omitempty"`
	DenyOut       []string `json:"deny_out,omitempty"`
}

type Sandbox struct {
	ID                  string        `json:"id"`
	ProjectID           string        `json:"project_id"`
	EnvironmentRevision string        `json:"environment_revision"`
	Backend             string        `json:"backend"`
	BackendID           string        `json:"backend_id,omitempty"`
	State               SandboxState  `json:"state"`
	CleanupTarget       SandboxState  `json:"cleanup_target,omitempty"`
	Lifecycle           Lifecycle     `json:"lifecycle"`
	Network             NetworkPolicy `json:"network"`
	ConnectorRevisions  []string      `json:"connector_revisions,omitempty"`
	CreatedAt           time.Time     `json:"created_at"`
	UpdatedAt           time.Time     `json:"updated_at"`
	ExpiresAt           time.Time     `json:"expires_at"`
	Revision            int64         `json:"revision"`
	Failure             *Failure      `json:"failure,omitempty"`
}

type Failure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type Operation struct {
	ID             string         `json:"id"`
	ProjectID      string         `json:"project_id"`
	Kind           string         `json:"kind"`
	ResourceID     string         `json:"resource_id"`
	State          OperationState `json:"state"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	Failure        *Failure       `json:"failure,omitempty"`
}

type Event struct {
	ID          string         `json:"id"`
	Sequence    int64          `json:"sequence"`
	ProjectID   string         `json:"project_id"`
	ResourceID  string         `json:"resource_id"`
	OperationID string         `json:"operation_id,omitempty"`
	Type        string         `json:"type"`
	State       SandboxState   `json:"state,omitempty"`
	At          time.Time      `json:"at"`
	Details     map[string]any `json:"details,omitempty"`
}

type Checkpoint struct {
	ID                  string         `json:"id"`
	ProjectID           string         `json:"project_id"`
	SourceSandboxID     string         `json:"source_sandbox_id"`
	EnvironmentRevision string         `json:"environment_revision"`
	Backend             string         `json:"backend"`
	BackendRef          string         `json:"backend_ref"`
	Kind                CheckpointKind `json:"kind"`
	CreatedAt           time.Time      `json:"created_at"`
}

type Connector struct {
	RevisionID          string    `json:"revision_id"`
	ProjectID           string    `json:"project_id"`
	Name                string    `json:"name"`
	Destination         string    `json:"destination"`
	AllowedMethods      []string  `json:"allowed_methods"`
	AllowedPaths        []string  `json:"allowed_paths"`
	CredentialRef       string    `json:"credential_ref"`
	AllowPrivateNetwork bool      `json:"allow_private_network"`
	Status              string    `json:"status"`
	CreatedAt           time.Time `json:"created_at"`
}
