// Package perfbench measures the customer-observed runtime API boundary.
//
// It deliberately benchmarks through the public HTTP API instead of calling
// engine internals. Results are evidence for one named deployment and cache
// condition, not universal product claims.
package perfbench

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/infercrane/brezel/internal/domain"
)

type Scenario string

const (
	ScenarioTTI                  Scenario = "tti"
	ScenarioWarmExec             Scenario = "warm-exec"
	ScenarioResume               Scenario = "resume"
	ScenarioFilesystemCheckpoint Scenario = "filesystem-checkpoint"
	ScenarioFilesystemRestore    Scenario = "filesystem-restore"
	ScenarioPreviewFirstByte     Scenario = "preview-first-byte"
	ScenarioPreviewWarm          Scenario = "preview-warm"
	ScenarioWorkspaceIO          Scenario = "workspace-io"
)

type Mode string

const (
	ModeSequential Mode = "sequential"
	ModeStaggered  Mode = "staggered"
	ModeBurst      Mode = "burst"
)

type EvidenceClass string

const (
	EvidenceSyntheticTest   EvidenceClass = "synthetic-test"
	EvidenceLocalDocker     EvidenceClass = "local-docker"
	EvidenceSingleHostLinux EvidenceClass = "single-host-linux-kvm"
	EvidenceHostedEndToEnd  EvidenceClass = "hosted-end-to-end"
)

type CacheState string

const (
	CacheCold           CacheState = "cold"
	CacheCachedTemplate CacheState = "cached-template"
	CacheWarmPool       CacheState = "warm-pool"
	CacheUnknown        CacheState = "unknown"
)

type Config struct {
	BaseURL         string
	Token           string
	ProjectID       string
	BackendTemplate string
	Target          string
	RuntimeRevision string
	EvidenceClass   EvidenceClass
	CacheState      CacheState
	Scenario        Scenario
	Mode            Mode
	Runs            int
	MaxInFlight     int
	StaggerInterval time.Duration
	AttemptTimeout  time.Duration
	CleanupTimeout  time.Duration
	IOBytes         int
	PreviewPort     uint16
	Execute         bool
	Client          *http.Client
}

type Metadata struct {
	Target                    string        `json:"target"`
	RuntimeRevision           string        `json:"runtime_revision"`
	EvidenceClass             EvidenceClass `json:"evidence_class"`
	CacheState                CacheState    `json:"cache_state"`
	BackendTemplate           string        `json:"backend_template"`
	ProjectID                 string        `json:"project_id"`
	APITransport              string        `json:"api_transport"`
	HTTPConnectionReuse       bool          `json:"http_connection_reuse"`
	HTTPConnectionReuseSource string        `json:"http_connection_reuse_source"`
	CacheStateSource          string        `json:"cache_state_source"`
	PercentileMethod          string        `json:"percentile_method"`
	LatencyDefinition         string        `json:"latency_definition"`
	ServiceLatencyDefinition  string        `json:"service_latency_definition"`
	Command                   []string      `json:"command"`
	CommandSuccessDefinition  string        `json:"command_success_definition"`
	ScenarioSetupDefinition   string        `json:"scenario_setup_definition"`
	VerificationDefinition    string        `json:"verification_definition"`
	IOBytes                   int           `json:"io_bytes,omitempty"`
	PreviewPort               uint16        `json:"preview_port,omitempty"`
	ClientOS                  string        `json:"client_os"`
	ClientArch                string        `json:"client_arch"`
	ClientGoVersion           string        `json:"client_go_version"`
	EnvironmentRevision       string        `json:"environment_revision,omitempty"`
	PreparationRequestID      string        `json:"preparation_request_id,omitempty"`
}

type PhaseDurations struct {
	SetupNS            int64 `json:"setup_ns,omitempty"`
	CreateNS           int64 `json:"create_ns,omitempty"`
	PrimeCommandNS     int64 `json:"prime_command_ns,omitempty"`
	PauseNS            int64 `json:"pause_ns,omitempty"`
	ResumeNS           int64 `json:"resume_ns,omitempty"`
	FirstCommandNS     int64 `json:"first_command_ns,omitempty"`
	FileWriteNS        int64 `json:"file_write_ns,omitempty"`
	FileReadNS         int64 `json:"file_read_ns,omitempty"`
	CheckpointNS       int64 `json:"checkpoint_ns,omitempty"`
	RestoreNS          int64 `json:"restore_ns,omitempty"`
	PortLeaseNS        int64 `json:"port_lease_ns,omitempty"`
	PreviewFirstByteNS int64 `json:"preview_first_byte_ns,omitempty"`
	PreviewCompleteNS  int64 `json:"preview_complete_ns,omitempty"`
	CleanupNS          int64 `json:"cleanup_ns,omitempty"`
}

// CleanupResource records cleanup evidence without exposing a runtime resource
// identity in a benchmark artifact. IdentityKnown means the harness received an
// ID it could use for cleanup; Outcome remains unknown when a mutation may have
// succeeded but returned no identity.
type CleanupResource struct {
	Kind          string `json:"kind"`
	IdentityKnown bool   `json:"identity_known"`
	Outcome       string `json:"outcome"`
	Attempts      int    `json:"attempts,omitempty"`
	ErrorCategory string `json:"error_category,omitempty"`
	ErrorDetail   string `json:"error_detail,omitempty"`
}

type Sample struct {
	Index                int               `json:"index"`
	ScheduledAt          time.Time         `json:"scheduled_at,omitempty"`
	StartedAt            time.Time         `json:"started_at,omitempty"`
	CompletedAt          time.Time         `json:"completed_at,omitempty"`
	ScheduleDelayNS      int64             `json:"schedule_delay_ns,omitempty"`
	ObservedNS           int64             `json:"observed_ns,omitempty"`
	MeasuredNS           int64             `json:"measured_ns,omitempty"`
	ReadyAgeNS           int64             `json:"ready_age_ns,omitempty"`
	Success              bool              `json:"success"`
	ErrorCategory        string            `json:"error_category,omitempty"`
	ErrorDetail          string            `json:"error_detail,omitempty"`
	FailureStage         string            `json:"failure_stage,omitempty"`
	CleanupSucceeded     bool              `json:"cleanup_succeeded"`
	CleanupAttempts      int               `json:"cleanup_attempts,omitempty"`
	CleanupErrorCategory string            `json:"cleanup_error_category,omitempty"`
	CleanupError         string            `json:"cleanup_error,omitempty"`
	ResourceOutcome      string            `json:"resource_outcome"`
	CleanupResources     []CleanupResource `json:"cleanup_resources,omitempty"`
	RequestIDs           []string          `json:"request_ids,omitempty"`
	BytesWritten         int64             `json:"bytes_written,omitempty"`
	BytesRead            int64             `json:"bytes_read,omitempty"`
	Phases               PhaseDurations    `json:"phases"`
}

type LatencySummary struct {
	Samples int     `json:"samples"`
	MinMS   float64 `json:"min_ms"`
	P50MS   float64 `json:"p50_ms"`
	P95MS   float64 `json:"p95_ms"`
	P99MS   float64 `json:"p99_ms"`
	MaxMS   float64 `json:"max_ms"`
	MeanMS  float64 `json:"mean_ms"`
}

type Summary struct {
	Requested                    int            `json:"requested"`
	Scheduled                    int            `json:"scheduled"`
	Started                      int            `json:"started"`
	Completed                    int            `json:"completed"`
	Succeeded                    int            `json:"succeeded"`
	Failed                       int            `json:"failed"`
	SuccessRate                  float64        `json:"success_rate"`
	CleanupNotRequired           int            `json:"cleanup_not_required"`
	CleanupSucceeded             int            `json:"cleanup_succeeded"`
	CleanupFailed                int            `json:"cleanup_failed"`
	CleanupResourcesExpected     int            `json:"cleanup_resources_expected"`
	CleanupResourcesSucceeded    int            `json:"cleanup_resources_succeeded"`
	CleanupResourcesFailed       int            `json:"cleanup_resources_failed"`
	SuccessfulBytesWritten       int64          `json:"successful_bytes_written,omitempty"`
	SuccessfulBytesRead          int64          `json:"successful_bytes_read,omitempty"`
	MeasurementWindowNS          int64          `json:"measurement_window_ns,omitempty"`
	TimeToFirstSuccessNS         int64          `json:"time_to_first_success_ns,omitempty"`
	ObservedCompletionsPerSecond float64        `json:"observed_completions_per_second,omitempty"`
	Latency                      LatencySummary `json:"latency"`
	ServiceLatency               LatencySummary `json:"service_latency"`
	FailureLatency               LatencySummary `json:"failure_latency"`
	LatencyCensored              int            `json:"latency_censored"`
	Errors                       map[string]int `json:"errors,omitempty"`
}

type Report struct {
	SchemaVersion            int       `json:"schema_version"`
	StartedAt                time.Time `json:"started_at"`
	FinishedAt               time.Time `json:"finished_at"`
	Boundary                 string    `json:"boundary"`
	Scenario                 Scenario  `json:"scenario"`
	Mode                     Mode      `json:"mode"`
	Runs                     int       `json:"runs"`
	MaxInFlight              int       `json:"max_in_flight"`
	StaggerNS                int64     `json:"stagger_ns,omitempty"`
	Metadata                 Metadata  `json:"metadata"`
	PreparationErrorCategory string    `json:"preparation_error_category,omitempty"`
	PreparationErrorDetail   string    `json:"preparation_error_detail,omitempty"`
	Summary                  Summary   `json:"summary"`
	Samples                  []Sample  `json:"samples"`
}

func (c Config) validate() (*url.URL, error) {
	if !c.Execute {
		return nil, errors.New("benchmark requires explicit Execute=true because it creates and deletes real resources")
	}
	if len(c.Token) < 32 || strings.ContainsAny(c.Token, "\r\n") {
		return nil, errors.New("service token must be at least 32 characters and contain no newlines")
	}
	if c.ProjectID == "" || c.BackendTemplate == "" || c.Target == "" || c.RuntimeRevision == "" {
		return nil, errors.New("project, backend template, target, and runtime revision are required")
	}
	if err := domain.ValidateProjectID(c.ProjectID); err != nil {
		return nil, fmt.Errorf("invalid benchmark project: %w", err)
	}
	if strings.ContainsAny(c.ProjectID+c.BackendTemplate+c.Target+c.RuntimeRevision, "\r\n") {
		return nil, errors.New("identity metadata must not contain newlines")
	}
	if len(c.Target) > 256 || len(c.RuntimeRevision) > 256 || len(c.BackendTemplate) > 256 {
		return nil, errors.New("identity metadata exceeds 256 characters")
	}
	if !validScenario(c.Scenario) {
		return nil, fmt.Errorf("unsupported scenario %q", c.Scenario)
	}
	if !validMode(c.Mode) {
		return nil, fmt.Errorf("unsupported mode %q", c.Mode)
	}
	if !validEvidenceClass(c.EvidenceClass) {
		return nil, fmt.Errorf("unsupported evidence class %q", c.EvidenceClass)
	}
	if !validCacheState(c.CacheState) {
		return nil, fmt.Errorf("unsupported cache state %q", c.CacheState)
	}
	if c.Runs < 1 || c.Runs > 256 {
		return nil, errors.New("runs must be between 1 and 256")
	}
	if c.MaxInFlight < 1 || c.MaxInFlight > 256 {
		return nil, errors.New("max in-flight must be between 1 and 256")
	}
	if c.Mode == ModeSequential && c.MaxInFlight != 1 {
		return nil, errors.New("sequential mode requires max in-flight=1")
	}
	if c.Mode == ModeBurst && c.MaxInFlight < c.Runs {
		return nil, errors.New("burst mode requires max in-flight to be at least runs")
	}
	if c.Mode == ModeStaggered && c.StaggerInterval <= 0 {
		return nil, errors.New("staggered mode requires a positive stagger interval")
	}
	if c.StaggerInterval > 10*time.Minute {
		return nil, errors.New("stagger interval cannot exceed 10 minutes")
	}
	if c.AttemptTimeout < time.Second || c.AttemptTimeout > 30*time.Minute {
		return nil, errors.New("attempt timeout must be between 1 second and 30 minutes")
	}
	if c.CleanupTimeout < time.Second || c.CleanupTimeout > 30*time.Minute {
		return nil, errors.New("cleanup timeout must be between 1 second and 30 minutes")
	}
	if c.IOBytes == 0 {
		c.IOBytes = 1 << 20
	}
	if c.IOBytes < 1 || c.IOBytes > 32<<20 {
		return nil, errors.New("I/O bytes must be between 1 and 33554432")
	}
	if c.PreviewPort == 0 {
		c.PreviewPort = 8080
	}
	return benchmarkBaseURL(c.BaseURL)
}

func validScenario(value Scenario) bool {
	switch value {
	case ScenarioTTI, ScenarioWarmExec, ScenarioResume,
		ScenarioFilesystemCheckpoint, ScenarioFilesystemRestore,
		ScenarioPreviewFirstByte, ScenarioPreviewWarm, ScenarioWorkspaceIO:
		return true
	default:
		return false
	}
}

func validMode(value Mode) bool {
	return value == ModeSequential || value == ModeStaggered || value == ModeBurst
}

func validEvidenceClass(value EvidenceClass) bool {
	switch value {
	case EvidenceSyntheticTest, EvidenceLocalDocker, EvidenceSingleHostLinux, EvidenceHostedEndToEnd:
		return true
	default:
		return false
	}
}

func validCacheState(value CacheState) bool {
	switch value {
	case CacheCold, CacheCachedTemplate, CacheWarmPool, CacheUnknown:
		return true
	default:
		return false
	}
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
