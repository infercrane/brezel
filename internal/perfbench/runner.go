package perfbench

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"
)

type attempt struct {
	sample            Sample
	sandboxID         string
	checkpointID      string
	workspaceID       string
	previewURL        string
	verificationPath  string
	verificationData  []byte
	resources         []attemptResource
	scheduledInstant  time.Time
	completedInstant  time.Time
	setupReadyInstant time.Time
}

type attemptResource struct {
	kind        string
	id          string
	sampleIndex int
}

// Run executes one scenario and scheduling mode through the public runtime API.
// It always returns the evidence collected so far. A non-nil error means at
// least one scheduled operation or cleanup did not complete successfully.
func Run(ctx context.Context, config Config) (Report, error) {
	if config.IOBytes == 0 {
		config.IOBytes = 1 << 20
	}
	if config.PreviewPort == 0 {
		config.PreviewPort = 8080
	}
	base, err := config.validate()
	if err != nil {
		return Report{}, err
	}
	client := newAPIClient(config, base)
	connectionReuseSource := "default benchmark transport with HTTP keep-alives enabled"
	if config.Client != nil {
		connectionReuseSource = "custom HTTP client; connection reuse is not asserted by the harness"
	}
	report := Report{
		SchemaVersion: 3,
		StartedAt:     time.Now().UTC(),
		Boundary:      boundaryFor(config.Scenario),
		Scenario:      config.Scenario,
		Mode:          config.Mode,
		Runs:          config.Runs,
		MaxInFlight:   config.MaxInFlight,
		Metadata: Metadata{
			Target:                    config.Target,
			RuntimeRevision:           config.RuntimeRevision,
			EvidenceClass:             config.EvidenceClass,
			CacheState:                config.CacheState,
			BackendTemplate:           config.BackendTemplate,
			ProjectID:                 config.ProjectID,
			APITransport:              base.Scheme,
			HTTPConnectionReuse:       config.Client == nil,
			HTTPConnectionReuseSource: connectionReuseSource,
			CacheStateSource:          "operator-declared; the harness does not clear or mutate host caches",
			PercentileMethod:          "nearest-rank; successful and failed observed latencies are summarized separately without trimming",
			LatencyDefinition:         "scheduled arrival through verified result; includes client-side admission delay to avoid coordinated omission",
			ServiceLatencyDefinition:  "request initiation through verified result; excludes client-side admission delay",
			Command:                   commandFor(config),
			CommandSuccessDefinition:  commandSuccessFor(config),
			ScenarioSetupDefinition:   setupFor(config),
			VerificationDefinition:    verificationFor(config),
			IOBytes:                   scenarioIOBytes(config),
			PreviewPort:               scenarioPreviewPort(config),
			ClientOS:                  runtime.GOOS,
			ClientArch:                runtime.GOARCH,
			ClientGoVersion:           runtime.Version(),
		},
	}
	if config.Mode == ModeStaggered {
		report.StaggerNS = config.StaggerInterval.Nanoseconds()
	}

	prepareCtx, cancelPrepare := context.WithTimeout(ctx, config.AttemptTimeout)
	environmentRevision, preparationRequestID, err := client.createEnvironment(prepareCtx, config.BackendTemplate, config.RuntimeRevision)
	cancelPrepare()
	report.Metadata.PreparationRequestID = preparationRequestID
	if err != nil {
		report.PreparationErrorCategory, report.PreparationErrorDetail = errorFields(err)
		report.FinishedAt = time.Now().UTC()
		return report, fmt.Errorf("prepare benchmark environment: %w", err)
	}
	report.Metadata.EnvironmentRevision = environmentRevision

	attempts := make([]*attempt, config.Runs)
	for index := range attempts {
		attempts[index] = &attempt{sample: Sample{
			Index: index + 1, CleanupSucceeded: true, ResourceOutcome: "none",
		}}
	}

	switch config.Mode {
	case ModeSequential:
		runSequential(ctx, config, client, environmentRevision, attempts)
	case ModeStaggered, ModeBurst:
		runConcurrent(ctx, config, client, environmentRevision, attempts)
	}

	report.Samples = make([]Sample, len(attempts))
	for index, value := range attempts {
		report.Samples[index] = value.sample
	}
	report.FinishedAt = time.Now().UTC()
	report.Summary = summarize(attempts, config.Mode)
	if report.Summary.Failed > 0 || report.Summary.CleanupFailed > 0 {
		return report, fmt.Errorf("benchmark incomplete: %d measured failures and %d cleanup failures", report.Summary.Failed, report.Summary.CleanupFailed)
	}
	return report, nil
}

func runSequential(ctx context.Context, config Config, client *apiClient, environmentRevision string, attempts []*attempt) {
	for index, value := range attempts {
		if ctx.Err() != nil {
			markFailure(&value.sample, "schedule", ctx.Err())
			continue
		}
		if config.Scenario != ScenarioTTI {
			setupAttempt(ctx, config, client, environmentRevision, value)
		}
		if value.sample.ErrorCategory == "" {
			value.scheduledInstant = time.Now()
			value.sample.ScheduledAt = value.scheduledInstant.UTC()
			measureAttempt(ctx, config, client, environmentRevision, value)
		}
		cleanupAttempt(config, client, value)
		if !value.sample.CleanupSucceeded {
			markUnscheduledCleanupRisk(attempts[index+1:])
			return
		}
	}
}

func runConcurrent(ctx context.Context, config Config, client *apiClient, environmentRevision string, attempts []*attempt) {
	if config.Scenario != ScenarioTTI {
		for index, value := range attempts {
			if ctx.Err() != nil {
				markFailure(&value.sample, "setup", ctx.Err())
				continue
			}
			setupAttempt(ctx, config, client, environmentRevision, value)
			if value.sample.ResourceOutcome == "unknown" {
				markPreparedCleanupRisk(attempts[:index])
				markUnscheduledCleanupRisk(attempts[index+1:])
				cleanupConcurrent(config, client, attempts)
				return
			}
		}
	}

	base := time.Now().Add(50 * time.Millisecond)
	semaphore := make(chan struct{}, config.MaxInFlight)
	var workers sync.WaitGroup
	for index, value := range attempts {
		if value.sample.ErrorCategory != "" {
			continue
		}
		scheduled := base
		if config.Mode == ModeStaggered {
			scheduled = base.Add(time.Duration(index) * config.StaggerInterval)
		}
		value.sample.ScheduledAt = scheduled.UTC()
		value.scheduledInstant = scheduled
		workers.Add(1)
		go func(current *attempt, start time.Time) {
			defer workers.Done()
			if err := waitUntil(ctx, start); err != nil {
				markScheduledFailure(current, err)
				return
			}
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				markScheduledFailure(current, ctx.Err())
				return
			}
			measureAttempt(ctx, config, client, environmentRevision, current)
		}(value, scheduled)
	}
	workers.Wait()

	// Cleanup begins only after every timed operation has completed, so cleanup
	// traffic cannot improve or degrade another sample in the same run.
	cleanupConcurrent(config, client, attempts)
}

func markPreparedCleanupRisk(attempts []*attempt) {
	for _, value := range attempts {
		if value.sample.ErrorCategory == "" {
			markFailure(&value.sample, "cleanup_guard", failure("cleanup_risk", "measurement skipped after an earlier mutation returned an unknown resource outcome"))
		}
	}
}

func markUnscheduledCleanupRisk(attempts []*attempt) {
	for _, value := range attempts {
		if value.sample.ErrorCategory == "" {
			markFailure(&value.sample, "cleanup_guard", failure("cleanup_risk", "attempt not started after cleanup could not be confirmed"))
		}
	}
}

func setupAttempt(parent context.Context, config Config, client *apiClient, environmentRevision string, value *attempt) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, config.AttemptTimeout)
	defer cancel()

	if config.Scenario == ScenarioWorkspaceIO {
		workspaceID, requestID, err := client.createWorkspace(ctx)
		appendRequestID(&value.sample, requestID)
		value.workspaceID = workspaceID
		recordMutationResource(value, "workspace", workspaceID, err)
		if err != nil {
			markFailure(&value.sample, "setup", err)
			value.sample.Phases.SetupNS = time.Since(started).Nanoseconds()
			return
		}
	}

	createStarted := time.Now()
	var sandboxID, requestID string
	var err error
	if config.Scenario == ScenarioWorkspaceIO {
		sandboxID, requestID, err = client.createSandboxWithWorkspace(ctx, environmentRevision, value.workspaceID)
	} else {
		sandboxID, requestID, err = client.createSandbox(ctx, environmentRevision)
	}
	value.sample.Phases.CreateNS = time.Since(createStarted).Nanoseconds()
	appendRequestID(&value.sample, requestID)
	value.sandboxID = sandboxID
	recordMutationResource(value, "sandbox", sandboxID, err)
	if err != nil {
		markFailure(&value.sample, "setup", err)
		value.sample.Phases.SetupNS = time.Since(started).Nanoseconds()
		return
	}

	switch config.Scenario {
	case ScenarioWarmExec, ScenarioResume:
		primeStarted := time.Now()
		requestID, err = client.runNonce(ctx, sandboxID, randomID("prime"))
		value.sample.Phases.PrimeCommandNS = time.Since(primeStarted).Nanoseconds()
		appendRequestID(&value.sample, requestID)
		if err != nil {
			markFailure(&value.sample, "setup", err)
			break
		}
	case ScenarioFilesystemCheckpoint, ScenarioFilesystemRestore:
		value.verificationPath = "/tmp/perfbench-filesystem-state"
		value.verificationData = []byte(randomID("filesystem-state"))
		writeStarted := time.Now()
		requestID, err = client.writeFile(ctx, sandboxID, value.verificationPath, value.verificationData)
		value.sample.Phases.FileWriteNS = time.Since(writeStarted).Nanoseconds()
		appendRequestID(&value.sample, requestID)
		if err != nil {
			markFailure(&value.sample, "setup", err)
			break
		}
		if config.Scenario == ScenarioFilesystemRestore {
			checkpointStarted := time.Now()
			value.checkpointID, requestID, err = client.createFilesystemCheckpoint(ctx, sandboxID)
			value.sample.Phases.CheckpointNS = time.Since(checkpointStarted).Nanoseconds()
			appendRequestID(&value.sample, requestID)
			recordMutationResource(value, "checkpoint", value.checkpointID, err)
			if err != nil {
				markFailure(&value.sample, "setup", err)
			}
		}
	case ScenarioPreviewFirstByte, ScenarioPreviewWarm:
		value.verificationData = []byte(randomID("preview-body"))
		serverScript := `mkdir -p /tmp/perfbench-preview && printf '%s' "$1" > /tmp/perfbench-preview/index.html && busybox httpd -p "$2" -h /tmp/perfbench-preview && printf '%s' "$1"`
		primeStarted := time.Now()
		requestID, err = client.runExpected(ctx, sandboxID, []string{"/bin/sh", "-lc", serverScript, "perfbench", string(value.verificationData), strconv.Itoa(int(config.PreviewPort))}, string(value.verificationData))
		value.sample.Phases.PrimeCommandNS = time.Since(primeStarted).Nanoseconds()
		appendRequestID(&value.sample, requestID)
		if err != nil {
			markFailure(&value.sample, "setup", err)
			break
		}
		if config.Scenario == ScenarioPreviewWarm {
			leaseStarted := time.Now()
			value.previewURL, requestID, err = client.createPortLease(ctx, sandboxID, config.PreviewPort)
			value.sample.Phases.PortLeaseNS = time.Since(leaseStarted).Nanoseconds()
			appendRequestID(&value.sample, requestID)
			if err != nil {
				markFailure(&value.sample, "setup", err)
				break
			}
			requestID, _, _, err = client.previewExact(ctx, value.previewURL, value.verificationData)
			appendRequestID(&value.sample, requestID)
			if err != nil {
				markFailure(&value.sample, "setup", err)
			}
		}
	case ScenarioWorkspaceIO:
		value.verificationPath = "/workspace/perfbench-io.bin"
		value.verificationData = benchmarkPayload(config.IOBytes, value.sample.Index)
		primeStarted := time.Now()
		requestID, err = client.runNonce(ctx, sandboxID, randomID("workspace-prime"))
		value.sample.Phases.PrimeCommandNS = time.Since(primeStarted).Nanoseconds()
		appendRequestID(&value.sample, requestID)
		if err != nil {
			markFailure(&value.sample, "setup", err)
		}
	}

	if value.sample.ErrorCategory == "" && config.Scenario == ScenarioResume {
		pauseStarted := time.Now()
		requestID, err = client.action(ctx, sandboxID, "pause", "standby")
		value.sample.Phases.PauseNS = time.Since(pauseStarted).Nanoseconds()
		appendRequestID(&value.sample, requestID)
		if err != nil {
			markFailure(&value.sample, "setup", err)
		}
	}
	value.sample.Phases.SetupNS = time.Since(started).Nanoseconds()
	if value.sample.ErrorCategory == "" {
		value.setupReadyInstant = time.Now()
	}
}

func measureAttempt(parent context.Context, config Config, client *apiClient, environmentRevision string, value *attempt) {
	ctx, cancel := context.WithTimeout(parent, config.AttemptTimeout)
	defer cancel()
	startedInstant := time.Now()
	value.sample.StartedAt = startedInstant.UTC()
	if !value.scheduledInstant.IsZero() {
		value.sample.ScheduleDelayNS = max(0, startedInstant.Sub(value.scheduledInstant).Nanoseconds())
	}
	if !value.setupReadyInstant.IsZero() {
		value.sample.ReadyAgeNS = max(0, startedInstant.Sub(value.setupReadyInstant).Nanoseconds())
	}
	started := startedInstant

	if config.Scenario == ScenarioTTI {
		createStarted := time.Now()
		sandboxID, requestID, err := client.createSandbox(ctx, environmentRevision)
		value.sample.Phases.CreateNS = time.Since(createStarted).Nanoseconds()
		appendRequestID(&value.sample, requestID)
		value.sandboxID = sandboxID
		recordMutationResource(value, "sandbox", sandboxID, err)
		if err != nil {
			markFailure(&value.sample, "measure", err)
			finishMeasurement(value, started)
			return
		}
	}

	if config.Scenario == ScenarioResume {
		resumeStarted := time.Now()
		requestID, err := client.action(ctx, value.sandboxID, "resume", "running")
		value.sample.Phases.ResumeNS = time.Since(resumeStarted).Nanoseconds()
		appendRequestID(&value.sample, requestID)
		if err != nil {
			markFailure(&value.sample, "measure", err)
			finishMeasurement(value, started)
			return
		}
	}

	var requestID string
	var err error
	switch config.Scenario {
	case ScenarioTTI, ScenarioWarmExec, ScenarioResume:
		commandStarted := time.Now()
		requestID, err = client.runNonce(ctx, value.sandboxID, randomID("measured"))
		value.sample.Phases.FirstCommandNS = time.Since(commandStarted).Nanoseconds()
		appendRequestID(&value.sample, requestID)
	case ScenarioFilesystemCheckpoint:
		checkpointStarted := time.Now()
		value.checkpointID, requestID, err = client.createFilesystemCheckpoint(ctx, value.sandboxID)
		value.sample.Phases.CheckpointNS = time.Since(checkpointStarted).Nanoseconds()
		appendRequestID(&value.sample, requestID)
		recordMutationResource(value, "checkpoint", value.checkpointID, err)
	case ScenarioFilesystemRestore:
		restoreStarted := time.Now()
		var restoredID string
		restoredID, requestID, err = client.createSandboxFromCheckpoint(ctx, value.checkpointID)
		value.sample.Phases.RestoreNS = time.Since(restoreStarted).Nanoseconds()
		appendRequestID(&value.sample, requestID)
		recordMutationResource(value, "sandbox", restoredID, err)
		if err == nil {
			commandStarted := time.Now()
			requestID, err = client.runExpected(ctx, restoredID, []string{"/bin/sh", "-lc", `cat "$1"`, "perfbench", value.verificationPath}, string(value.verificationData))
			value.sample.Phases.FirstCommandNS = time.Since(commandStarted).Nanoseconds()
			appendRequestID(&value.sample, requestID)
		}
	case ScenarioPreviewFirstByte, ScenarioPreviewWarm:
		if config.Scenario == ScenarioPreviewFirstByte {
			leaseStarted := time.Now()
			value.previewURL, requestID, err = client.createPortLease(ctx, value.sandboxID, config.PreviewPort)
			value.sample.Phases.PortLeaseNS = time.Since(leaseStarted).Nanoseconds()
			appendRequestID(&value.sample, requestID)
		}
		if err == nil {
			previewStarted := time.Now()
			var firstByte, complete time.Duration
			requestID, firstByte, complete, err = client.previewExact(ctx, value.previewURL, value.verificationData)
			appendRequestID(&value.sample, requestID)
			value.sample.Phases.PreviewFirstByteNS = firstByte.Nanoseconds()
			value.sample.Phases.PreviewCompleteNS = complete.Nanoseconds()
			if err == nil {
				value.sample.Success = true
				finishMeasurementAt(value, started, previewStarted.Add(firstByte))
				return
			}
		}
	case ScenarioWorkspaceIO:
		writeStarted := time.Now()
		requestID, err = client.writeFile(ctx, value.sandboxID, value.verificationPath, value.verificationData)
		value.sample.Phases.FileWriteNS = time.Since(writeStarted).Nanoseconds()
		appendRequestID(&value.sample, requestID)
		if err == nil {
			value.sample.BytesWritten = int64(len(value.verificationData))
			readStarted := time.Now()
			requestID, err = client.readExactFile(ctx, value.sandboxID, value.verificationPath, value.verificationData)
			value.sample.Phases.FileReadNS = time.Since(readStarted).Nanoseconds()
			appendRequestID(&value.sample, requestID)
			if err == nil {
				value.sample.BytesRead = int64(len(value.verificationData))
			}
		}
	}
	if err != nil {
		markFailure(&value.sample, "measure", err)
		finishMeasurement(value, started)
		return
	}
	value.sample.Success = true
	finishMeasurement(value, started)
}

func finishMeasurement(value *attempt, started time.Time) {
	finishMeasurementAt(value, started, time.Now())
}

func finishMeasurementAt(value *attempt, started, completed time.Time) {
	value.completedInstant = completed
	value.sample.MeasuredNS = value.completedInstant.Sub(started).Nanoseconds()
	value.sample.ObservedNS = value.sample.ScheduleDelayNS + value.sample.MeasuredNS
	value.sample.CompletedAt = value.completedInstant.UTC()
}

func recordMutationResource(value *attempt, kind, id string, err error) {
	if id != "" {
		trackResource(value, kind, id)
		return
	}
	if err == nil || createOutcomeIsDefinitelyEmpty(err) {
		return
	}
	value.sample.CleanupResources = append(value.sample.CleanupResources, CleanupResource{
		Kind: kind, Outcome: "unknown", ErrorCategory: "unknown_create_outcome",
		ErrorDetail: kind + " create outcome is unknown and no resource identity was returned",
	})
	value.sample.CleanupSucceeded = false
	value.sample.ResourceOutcome = "unknown"
	if value.sample.CleanupErrorCategory == "" {
		value.sample.CleanupErrorCategory = "unknown_create_outcome"
		value.sample.CleanupError = kind + " create outcome is unknown and no resource identity was returned"
	}
}

func trackResource(value *attempt, kind, id string) {
	for _, resource := range value.resources {
		if resource.kind == kind && resource.id == id {
			return
		}
	}
	value.sample.CleanupResources = append(value.sample.CleanupResources, CleanupResource{
		Kind: kind, IdentityKnown: true, Outcome: "known",
	})
	value.resources = append(value.resources, attemptResource{
		kind: kind, id: id, sampleIndex: len(value.sample.CleanupResources) - 1,
	})
	value.sample.CleanupSucceeded = false
	value.sample.ResourceOutcome = "known"
}

func cleanupAttempt(config Config, client *apiClient, value *attempt) {
	if len(value.resources) == 0 {
		if value.sample.ResourceOutcome == "none" {
			value.sample.CleanupSucceeded = true
		}
		return
	}
	started := time.Now()
	resources := append([]attemptResource(nil), value.resources...)
	sort.SliceStable(resources, func(i, j int) bool {
		return cleanupPriority(resources[i].kind) < cleanupPriority(resources[j].kind)
	})
	for _, resource := range resources {
		cleanupOneResource(config, client, value, resource)
	}
	value.sample.Phases.CleanupNS = time.Since(started).Nanoseconds()
	refreshCleanupAggregate(value)
}

func cleanupPriority(kind string) int {
	switch kind {
	case "sandbox":
		return 0
	case "checkpoint":
		return 1
	case "workspace":
		return 2
	default:
		return 3
	}
}

func cleanupOneResource(config Config, client *apiClient, value *attempt, resource attemptResource) {
	result := &value.sample.CleanupResources[resource.sampleIndex]
	ctx, cancel := context.WithTimeout(context.Background(), config.CleanupTimeout)
	defer cancel()
	idempotency := randomID("perfbench-delete-" + resource.kind)
	var lastErr error
	for attemptNumber := 1; attemptNumber <= 3; attemptNumber++ {
		result.Attempts = attemptNumber
		value.sample.CleanupAttempts++
		var requestID string
		var absent bool
		var err error
		switch resource.kind {
		case "sandbox":
			requestID, absent, err = client.deleteSandbox(ctx, resource.id, idempotency)
		case "checkpoint":
			requestID, absent, err = client.deleteCheckpoint(ctx, resource.id, idempotency)
		case "workspace":
			requestID, absent, err = client.deleteWorkspace(ctx, resource.id, idempotency)
		default:
			err = failure("cleanup_state", "unsupported cleanup resource kind")
		}
		appendRequestID(&value.sample, requestID)
		if err == nil {
			result.Outcome = "deleted"
			if absent {
				result.Outcome = "absent"
			}
			result.ErrorCategory = ""
			result.ErrorDetail = ""
			return
		}
		lastErr = err
		if attemptNumber < 3 {
			if err := waitUntil(ctx, time.Now().Add(time.Duration(attemptNumber)*100*time.Millisecond)); err != nil {
				lastErr = err
				break
			}
		}
	}
	result.Outcome = "cleanup_failed"
	result.ErrorCategory, result.ErrorDetail = errorFields(lastErr)
}

func refreshCleanupAggregate(value *attempt) {
	failed := false
	allAbsent := len(value.sample.CleanupResources) > 0
	for _, resource := range value.sample.CleanupResources {
		switch resource.Outcome {
		case "deleted":
			allAbsent = false
		case "absent":
		default:
			failed = true
			if value.sample.CleanupErrorCategory == "" {
				value.sample.CleanupErrorCategory = resource.ErrorCategory
				value.sample.CleanupError = resource.ErrorDetail
			}
		}
	}
	if failed {
		value.sample.CleanupSucceeded = false
		if value.sample.ResourceOutcome != "unknown" {
			value.sample.ResourceOutcome = "partial"
		}
		return
	}
	value.sample.CleanupSucceeded = true
	value.sample.CleanupErrorCategory = ""
	value.sample.CleanupError = ""
	value.sample.ResourceOutcome = "deleted"
	if allAbsent {
		value.sample.ResourceOutcome = "absent"
	}
}

func createOutcomeIsDefinitelyEmpty(err error) bool {
	var target *benchmarkError
	if !errors.As(err, &target) {
		return false
	}
	switch target.status {
	case 400, 401, 403, 404, 405, 409, 413, 415, 422, 429:
		return true
	default:
		return false
	}
}

func markFailure(sample *Sample, stage string, err error) {
	sample.Success = false
	sample.FailureStage = stage
	sample.ErrorCategory, sample.ErrorDetail = errorFields(err)
}

func markScheduledFailure(value *attempt, err error) {
	markFailure(&value.sample, "schedule", err)
	completed := time.Now()
	value.completedInstant = completed
	value.sample.CompletedAt = completed.UTC()
	if !value.scheduledInstant.IsZero() {
		value.sample.ObservedNS = max(0, completed.Sub(value.scheduledInstant).Nanoseconds())
	}
}

func appendRequestID(sample *Sample, value string) {
	if value != "" {
		sample.RequestIDs = append(sample.RequestIDs, value)
	}
}

func waitUntil(ctx context.Context, target time.Time) error {
	delay := time.Until(target)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func summarize(attempts []*attempt, mode Mode) Summary {
	summary := Summary{Requested: len(attempts), Errors: make(map[string]int)}
	values := make([]int64, 0, len(attempts))
	serviceValues := make([]int64, 0, len(attempts))
	failureValues := make([]int64, 0, len(attempts))
	var firstScheduled, lastCompleted, firstSuccess time.Time
	for _, value := range attempts {
		sample := value.sample
		if !sample.ScheduledAt.IsZero() {
			summary.Scheduled++
			if firstScheduled.IsZero() || value.scheduledInstant.Before(firstScheduled) {
				firstScheduled = value.scheduledInstant
			}
		}
		if !sample.StartedAt.IsZero() {
			summary.Started++
		}
		if !sample.CompletedAt.IsZero() {
			summary.Completed++
			if value.completedInstant.After(lastCompleted) {
				lastCompleted = value.completedInstant
			}
		}
		if sample.Success {
			summary.Succeeded++
			values = append(values, sample.ObservedNS)
			serviceValues = append(serviceValues, sample.MeasuredNS)
			summary.SuccessfulBytesWritten += sample.BytesWritten
			summary.SuccessfulBytesRead += sample.BytesRead
			if firstSuccess.IsZero() || value.completedInstant.Before(firstSuccess) {
				firstSuccess = value.completedInstant
			}
		} else {
			summary.Failed++
			if sample.ObservedNS > 0 {
				failureValues = append(failureValues, sample.ObservedNS)
			} else {
				summary.LatencyCensored++
			}
			if sample.ErrorCategory != "" {
				summary.Errors[sample.ErrorCategory]++
			}
		}
		switch sample.ResourceOutcome {
		case "none":
			summary.CleanupNotRequired++
		case "deleted", "absent":
			summary.CleanupSucceeded++
		default:
			summary.CleanupFailed++
			if sample.CleanupErrorCategory != "" {
				summary.Errors["cleanup:"+sample.CleanupErrorCategory]++
			}
		}
		for _, resource := range sample.CleanupResources {
			summary.CleanupResourcesExpected++
			switch resource.Outcome {
			case "deleted", "absent":
				summary.CleanupResourcesSucceeded++
			default:
				summary.CleanupResourcesFailed++
			}
		}
	}
	if len(summary.Errors) == 0 {
		summary.Errors = nil
	}
	if len(attempts) > 0 {
		summary.SuccessRate = round(float64(summary.Succeeded)/float64(summary.Requested), 6)
	}
	summary.Latency = summarizeLatency(values)
	summary.ServiceLatency = summarizeLatency(serviceValues)
	summary.FailureLatency = summarizeLatency(failureValues)
	if mode != ModeSequential && !firstScheduled.IsZero() && lastCompleted.After(firstScheduled) {
		summary.MeasurementWindowNS = lastCompleted.Sub(firstScheduled).Nanoseconds()
		summary.ObservedCompletionsPerSecond = round(float64(summary.Succeeded)/lastCompleted.Sub(firstScheduled).Seconds(), 6)
	}
	if !firstScheduled.IsZero() && !firstSuccess.IsZero() && firstSuccess.After(firstScheduled) {
		summary.TimeToFirstSuccessNS = firstSuccess.Sub(firstScheduled).Nanoseconds()
	}
	return summary
}

func cleanupConcurrent(config Config, client *apiClient, attempts []*attempt) {
	limit := min(config.MaxInFlight, 8)
	semaphore := make(chan struct{}, limit)
	var workers sync.WaitGroup
	for _, value := range attempts {
		current := value
		workers.Add(1)
		go func() {
			defer workers.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			cleanupAttempt(config, client, current)
		}()
	}
	workers.Wait()
}

func summarizeLatency(input []int64) LatencySummary {
	if len(input) == 0 {
		return LatencySummary{}
	}
	values := append([]int64(nil), input...)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	var total int64
	for _, value := range values {
		total += value
	}
	toMilliseconds := func(value int64) float64 { return round(float64(value)/float64(time.Millisecond), 6) }
	return LatencySummary{
		Samples: len(values),
		MinMS:   toMilliseconds(values[0]),
		P50MS:   toMilliseconds(percentile(values, 0.50)),
		P95MS:   toMilliseconds(percentile(values, 0.95)),
		P99MS:   toMilliseconds(percentile(values, 0.99)),
		MaxMS:   toMilliseconds(values[len(values)-1]),
		MeanMS:  toMilliseconds(total / int64(len(values))),
	}
}

func percentile(sorted []int64, quantile float64) int64 {
	index := int(math.Ceil(quantile*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	return sorted[index]
}

func round(value float64, digits int) float64 {
	factor := math.Pow10(digits)
	return math.Round(value*factor) / factor
}

func boundaryFor(scenario Scenario) string {
	switch scenario {
	case ScenarioTTI:
		return "customer-observed wall clock from initiating sandbox creation through a confirmed successful nonce command; environment preparation and cleanup excluded"
	case ScenarioWarmExec:
		return "customer-observed wall clock for a confirmed successful nonce command in a pre-created, primed sandbox; creation, priming, and cleanup excluded"
	case ScenarioResume:
		return "customer-observed wall clock from initiating same-sandbox resume through a confirmed successful nonce command; creation, priming, pause, and cleanup excluded"
	case ScenarioFilesystemCheckpoint:
		return "customer-observed wall clock for a synchronous filesystem checkpoint request through a response that binds filesystem kind and source sandbox; sandbox creation, marker write, and cleanup excluded"
	case ScenarioFilesystemRestore:
		return "customer-observed wall clock from initiating sandbox creation from a filesystem checkpoint through a command that returns the exact checkpointed nonce; source creation, marker write, checkpoint capture, and cleanup excluded"
	case ScenarioPreviewFirstByte:
		return "customer-observed wall clock from requesting an authenticated opaque port lease through the first response byte from the preview route; sandbox and server startup, full-body validation, and cleanup excluded"
	case ScenarioPreviewWarm:
		return "customer-observed wall clock from a warm authenticated opaque preview request through its first response byte; sandbox, server, lease, priming request, full-body validation, and cleanup excluded"
	case ScenarioWorkspaceIO:
		return "customer-observed wall clock for writing and reading the configured generated byte payload through the public file API on a mounted durable workspace; workspace and sandbox creation, priming, and cleanup excluded"
	default:
		panic(errors.New("unreachable benchmark scenario"))
	}
}

func commandFor(config Config) []string {
	switch config.Scenario {
	case ScenarioTTI, ScenarioWarmExec, ScenarioResume:
		return []string{"/bin/sh", "-lc", `printf '%s' "$1"`, "perfbench", "<random-nonce>"}
	case ScenarioFilesystemRestore:
		return []string{"/bin/sh", "-lc", `cat "$1"`, "perfbench", "/tmp/perfbench-filesystem-state"}
	case ScenarioPreviewFirstByte, ScenarioPreviewWarm:
		return []string{"/bin/sh", "-lc", `mkdir -p /tmp/perfbench-preview && printf '%s' "$1" > /tmp/perfbench-preview/index.html && busybox httpd -p "$2" -h /tmp/perfbench-preview && printf '%s' "$1"`, "perfbench", "<random-nonce>", "<preview-port>"}
	case ScenarioFilesystemCheckpoint, ScenarioWorkspaceIO:
		return nil
	default:
		return nil
	}
}

func commandSuccessFor(config Config) string {
	switch config.Scenario {
	case ScenarioTTI, ScenarioWarmExec, ScenarioResume, ScenarioFilesystemRestore:
		return "HTTP 200; started event; exact generated nonce on stdout; zero exit event; complete ordered stream"
	case ScenarioPreviewFirstByte, ScenarioPreviewWarm:
		return "HTTP 200; preview server launch command returns exact generated nonce with a complete ordered zero-exit stream"
	case ScenarioFilesystemCheckpoint, ScenarioWorkspaceIO:
		return "not part of the measured boundary"
	default:
		return ""
	}
}

func setupFor(config Config) string {
	switch config.Scenario {
	case ScenarioTTI:
		return "resolve one immutable environment revision"
	case ScenarioWarmExec:
		return "create one sandbox and complete one verified priming command"
	case ScenarioResume:
		return "create one sandbox, complete one verified priming command, and synchronously pause to standby"
	case ScenarioFilesystemCheckpoint:
		return "create one sandbox and write generated nonce bytes to its root filesystem"
	case ScenarioFilesystemRestore:
		return "create one source sandbox, write generated nonce bytes, and synchronously capture a filesystem checkpoint"
	case ScenarioPreviewFirstByte:
		return "create one sandbox and start a nonce-serving busybox HTTP server"
	case ScenarioPreviewWarm:
		return "create one sandbox, start a nonce-serving busybox HTTP server, create an opaque lease, and complete one exact-body priming request"
	case ScenarioWorkspaceIO:
		return "create one durable workspace, attach it at /workspace to one sandbox, and complete one verified priming command"
	default:
		return ""
	}
}

func verificationFor(config Config) string {
	switch config.Scenario {
	case ScenarioFilesystemCheckpoint:
		return "checkpoint response must include an identity, filesystem kind, and the exact source sandbox identity"
	case ScenarioFilesystemRestore:
		return "restored sandbox must bind the exact checkpoint and return the checkpointed nonce from its filesystem"
	case ScenarioPreviewFirstByte, ScenarioPreviewWarm:
		return "latency stops at first byte, but success requires HTTP 200 and the full body to exactly match generated nonce bytes"
	case ScenarioWorkspaceIO:
		return "write must return HTTP 200 and the subsequent read must exactly match every generated byte"
	default:
		return "exact generated nonce through the complete ordered command stream"
	}
}

func scenarioIOBytes(config Config) int {
	if config.Scenario == ScenarioWorkspaceIO {
		return config.IOBytes
	}
	return 0
}

func scenarioPreviewPort(config Config) uint16 {
	if config.Scenario == ScenarioPreviewFirstByte || config.Scenario == ScenarioPreviewWarm {
		return config.PreviewPort
	}
	return 0
}

func benchmarkPayload(size, sampleIndex int) []byte {
	payload := make([]byte, size)
	seed := sha256.Sum256([]byte(fmt.Sprintf("perfbench-workspace-%d-%d", sampleIndex, size)))
	for offset := 0; offset < len(payload); {
		seed = sha256.Sum256(seed[:])
		offset += copy(payload[offset:], seed[:])
	}
	return payload
}
