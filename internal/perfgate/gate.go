// Package perfgate turns Brezel benchmark matrices into a conservative,
// machine-checkable release decision. It intentionally accepts only complete,
// successful, repeated matrices with identical test configuration.
package perfgate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const matrixSchemaVersion = 3

type Metric string

const (
	MetricScheduledP50 Metric = "scheduled_p50_ms"
	MetricScheduledP95 Metric = "scheduled_p95_ms"
	MetricScheduledP99 Metric = "scheduled_p99_ms"
	MetricServiceP50   Metric = "service_p50_ms"
	MetricServiceP95   Metric = "service_p95_ms"
	MetricServiceP99   Metric = "service_p99_ms"
	MetricThroughput   Metric = "throughput_per_second"
)

type Target struct {
	Case   string `json:"case"`
	Metric Metric `json:"metric"`
}

type Config struct {
	BaselinePaths        []string
	CandidatePaths       []string
	Targets              []Target
	RequiredImprovement  float64
	MaxLatencyRegression float64
	MaxThroughputLoss    float64
	AbsoluteToleranceMS  float64
}

type Decision string

const (
	DecisionPromote Decision = "promote"
	DecisionReject  Decision = "reject"
)

type Result struct {
	SchemaVersion        int      `json:"schema_version"`
	Decision             Decision `json:"decision"`
	BaselineRevisions    []string `json:"baseline_revisions"`
	CandidateRevisions   []string `json:"candidate_revisions"`
	RequiredImprovement  float64  `json:"required_improvement_percent"`
	MaxLatencyRegression float64  `json:"max_latency_regression_percent"`
	MaxThroughputLoss    float64  `json:"max_throughput_loss_percent"`
	AbsoluteToleranceMS  float64  `json:"absolute_latency_tolerance_ms"`
	Checks               []Check  `json:"checks"`
	Failures             []string `json:"failures,omitempty"`
}

type Check struct {
	Case               string    `json:"case"`
	Metric             Metric    `json:"metric"`
	Purpose            string    `json:"purpose"`
	BaselineSamples    []float64 `json:"baseline_replicates"`
	CandidateSamples   []float64 `json:"candidate_replicates"`
	BaselineAggregate  float64   `json:"baseline_median"`
	CandidateAggregate float64   `json:"candidate_median"`
	ChangePercent      float64   `json:"change_percent"`
	Passed             bool      `json:"passed"`
	Reason             string    `json:"reason"`
}

type matrix struct {
	sourcePath         string
	SchemaVersion      int             `json:"schema_version"`
	Target             string          `json:"target"`
	RuntimeRevision    string          `json:"runtime_revision"`
	EvidenceClass      string          `json:"evidence_class"`
	CacheState         string          `json:"cache_state"`
	BackendTemplate    string          `json:"backend_template"`
	Project            string          `json:"project"`
	Configuration      json.RawMessage `json:"configuration"`
	ExpectedCases      int             `json:"expected_cases"`
	CompletedCases     int             `json:"completed_cases"`
	PassedCases        int             `json:"passed_cases"`
	FailedCases        int             `json:"failed_cases"`
	PlannedAttempts    int             `json:"planned_attempts"`
	RequestedAttempts  int             `json:"requested_attempts"`
	ScheduledAttempts  int             `json:"scheduled_attempts"`
	StartedAttempts    int             `json:"started_attempts"`
	CompletedAttempts  int             `json:"completed_attempts"`
	SuccessfulAttempts int             `json:"successful_attempts"`
	FailedAttempts     int             `json:"failed_attempts"`
	LatencyCensored    int             `json:"latency_censored"`
	Cleanup            matrixCleanup   `json:"cleanup"`
	Outcome            string          `json:"outcome"`
	HostMetadata       hostMetadata    `json:"host_metadata"`
	Cases              []matrixCase    `json:"cases"`
}

type hostMetadata struct {
	Before string `json:"before"`
	After  string `json:"after"`
}

type hostSnapshot struct {
	SchemaVersion int    `json:"schema_version"`
	Phase         string `json:"phase"`
	Host          struct {
		OS struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		} `json:"os"`
		KernelRelease  string `json:"kernel_release"`
		Architecture   string `json:"architecture"`
		Virtualization string `json:"virtualization"`
		CPU            struct {
			Model          string `json:"model"`
			Sockets        string `json:"sockets"`
			CoresPerSocket string `json:"cores_per_socket"`
			ThreadsPerCore string `json:"threads_per_core"`
			Governors      string `json:"governors"`
		} `json:"cpu"`
		Memory struct {
			TotalKiB string `json:"total_kib"`
		} `json:"memory"`
		RootFilesystem struct {
			Source  string `json:"source"`
			Type    string `json:"type"`
			Options string `json:"options"`
		} `json:"root_filesystem"`
		Devices struct {
			KVM bool `json:"kvm"`
			TUN bool `json:"tun"`
		} `json:"devices"`
	} `json:"host"`
	Software struct {
		Docker struct {
			ServerVersion string `json:"server_version"`
			StorageDriver string `json:"storage_driver"`
			CgroupVersion string `json:"cgroup_version"`
		} `json:"docker"`
		Repository struct {
			Revision string `json:"revision"`
			Dirty    bool   `json:"dirty"`
		} `json:"repository"`
		BenchmarkBinarySHA256 string `json:"benchmark_binary_sha256"`
		EngineLockSHA256      string `json:"engine_lock_sha256"`
	} `json:"software"`
}

type loadedMatrix struct {
	matrix     matrix
	hostBefore hostSnapshot
	hostAfter  hostSnapshot
}

type matrixCleanup struct {
	Attempts struct {
		NotRequired int `json:"not_required"`
		Attempted   int `json:"attempted"`
		Confirmed   int `json:"confirmed"`
		Failed      int `json:"failed"`
	} `json:"attempts"`
	Resources struct {
		Expected  int `json:"expected"`
		Confirmed int `json:"confirmed"`
		Failed    int `json:"failed"`
	} `json:"resources"`
}

type latency struct {
	Samples int     `json:"samples"`
	P50MS   float64 `json:"p50_ms"`
	P95MS   float64 `json:"p95_ms"`
	P99MS   float64 `json:"p99_ms"`
}

type matrixCase struct {
	Key                          string        `json:"key"`
	Outcome                      string        `json:"outcome"`
	Requested                    int           `json:"requested"`
	Scheduled                    int           `json:"scheduled"`
	Started                      int           `json:"started"`
	Completed                    int           `json:"completed"`
	Succeeded                    int           `json:"succeeded"`
	Failed                       int           `json:"failed"`
	LatencyCensored              int           `json:"latency_censored"`
	ObservedCompletionsPerSecond float64       `json:"observed_completions_per_second"`
	ScheduledLatency             latency       `json:"scheduled_latency"`
	ServiceLatency               latency       `json:"service_latency"`
	Cleanup                      matrixCleanup `json:"cleanup"`
}

// Evaluate validates two independently repeated matrix sets and applies a
// no-regression gate to every cell before checking the declared optimization
// targets. Percent values are expressed as 5 for five percent.
func Evaluate(config Config) (Result, error) {
	if len(config.BaselinePaths) < 2 || len(config.CandidatePaths) < 2 {
		return Result{}, errors.New("at least two baseline and two candidate matrices are required")
	}
	if len(config.Targets) == 0 {
		return Result{}, errors.New("at least one optimization target is required")
	}
	for name, value := range map[string]float64{
		"required improvement":       config.RequiredImprovement,
		"max latency regression":     config.MaxLatencyRegression,
		"max throughput loss":        config.MaxThroughputLoss,
		"absolute latency tolerance": config.AbsoluteToleranceMS,
	} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
			return Result{}, fmt.Errorf("%s must be between 0 and 100", name)
		}
	}
	for _, target := range config.Targets {
		if strings.TrimSpace(target.Case) == "" || !validMetric(target.Metric) {
			return Result{}, fmt.Errorf("invalid optimization target %q:%q", target.Case, target.Metric)
		}
	}

	baseline, err := loadSet("baseline", config.BaselinePaths)
	if err != nil {
		return Result{}, err
	}
	candidate, err := loadSet("candidate", config.CandidatePaths)
	if err != nil {
		return Result{}, err
	}
	if err := compatibleSets(baseline, candidate); err != nil {
		return Result{}, err
	}

	result := Result{
		SchemaVersion: 1, Decision: DecisionPromote,
		BaselineRevisions: revisions(baseline), CandidateRevisions: revisions(candidate),
		RequiredImprovement: config.RequiredImprovement, MaxLatencyRegression: config.MaxLatencyRegression,
		MaxThroughputLoss: config.MaxThroughputLoss, AbsoluteToleranceMS: config.AbsoluteToleranceMS,
	}
	caseKeys := sortedCaseKeys(baseline[0].matrix)
	for _, key := range caseKeys {
		for _, metric := range []Metric{MetricScheduledP95, MetricScheduledP99, MetricServiceP95, MetricServiceP99} {
			check := compareMetric(baseline, candidate, key, metric, "regression", config)
			result.Checks = append(result.Checks, check)
			if !check.Passed {
				result.Failures = append(result.Failures, check.Reason)
			}
		}
		if strings.HasSuffix(key, "-staggered") || strings.HasSuffix(key, "-burst") {
			check := compareMetric(baseline, candidate, key, MetricThroughput, "regression", config)
			result.Checks = append(result.Checks, check)
			if !check.Passed {
				result.Failures = append(result.Failures, check.Reason)
			}
		}
	}
	for _, target := range config.Targets {
		check := compareMetric(baseline, candidate, target.Case, target.Metric, "target", config)
		result.Checks = append(result.Checks, check)
		if !check.Passed {
			result.Failures = append(result.Failures, check.Reason)
		}
	}
	if len(result.Failures) > 0 {
		result.Decision = DecisionReject
	}
	return result, nil
}

func loadSet(label string, paths []string) ([]loadedMatrix, error) {
	result := make([]loadedMatrix, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s matrix %q: %w", label, path, err)
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		var value matrix
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode %s matrix %q: %w", label, path, err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				return nil, fmt.Errorf("decode %s matrix %q: trailing JSON", label, path)
			}
			return nil, fmt.Errorf("decode %s matrix %q: %w", label, path, err)
		}
		if err := validateMatrix(value); err != nil {
			return nil, fmt.Errorf("validate %s matrix %q: %w", label, path, err)
		}
		value.sourcePath = path
		hostBefore, err := loadHostSnapshot(value, value.HostMetadata.Before, "before")
		if err != nil {
			return nil, fmt.Errorf("validate %s matrix %q host evidence: %w", label, path, err)
		}
		hostAfter, err := loadHostSnapshot(value, value.HostMetadata.After, "after")
		if err != nil {
			return nil, fmt.Errorf("validate %s matrix %q host evidence: %w", label, path, err)
		}
		if !sameHost(hostBefore, hostAfter) || hostBefore.Software.BenchmarkBinarySHA256 != hostAfter.Software.BenchmarkBinarySHA256 {
			return nil, fmt.Errorf("validate %s matrix %q host evidence: stable host or benchmark binary identity changed during the matrix", label, path)
		}
		result = append(result, loadedMatrix{matrix: value, hostBefore: hostBefore, hostAfter: hostAfter})
	}
	for index := 1; index < len(result); index++ {
		if err := compatible(result[0], result[index], true); err != nil {
			return nil, fmt.Errorf("%s replicate %d: %w", label, index+1, err)
		}
	}
	return result, nil
}

func loadHostSnapshot(value matrix, name, expectedPhase string) (hostSnapshot, error) {
	if name == "" {
		return hostSnapshot{}, fmt.Errorf("host_metadata.%s is required", expectedPhase)
	}
	if filepath.IsAbs(name) || filepath.Clean(name) != name || strings.Contains(name, string(filepath.Separator)) {
		return hostSnapshot{}, fmt.Errorf("host_metadata.%s must be a plain relative file name", expectedPhase)
	}
	hostPath := filepath.Join(filepath.Dir(value.sourcePath), name)
	data, err := os.ReadFile(hostPath)
	if err != nil {
		return hostSnapshot{}, fmt.Errorf("read %q: %w", hostPath, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var host hostSnapshot
	if err := decoder.Decode(&host); err != nil {
		return hostSnapshot{}, fmt.Errorf("decode %q: %w", hostPath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return hostSnapshot{}, fmt.Errorf("decode %q: trailing or invalid JSON", hostPath)
	}
	if host.SchemaVersion != 1 || host.Phase != expectedPhase {
		return hostSnapshot{}, fmt.Errorf("host snapshot must be schema version 1 and phase %s", expectedPhase)
	}
	if host.Software.Repository.Dirty || host.Software.Repository.Revision != value.RuntimeRevision {
		return hostSnapshot{}, errors.New("host snapshot must be clean and bind the matrix runtime revision")
	}
	if !host.Host.Devices.KVM || !host.Host.Devices.TUN {
		return hostSnapshot{}, errors.New("host snapshot must confirm KVM and TUN")
	}
	requiredIdentity := []string{
		host.Host.OS.ID,
		host.Host.OS.Version,
		host.Host.KernelRelease,
		host.Host.Architecture,
		host.Host.Virtualization,
		host.Host.CPU.Model,
		host.Host.CPU.Sockets,
		host.Host.CPU.CoresPerSocket,
		host.Host.CPU.ThreadsPerCore,
		host.Host.CPU.Governors,
		host.Host.Memory.TotalKiB,
		host.Host.RootFilesystem.Source,
		host.Host.RootFilesystem.Type,
		host.Host.RootFilesystem.Options,
		host.Software.Docker.ServerVersion,
		host.Software.Docker.StorageDriver,
		host.Software.Docker.CgroupVersion,
		host.Software.BenchmarkBinarySHA256,
		host.Software.EngineLockSHA256,
	}
	for _, field := range requiredIdentity {
		if strings.TrimSpace(field) == "" {
			return hostSnapshot{}, errors.New("host snapshot is missing stable identity fields")
		}
	}
	return host, nil
}

func validateMatrix(value matrix) error {
	if value.SchemaVersion != matrixSchemaVersion {
		return fmt.Errorf("schema_version must be %d", matrixSchemaVersion)
	}
	if value.Outcome != "passed" || value.ExpectedCases < 1 || value.CompletedCases != value.ExpectedCases || value.PassedCases != value.ExpectedCases || value.FailedCases != 0 {
		return errors.New("matrix must be complete and passed")
	}
	if value.FailedAttempts != 0 || value.LatencyCensored != 0 || value.Cleanup.Attempts.Failed != 0 || value.Cleanup.Resources.Failed != 0 {
		return errors.New("matrix contains failed, censored, or unconfirmed-cleanup attempts")
	}
	if value.PlannedAttempts < 1 || value.RequestedAttempts != value.PlannedAttempts || value.ScheduledAttempts != value.RequestedAttempts ||
		value.StartedAttempts != value.RequestedAttempts || value.CompletedAttempts != value.RequestedAttempts || value.SuccessfulAttempts != value.RequestedAttempts {
		return errors.New("matrix attempt totals are missing or inconsistent")
	}
	if value.Cleanup.Attempts.NotRequired+value.Cleanup.Attempts.Attempted != value.RequestedAttempts ||
		value.Cleanup.Attempts.Confirmed != value.Cleanup.Attempts.Attempted ||
		value.Cleanup.Resources.Confirmed != value.Cleanup.Resources.Expected {
		return errors.New("matrix cleanup confirmation totals are missing or inconsistent")
	}
	if len(value.Cases) != value.ExpectedCases {
		return errors.New("matrix case count does not match expected_cases")
	}
	seen := make(map[string]struct{}, len(value.Cases))
	for _, current := range value.Cases {
		if current.Key == "" || current.Outcome != "passed" || current.Requested < 1 || current.Scheduled != current.Requested || current.Started != current.Requested ||
			current.Completed != current.Requested || current.Succeeded != current.Requested || current.Failed != 0 || current.LatencyCensored != 0 {
			return fmt.Errorf("case %q is incomplete", current.Key)
		}
		if current.Cleanup.Attempts.Failed != 0 || current.Cleanup.Resources.Failed != 0 {
			return fmt.Errorf("case %q has unconfirmed cleanup", current.Key)
		}
		if current.Cleanup.Attempts.NotRequired+current.Cleanup.Attempts.Attempted != current.Requested ||
			current.Cleanup.Attempts.Confirmed != current.Cleanup.Attempts.Attempted ||
			current.Cleanup.Resources.Confirmed != current.Cleanup.Resources.Expected {
			return fmt.Errorf("case %q cleanup confirmation totals are missing or inconsistent", current.Key)
		}
		if current.ScheduledLatency.Samples != current.Succeeded || current.ServiceLatency.Samples != current.Succeeded {
			return fmt.Errorf("case %q latency sample counts do not match successes", current.Key)
		}
		if _, duplicate := seen[current.Key]; duplicate {
			return fmt.Errorf("case %q is duplicated", current.Key)
		}
		seen[current.Key] = struct{}{}
	}
	return nil
}

func compatibleSets(baseline, candidate []loadedMatrix) error {
	if err := compatible(baseline[0], candidate[0], false); err != nil {
		return fmt.Errorf("candidate is not comparable with baseline: %w", err)
	}
	return nil
}

func compatible(a, b loadedMatrix, requireRevision bool) error {
	if a.matrix.Target != b.matrix.Target || a.matrix.EvidenceClass != b.matrix.EvidenceClass || a.matrix.CacheState != b.matrix.CacheState || a.matrix.BackendTemplate != b.matrix.BackendTemplate || a.matrix.Project != b.matrix.Project || !semanticJSONEqual(a.matrix.Configuration, b.matrix.Configuration) {
		return errors.New("target, evidence class, cache state, template, project, and configuration must match exactly")
	}
	if requireRevision && a.matrix.RuntimeRevision != b.matrix.RuntimeRevision {
		return errors.New("replicate runtime revisions must match")
	}
	if !sameHost(a.hostBefore, b.hostBefore) {
		return errors.New("captured host, Docker, or engine-lock identity differs")
	}
	if requireRevision && a.hostBefore.Software.BenchmarkBinarySHA256 != b.hostBefore.Software.BenchmarkBinarySHA256 {
		return errors.New("replicate benchmark binary digests differ")
	}
	aKeys, bKeys := sortedCaseKeys(a.matrix), sortedCaseKeys(b.matrix)
	if strings.Join(aKeys, "\x00") != strings.Join(bKeys, "\x00") {
		return errors.New("case sets differ")
	}
	return nil
}

func semanticJSONEqual(a, b json.RawMessage) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func sameHost(a, b hostSnapshot) bool {
	a.Software.Repository.Revision = ""
	b.Software.Repository.Revision = ""
	a.Software.BenchmarkBinarySHA256 = ""
	b.Software.BenchmarkBinarySHA256 = ""
	return a.Host == b.Host && a.Software == b.Software
}

func compareMetric(baseline, candidate []loadedMatrix, key string, metric Metric, purpose string, config Config) Check {
	base := metricValues(baseline, key, metric)
	next := metricValues(candidate, key, metric)
	check := Check{Case: key, Metric: metric, Purpose: purpose, BaselineSamples: base, CandidateSamples: next}
	if len(base) != len(baseline) || len(next) != len(candidate) {
		check.Reason = fmt.Sprintf("%s %s is missing from one or more matrices", key, metric)
		return check
	}
	check.BaselineAggregate = median(base)
	check.CandidateAggregate = median(next)
	if check.BaselineAggregate <= 0 || check.CandidateAggregate < 0 {
		check.Reason = fmt.Sprintf("%s %s contains a non-positive baseline or negative candidate", key, metric)
		return check
	}
	check.ChangePercent = roundPercent((check.CandidateAggregate/check.BaselineAggregate - 1) * 100)
	lowerIsBetter := metric != MetricThroughput
	if purpose == "target" {
		if lowerIsBetter {
			check.Passed = check.CandidateAggregate <= check.BaselineAggregate*(1-config.RequiredImprovement/100)
		} else {
			check.Passed = check.CandidateAggregate >= check.BaselineAggregate*(1+config.RequiredImprovement/100)
		}
		check.Reason = fmt.Sprintf("target %s %s changed %.3f%%; required improvement is %.3f%%", key, metric, check.ChangePercent, config.RequiredImprovement)
		return check
	}
	if lowerIsBetter {
		allowed := math.Max(check.BaselineAggregate*(1+config.MaxLatencyRegression/100), check.BaselineAggregate+config.AbsoluteToleranceMS)
		check.Passed = check.CandidateAggregate <= allowed
		check.Reason = fmt.Sprintf("guard %s %s changed %.3f%%; allowed latency regression is %.3f%% or %.3f ms", key, metric, check.ChangePercent, config.MaxLatencyRegression, config.AbsoluteToleranceMS)
	} else {
		check.Passed = check.CandidateAggregate >= check.BaselineAggregate*(1-config.MaxThroughputLoss/100)
		check.Reason = fmt.Sprintf("guard %s %s changed %.3f%%; allowed throughput loss is %.3f%%", key, metric, check.ChangePercent, config.MaxThroughputLoss)
	}
	return check
}

func metricValues(set []loadedMatrix, key string, metric Metric) []float64 {
	values := make([]float64, 0, len(set))
	for _, value := range set {
		for _, current := range value.matrix.Cases {
			if current.Key != key {
				continue
			}
			switch metric {
			case MetricScheduledP50:
				values = append(values, current.ScheduledLatency.P50MS)
			case MetricScheduledP95:
				values = append(values, current.ScheduledLatency.P95MS)
			case MetricScheduledP99:
				values = append(values, current.ScheduledLatency.P99MS)
			case MetricServiceP50:
				values = append(values, current.ServiceLatency.P50MS)
			case MetricServiceP95:
				values = append(values, current.ServiceLatency.P95MS)
			case MetricServiceP99:
				values = append(values, current.ServiceLatency.P99MS)
			case MetricThroughput:
				values = append(values, current.ObservedCompletionsPerSecond)
			}
			break
		}
	}
	return values
}

func validMetric(value Metric) bool {
	switch value {
	case MetricScheduledP50, MetricScheduledP95, MetricScheduledP99, MetricServiceP50, MetricServiceP95, MetricServiceP99, MetricThroughput:
		return true
	default:
		return false
	}
}

func revisions(set []loadedMatrix) []string {
	result := make([]string, len(set))
	for index := range set {
		result[index] = set[index].matrix.RuntimeRevision
	}
	return result
}

func sortedCaseKeys(value matrix) []string {
	result := make([]string, len(value.Cases))
	for index := range value.Cases {
		result[index] = value.Cases[index].Key
	}
	sort.Strings(result)
	return result
}

func median(input []float64) float64 {
	values := append([]float64(nil), input...)
	sort.Float64s(values)
	middle := len(values) / 2
	if len(values)%2 == 1 {
		return values[middle]
	}
	return (values[middle-1] + values[middle]) / 2
}

func roundPercent(value float64) float64 { return math.Round(value*1000) / 1000 }
