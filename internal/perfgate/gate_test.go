package perfgate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEvaluatePromotesRepeatedCompatibleImprovement(t *testing.T) {
	baselineA := writeMatrix(t, testMatrix("base", 100, 120, 10))
	baselineB := writeMatrix(t, testMatrix("base", 102, 118, 9.8))
	candidateA := writeMatrix(t, testMatrix("candidate", 90, 115, 10.2))
	candidateB := writeMatrix(t, testMatrix("candidate", 92, 116, 10.1))

	result, err := Evaluate(Config{
		BaselinePaths: []string{baselineA, baselineB}, CandidatePaths: []string{candidateA, candidateB},
		Targets: []Target{{Case: "tti-sequential", Metric: MetricScheduledP50}}, RequiredImprovement: 5,
		MaxLatencyRegression: 5, MaxThroughputLoss: 5, AbsoluteToleranceMS: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionPromote || len(result.Failures) != 0 {
		t.Fatalf("decision=%s failures=%v", result.Decision, result.Failures)
	}
}

func TestEvaluateRejectsTailRegressionEvenWhenTargetImproves(t *testing.T) {
	baselineA := writeMatrix(t, testMatrix("base", 100, 120, 10))
	baselineB := writeMatrix(t, testMatrix("base", 100, 120, 10))
	candidateOne := testMatrix("candidate", 80, 150, 10)
	candidateA := writeMatrix(t, candidateOne)
	candidateB := writeMatrix(t, candidateOne)

	result, err := Evaluate(Config{
		BaselinePaths: []string{baselineA, baselineB}, CandidatePaths: []string{candidateA, candidateB},
		Targets: []Target{{Case: "tti-sequential", Metric: MetricScheduledP50}}, RequiredImprovement: 5,
		MaxLatencyRegression: 5, MaxThroughputLoss: 5, AbsoluteToleranceMS: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != DecisionReject || len(result.Failures) == 0 {
		t.Fatalf("decision=%s failures=%v", result.Decision, result.Failures)
	}
}

func TestEvaluateRejectsFailedMatrixAndConfigurationDrift(t *testing.T) {
	good := testMatrix("base", 100, 120, 10)
	failed := good
	failed.Outcome = "failed"
	if _, err := Evaluate(Config{
		BaselinePaths:  []string{writeMatrix(t, good), writeMatrix(t, failed)},
		CandidatePaths: []string{writeMatrix(t, good), writeMatrix(t, good)},
		Targets:        []Target{{Case: "tti-sequential", Metric: MetricScheduledP50}},
	}); err == nil {
		t.Fatal("failed matrix was accepted")
	}

	drifted := testMatrix("candidate", 90, 115, 10)
	drifted.Configuration = json.RawMessage(`{"scenarios":["different"]}`)
	if _, err := Evaluate(Config{
		BaselinePaths:  []string{writeMatrix(t, good), writeMatrix(t, good)},
		CandidatePaths: []string{writeMatrix(t, drifted), writeMatrix(t, drifted)},
		Targets:        []Target{{Case: "tti-sequential", Metric: MetricScheduledP50}},
	}); err == nil {
		t.Fatal("configuration drift was accepted")
	}
}

func TestEvaluateRejectsHostDriftAndDirtyEvidence(t *testing.T) {
	baselineA := writeMatrix(t, testMatrix("base", 100, 120, 10))
	baselineB := writeMatrix(t, testMatrix("base", 100, 120, 10))
	candidateA := writeMatrixWithHost(t, testMatrix("candidate", 90, 115, 10), func(host *hostSnapshot) {
		host.Host.CPU.Model = "different CPU"
	})
	candidateB := writeMatrixWithHost(t, testMatrix("candidate", 90, 115, 10), func(host *hostSnapshot) {
		host.Host.CPU.Model = "different CPU"
	})
	if _, err := Evaluate(Config{
		BaselinePaths: []string{baselineA, baselineB}, CandidatePaths: []string{candidateA, candidateB},
		Targets: []Target{{Case: "tti-sequential", Metric: MetricScheduledP50}},
	}); err == nil {
		t.Fatal("host drift was accepted")
	}

	dirty := writeMatrixWithHost(t, testMatrix("base", 100, 120, 10), func(host *hostSnapshot) {
		host.Software.Repository.Dirty = true
	})
	if _, err := Evaluate(Config{
		BaselinePaths: []string{baselineA, dirty}, CandidatePaths: []string{baselineA, baselineB},
		Targets: []Target{{Case: "tti-sequential", Metric: MetricScheduledP50}},
	}); err == nil {
		t.Fatal("dirty host evidence was accepted")
	}
}

func TestEvaluateRejectsIncompleteHostIdentity(t *testing.T) {
	complete := testMatrix("base", 100, 120, 10)
	missing := writeMatrixWithHost(t, complete, func(host *hostSnapshot) {
		host.Software.Docker.StorageDriver = ""
	})
	valid := writeMatrix(t, complete)
	if _, err := Evaluate(Config{
		BaselinePaths:  []string{valid, missing},
		CandidatePaths: []string{valid, valid},
		Targets:        []Target{{Case: "tti-sequential", Metric: MetricScheduledP50}},
	}); err == nil {
		t.Fatal("host evidence with an incomplete stable identity was accepted")
	}
}

func TestEvaluateRejectsMissingAndInconsistentCleanupEvidence(t *testing.T) {
	valid := testMatrix("base", 100, 120, 10)
	missing := valid
	missing.Cleanup.Attempts = struct {
		NotRequired int `json:"not_required"`
		Attempted   int `json:"attempted"`
		Confirmed   int `json:"confirmed"`
		Failed      int `json:"failed"`
	}{}
	if _, err := Evaluate(Config{
		BaselinePaths:  []string{writeMatrix(t, valid), writeMatrix(t, missing)},
		CandidatePaths: []string{writeMatrix(t, valid), writeMatrix(t, valid)},
		Targets:        []Target{{Case: "tti-sequential", Metric: MetricScheduledP50}},
	}); err == nil {
		t.Fatal("matrix without cleanup proof was accepted")
	}

	inconsistent := testMatrix("candidate", 90, 115, 10)
	inconsistent.Cases[0].Cleanup.Resources.Expected = 1
	if _, err := Evaluate(Config{
		BaselinePaths:  []string{writeMatrix(t, valid), writeMatrix(t, valid)},
		CandidatePaths: []string{writeMatrix(t, inconsistent), writeMatrix(t, inconsistent)},
		Targets:        []Target{{Case: "tti-sequential", Metric: MetricScheduledP50}},
	}); err == nil {
		t.Fatal("matrix with unconfirmed resource cleanup was accepted")
	}
}

func TestEvaluateRejectsPassedCaseThatClaimsCleanupWasNotRequired(t *testing.T) {
	valid := testMatrix("base", 100, 120, 10)
	forged := testMatrix("candidate", 90, 115, 10)
	forged.Cases[0].Cleanup.Attempts.NotRequired = 1
	forged.Cases[0].Cleanup.Attempts.Attempted--
	forged.Cases[0].Cleanup.Attempts.Confirmed--
	forged.Cases[0].Cleanup.Resources.Expected--
	forged.Cases[0].Cleanup.Resources.Confirmed--

	if _, err := Evaluate(Config{
		BaselinePaths:  []string{writeMatrix(t, valid), writeMatrix(t, valid)},
		CandidatePaths: []string{writeMatrix(t, forged), writeMatrix(t, forged)},
		Targets:        []Target{{Case: "tti-sequential", Metric: MetricScheduledP50}},
	}); err == nil {
		t.Fatal("passed case without confirmed cleanup was accepted")
	}
}

func TestEvaluateRejectsAggregateTotalsThatDoNotMatchCases(t *testing.T) {
	valid := testMatrix("base", 100, 120, 10)
	forged := testMatrix("candidate", 90, 115, 10)
	forged.RequestedAttempts++
	forged.PlannedAttempts++
	forged.ScheduledAttempts++
	forged.StartedAttempts++
	forged.CompletedAttempts++
	forged.SuccessfulAttempts++
	forged.Cleanup.Attempts.Attempted++
	forged.Cleanup.Attempts.Confirmed++
	forged.Cleanup.Resources.Expected++
	forged.Cleanup.Resources.Confirmed++

	if _, err := Evaluate(Config{
		BaselinePaths:  []string{writeMatrix(t, valid), writeMatrix(t, valid)},
		CandidatePaths: []string{writeMatrix(t, forged), writeMatrix(t, forged)},
		Targets:        []Target{{Case: "tti-sequential", Metric: MetricScheduledP50}},
	}); err == nil {
		t.Fatal("matrix totals inconsistent with case evidence were accepted")
	}
}

func testMatrix(revision string, p50, p99, throughput float64) matrix {
	result := matrix{
		SchemaVersion: matrixSchemaVersion, Target: "host-a", RuntimeRevision: revision,
		EvidenceClass: "single-host-linux-kvm", CacheState: "cached-template", BackendTemplate: "base", Project: "benchmark",
		Configuration: json.RawMessage(`{"scenarios":["tti","warm-exec"],"sequential_runs":100,"burst_runs":24}`),
		ExpectedCases: 2, CompletedCases: 2, PassedCases: 2, Outcome: "passed",
		PlannedAttempts: 124, RequestedAttempts: 124, ScheduledAttempts: 124,
		StartedAttempts: 124, CompletedAttempts: 124, SuccessfulAttempts: 124,
	}
	result.Cleanup.Attempts.Attempted = 124
	result.Cleanup.Attempts.Confirmed = 124
	result.Cleanup.Resources.Expected = 124
	result.Cleanup.Resources.Confirmed = 124
	result.Cases = []matrixCase{
		passingCase("tti-sequential", p50, p99, 0, 100),
		passingCase("warm-exec-burst", p50/2, p99/2, throughput, 24),
	}
	return result
}

func passingCase(key string, p50, p99, throughput float64, runs int) matrixCase {
	value := matrixCase{Key: key, Outcome: "passed", Requested: runs, Scheduled: runs, Started: runs, Completed: runs, Succeeded: runs, ObservedCompletionsPerSecond: throughput}
	value.Cleanup.Attempts.Attempted = runs
	value.Cleanup.Attempts.Confirmed = runs
	value.Cleanup.Resources.Expected = runs
	value.Cleanup.Resources.Confirmed = runs
	value.ScheduledLatency = latency{Samples: runs, P50MS: p50, P95MS: p99 * 0.9, P99MS: p99}
	value.ServiceLatency = latency{Samples: runs, P50MS: p50 * 0.9, P95MS: p99 * 0.8, P99MS: p99 * 0.9}
	return value
}

func writeMatrix(t *testing.T, value matrix) string {
	return writeMatrixWithHost(t, value, nil)
}

func writeMatrixWithHost(t *testing.T, value matrix, mutate func(*hostSnapshot)) string {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "summary.json")
	value.HostMetadata = hostMetadata{Before: "host-before.json", After: "host-after.json"}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	host := testHost(value.RuntimeRevision)
	if mutate != nil {
		mutate(&host)
	}
	hostData, err := json.Marshal(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "host-before.json"), hostData, 0o600); err != nil {
		t.Fatal(err)
	}
	host.Phase = "after"
	hostData, err = json.Marshal(host)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "host-after.json"), hostData, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testHost(revision string) hostSnapshot {
	var host hostSnapshot
	host.SchemaVersion = 1
	host.Phase = "before"
	host.Host.OS.ID = "ubuntu"
	host.Host.OS.Version = "24.04"
	host.Host.KernelRelease = "6.8.0"
	host.Host.Architecture = "x86_64"
	host.Host.Virtualization = "none"
	host.Host.CPU.Model = "test CPU"
	host.Host.CPU.Sockets = "1"
	host.Host.CPU.CoresPerSocket = "8"
	host.Host.CPU.ThreadsPerCore = "2"
	host.Host.CPU.Governors = "performance"
	host.Host.Memory.TotalKiB = "33554432"
	host.Host.RootFilesystem.Source = "/dev/sda1"
	host.Host.RootFilesystem.Type = "ext4"
	host.Host.RootFilesystem.Options = "rw,relatime"
	host.Host.Devices.KVM = true
	host.Host.Devices.TUN = true
	host.Software.Docker.ServerVersion = "28.0.0"
	host.Software.Docker.StorageDriver = "overlay2"
	host.Software.Docker.CgroupVersion = "2"
	host.Software.Repository.Revision = revision
	host.Software.BenchmarkBinarySHA256 = revision + "-binary"
	host.Software.EngineLockSHA256 = "engine-lock"
	return host
}
