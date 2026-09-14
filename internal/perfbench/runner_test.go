package perfbench

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type testRuntime struct {
	mu                sync.Mutex
	next              int
	nextCheckpoint    int
	nextWorkspace     int
	states            map[string]string
	files             map[string]map[string][]byte
	checkpointFiles   map[string]map[string][]byte
	previewBodies     map[string][]byte
	createCalls       int
	commandCalls      int
	checkpointCalls   int
	checkpointStatus  int
	workspaceCalls    int
	fileWriteCalls    int
	fileReadCalls     int
	leaseCalls        int
	previewCalls      int
	pauseCalls        int
	resumeCalls       int
	deleteCalls       int
	checkpointDeletes int
	workspaceDeletes  int
	cleanupOrder      []string
	corruptOutput     bool
	exitBeforeStart   bool
	createStatus      int
	createDelay       time.Duration
	deleteFailures    int
	deleteNotFound    bool
	deleteKeys        []string
	corruptPreview    bool
	workspaceMountID  string
	workspaceMountAt  string
	checkpointPayload []byte
}

func newTestRuntime(t *testing.T) (*testRuntime, *httptest.Server) {
	t.Helper()
	runtime := &testRuntime{
		states: make(map[string]string), files: make(map[string]map[string][]byte),
		checkpointFiles: make(map[string]map[string][]byte), previewBodies: make(map[string][]byte),
	}
	server := httptest.NewServer(http.HandlerFunc(runtime.serveHTTP))
	t.Cleanup(server.Close)
	return runtime, server
}

func (f *testRuntime) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/p/") {
		f.mu.Lock()
		body, ok := f.previewBodies[r.URL.Path]
		if f.corruptPreview {
			body = []byte("wrong-preview-body")
		}
		f.previewCalls++
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Request-ID", fmt.Sprintf("request-%d", time.Now().UnixNano()))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}
	if r.Header.Get("Authorization") != "Bearer 0123456789abcdef0123456789abcdef" || r.Header.Get("X-Project-ID") != "benchmark-project" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-ID", fmt.Sprintf("request-%d", time.Now().UnixNano()))
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/environments":
		writeTestJSON(w, http.StatusCreated, map[string]any{"resource": map[string]string{"revision_id": "envr-benchmark"}})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
		var input struct {
			CheckpointID    string `json:"checkpoint_id"`
			WorkspaceMounts []struct {
				WorkspaceID string `json:"workspace_id"`
				Path        string `json:"path"`
			} `json:"workspace_mounts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, "invalid", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		delay := f.createDelay
		if f.createStatus != 0 {
			status := f.createStatus
			f.createCalls++
			f.mu.Unlock()
			if delay > 0 {
				time.Sleep(delay)
			}
			writeTestJSON(w, status, map[string]string{"error": "injected"})
			return
		}
		f.next++
		id := fmt.Sprintf("sbx-%03d", f.next)
		f.states[id] = "running"
		f.files[id] = make(map[string][]byte)
		if len(input.WorkspaceMounts) == 1 {
			f.workspaceMountID = input.WorkspaceMounts[0].WorkspaceID
			f.workspaceMountAt = input.WorkspaceMounts[0].Path
		}
		if input.CheckpointID != "" {
			for path, data := range f.checkpointFiles[input.CheckpointID] {
				f.files[id][path] = append([]byte(nil), data...)
			}
		}
		f.createCalls++
		f.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		writeTestJSON(w, http.StatusAccepted, map[string]any{"resource": map[string]string{"id": id, "state": "running", "source_checkpoint_id": input.CheckpointID}})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/workspaces":
		f.mu.Lock()
		f.nextWorkspace++
		id := fmt.Sprintf("wrk-%03d", f.nextWorkspace)
		f.workspaceCalls++
		f.mu.Unlock()
		writeTestJSON(w, http.StatusAccepted, map[string]any{"resource": map[string]string{"id": id, "state": "ready"}})
	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/files"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/"), "/files")
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		if f.files[id] == nil {
			f.files[id] = make(map[string][]byte)
		}
		f.files[id][r.URL.Query().Get("path")] = append([]byte(nil), data...)
		f.fileWriteCalls++
		f.mu.Unlock()
		writeTestJSON(w, http.StatusOK, map[string]any{"size": len(data)})
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/files"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/"), "/files")
		f.mu.Lock()
		data := append([]byte(nil), f.files[id][r.URL.Query().Get("path")]...)
		f.fileReadCalls++
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/checkpoints"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/"), "/checkpoints")
		f.mu.Lock()
		if f.checkpointStatus != 0 {
			status := f.checkpointStatus
			f.checkpointCalls++
			f.mu.Unlock()
			writeTestJSON(w, status, map[string]string{"error": "injected"})
			return
		}
		f.nextCheckpoint++
		checkpointID := fmt.Sprintf("chk-%03d", f.nextCheckpoint)
		f.checkpointFiles[checkpointID] = make(map[string][]byte)
		for path, data := range f.files[id] {
			f.checkpointFiles[checkpointID][path] = append([]byte(nil), data...)
			if path == "/tmp/perfbench-filesystem-state" {
				f.checkpointPayload = append([]byte(nil), data...)
			}
		}
		f.checkpointCalls++
		f.mu.Unlock()
		writeTestJSON(w, http.StatusAccepted, map[string]any{"resource": map[string]string{"id": checkpointID, "kind": "filesystem", "source_sandbox_id": id}})
	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/ports/") && strings.HasSuffix(r.URL.Path, "/leases"):
		parts := strings.Split(r.URL.Path, "/")
		id := parts[3]
		leasePath := fmt.Sprintf("/p/lease-%s/", id)
		f.mu.Lock()
		f.previewBodies[leasePath] = append([]byte(nil), f.files[id]["/tmp/perfbench-preview/index.html"]...)
		f.leaseCalls++
		f.mu.Unlock()
		writeTestJSON(w, http.StatusCreated, map[string]string{"path": leasePath})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":pause"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/"), ":pause")
		f.setState(id, "standby")
		f.mu.Lock()
		f.pauseCalls++
		f.mu.Unlock()
		writeTestJSON(w, http.StatusAccepted, map[string]any{"resource": map[string]string{"id": id, "state": "standby"}})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":resume"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/"), ":resume")
		f.setState(id, "running")
		f.mu.Lock()
		f.resumeCalls++
		f.mu.Unlock()
		writeTestJSON(w, http.StatusAccepted, map[string]any{"resource": map[string]string{"id": id, "state": "running"}})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commands"):
		var input struct {
			Argv []string `json:"argv"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Argv) < 3 {
			http.Error(w, "invalid", http.StatusBadRequest)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/"), "/commands")
		nonce := ""
		if len(input.Argv) >= 5 {
			nonce = input.Argv[4]
		}
		f.mu.Lock()
		f.commandCalls++
		corrupt := f.corruptOutput
		exitFirst := f.exitBeforeStart
		if strings.Contains(input.Argv[2], "busybox httpd") && len(input.Argv) >= 5 {
			if f.files[id] == nil {
				f.files[id] = make(map[string][]byte)
			}
			f.files[id]["/tmp/perfbench-preview/index.html"] = []byte(input.Argv[4])
		}
		if strings.Contains(input.Argv[2], "cat") && len(input.Argv) >= 5 {
			nonce = string(f.files[id][input.Argv[4]])
		}
		f.mu.Unlock()
		if corrupt {
			nonce = "wrong-output"
		}
		w.WriteHeader(http.StatusOK)
		encoder := json.NewEncoder(w)
		if exitFirst {
			_ = encoder.Encode(map[string]any{"type": "exited", "exit_code": 0})
			_ = encoder.Encode(map[string]any{"type": "started"})
			return
		}
		_ = encoder.Encode(map[string]any{"type": "started"})
		_ = encoder.Encode(map[string]any{"type": "stdout", "data": base64.StdEncoding.EncodeToString([]byte(nonce))})
		_ = encoder.Encode(map[string]any{"type": "exited", "exit_code": 0})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/sandboxes/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/")
		f.mu.Lock()
		f.deleteCalls++
		f.cleanupOrder = append(f.cleanupOrder, "sandbox")
		f.deleteKeys = append(f.deleteKeys, r.Header.Get("Idempotency-Key"))
		if f.deleteNotFound {
			f.mu.Unlock()
			writeTestJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		if f.deleteFailures > 0 {
			f.deleteFailures--
			f.mu.Unlock()
			writeTestJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "injected"})
			return
		}
		f.mu.Unlock()
		f.setState(id, "deleted")
		writeTestJSON(w, http.StatusAccepted, map[string]any{"resource": map[string]string{"id": id, "state": "deleted"}})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/checkpoints/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/checkpoints/")
		f.mu.Lock()
		delete(f.checkpointFiles, id)
		f.checkpointDeletes++
		f.cleanupOrder = append(f.cleanupOrder, "checkpoint")
		f.mu.Unlock()
		writeTestJSON(w, http.StatusAccepted, map[string]any{"operation": map[string]string{"state": "succeeded"}})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/workspaces/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/workspaces/")
		f.mu.Lock()
		f.workspaceDeletes++
		f.cleanupOrder = append(f.cleanupOrder, "workspace")
		f.mu.Unlock()
		writeTestJSON(w, http.StatusAccepted, map[string]any{"resource": map[string]string{"id": id, "state": "deleted"}})
	default:
		http.NotFound(w, r)
	}
}

func (f *testRuntime) setState(id, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[id] = state
}

func writeTestJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func testConfig(server *httptest.Server, scenario Scenario, mode Mode, runs int) Config {
	maxInFlight := 1
	if mode != ModeSequential {
		maxInFlight = runs
	}
	return Config{
		BaseURL: server.URL, Token: "0123456789abcdef0123456789abcdef",
		ProjectID: "benchmark-project", BackendTemplate: "base",
		Target: "in-process-test", RuntimeRevision: "test-revision",
		EvidenceClass: EvidenceSyntheticTest, CacheState: CacheCachedTemplate,
		Scenario: scenario, Mode: mode, Runs: runs, MaxInFlight: maxInFlight,
		StaggerInterval: time.Millisecond, AttemptTimeout: 5 * time.Second,
		CleanupTimeout: 5 * time.Second, Execute: true, Client: server.Client(),
	}
}

func TestTTISequentialMeasuresFullBoundaryAndCleans(t *testing.T) {
	fake, server := newTestRuntime(t)
	report, err := Run(context.Background(), testConfig(server, ScenarioTTI, ModeSequential, 3))
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != 3 || report.Summary.Requested != 3 || report.Summary.Scheduled != 3 || report.Summary.Started != 3 || report.Summary.Completed != 3 || report.Summary.Succeeded != 3 || report.Summary.CleanupSucceeded != 3 || report.Summary.CleanupResourcesSucceeded != 3 || report.Summary.Latency.Samples != 3 || report.Summary.ServiceLatency.Samples != 3 {
		t.Fatalf("summary = %#v", report.Summary)
	}
	for _, sample := range report.Samples {
		if sample.Phases.CreateNS <= 0 || sample.Phases.FirstCommandNS <= 0 || sample.MeasuredNS < sample.Phases.CreateNS || sample.ObservedNS < sample.MeasuredNS || sample.ResourceOutcome != "deleted" {
			t.Fatalf("invalid TTI sample: %#v", sample)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.createCalls != 3 || fake.commandCalls != 3 || fake.deleteCalls != 3 {
		t.Fatalf("calls create=%d command=%d delete=%d", fake.createCalls, fake.commandCalls, fake.deleteCalls)
	}
}

func TestFilesystemCheckpointMeasuresCaptureAndCleansSourceFirst(t *testing.T) {
	fake, server := newTestRuntime(t)
	report, err := Run(context.Background(), testConfig(server, ScenarioFilesystemCheckpoint, ModeSequential, 2))
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range report.Samples {
		if sample.Phases.FileWriteNS <= 0 || sample.Phases.CheckpointNS <= 0 || sample.MeasuredNS < sample.Phases.CheckpointNS {
			t.Fatalf("invalid checkpoint sample: %#v", sample)
		}
		if len(sample.CleanupResources) != 2 || !sample.CleanupSucceeded {
			t.Fatalf("checkpoint cleanup evidence: %#v", sample.CleanupResources)
		}
	}
	if report.Summary.CleanupResourcesSucceeded != 4 || report.Summary.CleanupResourcesFailed != 0 {
		t.Fatalf("summary = %#v", report.Summary)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.checkpointCalls != 2 || fake.deleteCalls != 2 || fake.checkpointDeletes != 2 {
		t.Fatalf("calls checkpoint=%d sandbox-delete=%d checkpoint-delete=%d", fake.checkpointCalls, fake.deleteCalls, fake.checkpointDeletes)
	}
	if strings.Join(fake.cleanupOrder, ",") != "sandbox,checkpoint,sandbox,checkpoint" {
		t.Fatalf("cleanup order = %v", fake.cleanupOrder)
	}
}

func TestFilesystemRestoreVerifiesCheckpointedBytes(t *testing.T) {
	fake, server := newTestRuntime(t)
	report, err := Run(context.Background(), testConfig(server, ScenarioFilesystemRestore, ModeSequential, 1))
	if err != nil {
		t.Fatal(err)
	}
	sample := report.Samples[0]
	if sample.Phases.CheckpointNS <= 0 || sample.Phases.RestoreNS <= 0 || sample.Phases.FirstCommandNS <= 0 || sample.MeasuredNS < sample.Phases.RestoreNS+sample.Phases.FirstCommandNS {
		t.Fatalf("restore sample = %#v", sample)
	}
	if len(sample.CleanupResources) != 3 || report.Summary.CleanupResourcesSucceeded != 3 {
		t.Fatalf("restore cleanup = %#v", sample.CleanupResources)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.createCalls != 2 || fake.checkpointCalls != 1 || fake.commandCalls != 1 || fake.deleteCalls != 2 || fake.checkpointDeletes != 1 {
		t.Fatalf("calls create=%d checkpoint=%d command=%d sandbox-delete=%d checkpoint-delete=%d", fake.createCalls, fake.checkpointCalls, fake.commandCalls, fake.deleteCalls, fake.checkpointDeletes)
	}
}

func TestAmbiguousCheckpointCreateKeepsCleanupRiskVisible(t *testing.T) {
	fake, server := newTestRuntime(t)
	fake.checkpointStatus = http.StatusServiceUnavailable
	report, err := Run(context.Background(), testConfig(server, ScenarioFilesystemCheckpoint, ModeSequential, 1))
	if err == nil {
		t.Fatal("ambiguous checkpoint create unexpectedly passed")
	}
	sample := report.Samples[0]
	if sample.ResourceOutcome != "unknown" || sample.CleanupSucceeded || len(sample.CleanupResources) != 2 {
		t.Fatalf("sample = %#v", sample)
	}
	if report.Summary.CleanupResourcesSucceeded != 1 || report.Summary.CleanupResourcesFailed != 1 || report.Summary.Errors["cleanup:unknown_create_outcome"] != 1 {
		t.Fatalf("summary = %#v", report.Summary)
	}
}

func TestPreviewScenariosSeparateLeaseAndWarmFirstByte(t *testing.T) {
	for _, scenario := range []Scenario{ScenarioPreviewFirstByte, ScenarioPreviewWarm} {
		t.Run(string(scenario), func(t *testing.T) {
			fake, server := newTestRuntime(t)
			report, err := Run(context.Background(), testConfig(server, scenario, ModeSequential, 1))
			if err != nil {
				t.Fatal(err)
			}
			sample := report.Samples[0]
			if sample.Phases.PreviewFirstByteNS <= 0 || sample.Phases.PreviewCompleteNS < sample.Phases.PreviewFirstByteNS || sample.MeasuredNS <= 0 {
				t.Fatalf("preview sample = %#v", sample)
			}
			if scenario == ScenarioPreviewFirstByte && sample.MeasuredNS < sample.Phases.PortLeaseNS+sample.Phases.PreviewFirstByteNS {
				t.Fatalf("first preview omitted lease: %#v", sample)
			}
			if scenario == ScenarioPreviewWarm && sample.MeasuredNS < sample.Phases.PreviewFirstByteNS {
				t.Fatalf("warm preview omitted first byte: %#v", sample)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			wantPreviewCalls := 1
			if scenario == ScenarioPreviewWarm {
				wantPreviewCalls = 2
			}
			if fake.leaseCalls != 1 || fake.previewCalls != wantPreviewCalls {
				t.Fatalf("lease=%d previews=%d", fake.leaseCalls, fake.previewCalls)
			}
		})
	}
}

func TestPreviewRequiresExactCompleteBodyAfterFirstByte(t *testing.T) {
	fake, server := newTestRuntime(t)
	fake.corruptPreview = true
	report, err := Run(context.Background(), testConfig(server, ScenarioPreviewFirstByte, ModeSequential, 1))
	if err == nil {
		t.Fatal("corrupt preview body unexpectedly passed")
	}
	sample := report.Samples[0]
	if sample.Success || sample.ErrorCategory != "output_mismatch" || sample.Phases.PreviewFirstByteNS <= 0 || sample.Phases.PreviewCompleteNS < sample.Phases.PreviewFirstByteNS || !sample.CleanupSucceeded {
		t.Fatalf("preview failure = %#v", sample)
	}
}

func TestWorkspaceIOMeasuresExactGeneratedPayloadAndCleans(t *testing.T) {
	fake, server := newTestRuntime(t)
	config := testConfig(server, ScenarioWorkspaceIO, ModeSequential, 1)
	config.IOBytes = 64 << 10
	report, err := Run(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	sample := report.Samples[0]
	if sample.BytesWritten != int64(config.IOBytes) || sample.BytesRead != int64(config.IOBytes) || sample.Phases.FileWriteNS <= 0 || sample.Phases.FileReadNS <= 0 {
		t.Fatalf("workspace sample = %#v", sample)
	}
	if report.Metadata.IOBytes != config.IOBytes || report.Summary.SuccessfulBytesWritten != int64(config.IOBytes) || report.Summary.SuccessfulBytesRead != int64(config.IOBytes) || report.Summary.CleanupResourcesSucceeded != 2 {
		t.Fatalf("workspace report = %#v", report)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.workspaceCalls != 1 || fake.workspaceDeletes != 1 || fake.fileWriteCalls != 1 || fake.fileReadCalls != 1 || fake.deleteCalls != 1 {
		t.Fatalf("workspace calls create=%d delete=%d writes=%d reads=%d sandbox-delete=%d", fake.workspaceCalls, fake.workspaceDeletes, fake.fileWriteCalls, fake.fileReadCalls, fake.deleteCalls)
	}
	if fake.workspaceMountID != "wrk-001" || fake.workspaceMountAt != "/workspace" {
		t.Fatalf("workspace mount = %q at %q", fake.workspaceMountID, fake.workspaceMountAt)
	}
}

func TestEveryScenarioSupportsEveryLoadShape(t *testing.T) {
	scenarios := []Scenario{
		ScenarioTTI,
		ScenarioWarmExec,
		ScenarioResume,
		ScenarioFilesystemCheckpoint,
		ScenarioFilesystemRestore,
		ScenarioPreviewFirstByte,
		ScenarioPreviewWarm,
		ScenarioWorkspaceIO,
	}
	modes := []Mode{ModeSequential, ModeStaggered, ModeBurst}
	for _, scenario := range scenarios {
		for _, mode := range modes {
			t.Run(string(scenario)+"/"+string(mode), func(t *testing.T) {
				_, server := newTestRuntime(t)
				config := testConfig(server, scenario, mode, 2)
				config.IOBytes = 4 << 10
				report, err := Run(context.Background(), config)
				if err != nil {
					t.Fatal(err)
				}
				if report.Summary.Succeeded != 2 || report.Summary.Failed != 0 || report.Summary.CleanupFailed != 0 || report.Summary.CleanupResourcesFailed != 0 {
					t.Fatalf("summary = %#v", report.Summary)
				}
			})
		}
	}
}

func TestReportOmitsGeneratedContentAndRuntimeResourceIdentities(t *testing.T) {
	fake, server := newTestRuntime(t)
	report, err := Run(context.Background(), testConfig(server, ScenarioFilesystemRestore, ModeSequential, 1))
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	generated := string(fake.checkpointPayload)
	fake.mu.Unlock()
	if generated == "" {
		t.Fatal("fake runtime did not capture generated checkpoint content")
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{generated, "sbx-001", "sbx-002", "chk-001", "/p/lease-"} {
		if forbidden != "" && strings.Contains(string(encoded), forbidden) {
			t.Fatalf("report leaked generated content or runtime identity %q", forbidden)
		}
	}
}

func TestSequentialStopsAfterUnconfirmedCleanup(t *testing.T) {
	fake, server := newTestRuntime(t)
	fake.createStatus = http.StatusServiceUnavailable
	report, err := Run(context.Background(), testConfig(server, ScenarioTTI, ModeSequential, 3))
	if err == nil {
		t.Fatal("unconfirmed create outcome unexpectedly passed")
	}
	if report.Summary.Requested != 3 || report.Summary.Failed != 3 || report.Samples[1].FailureStage != "cleanup_guard" || report.Samples[2].FailureStage != "cleanup_guard" {
		t.Fatalf("report = %#v", report)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.createCalls != 1 {
		t.Fatalf("create calls after cleanup risk = %d", fake.createCalls)
	}
}

func TestConcurrentSetupStopsBeforeMeasuredMutationsAfterUnknownOutcome(t *testing.T) {
	fake, server := newTestRuntime(t)
	fake.checkpointStatus = http.StatusServiceUnavailable
	report, err := Run(context.Background(), testConfig(server, ScenarioFilesystemRestore, ModeBurst, 3))
	if err == nil {
		t.Fatal("unconfirmed checkpoint outcome unexpectedly passed")
	}
	if report.Summary.Requested != 3 || report.Summary.Scheduled != 0 || report.Summary.Failed != 3 || report.Samples[1].FailureStage != "cleanup_guard" || report.Samples[2].FailureStage != "cleanup_guard" {
		t.Fatalf("report = %#v", report)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.createCalls != 1 || fake.checkpointCalls != 1 || fake.deleteCalls != 1 {
		t.Fatalf("calls create=%d checkpoint=%d delete=%d", fake.createCalls, fake.checkpointCalls, fake.deleteCalls)
	}
}

func TestResumeBurstSeparatesSetupFromMeasuredBoundary(t *testing.T) {
	fake, server := newTestRuntime(t)
	report, err := Run(context.Background(), testConfig(server, ScenarioResume, ModeBurst, 3))
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Succeeded != 3 || report.Summary.ObservedCompletionsPerSecond <= 0 {
		t.Fatalf("summary = %#v", report.Summary)
	}
	for _, sample := range report.Samples {
		if sample.Phases.SetupNS <= 0 || sample.Phases.PrimeCommandNS <= 0 || sample.Phases.PauseNS <= 0 || sample.Phases.ResumeNS <= 0 || sample.Phases.FirstCommandNS <= 0 {
			t.Fatalf("invalid resume sample: %#v", sample)
		}
		if sample.MeasuredNS < sample.Phases.ResumeNS+sample.Phases.FirstCommandNS {
			t.Fatal("measured resume boundary omitted a timed phase")
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.createCalls != 3 || fake.commandCalls != 6 || fake.pauseCalls != 3 || fake.resumeCalls != 3 || fake.deleteCalls != 3 {
		t.Fatalf("calls create=%d command=%d pause=%d resume=%d delete=%d", fake.createCalls, fake.commandCalls, fake.pauseCalls, fake.resumeCalls, fake.deleteCalls)
	}
}

func TestWarmExecStaggeredPrimesOutsideMeasuredBoundary(t *testing.T) {
	fake, server := newTestRuntime(t)
	report, err := Run(context.Background(), testConfig(server, ScenarioWarmExec, ModeStaggered, 2))
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range report.Samples {
		if sample.Phases.SetupNS <= 0 || sample.Phases.PrimeCommandNS <= 0 || sample.Phases.FirstCommandNS <= 0 || sample.Phases.ResumeNS != 0 {
			t.Fatalf("invalid warm-exec sample: %#v", sample)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.createCalls != 2 || fake.commandCalls != 4 || fake.pauseCalls != 0 || fake.resumeCalls != 0 || fake.deleteCalls != 2 {
		t.Fatalf("calls create=%d command=%d pause=%d resume=%d delete=%d", fake.createCalls, fake.commandCalls, fake.pauseCalls, fake.resumeCalls, fake.deleteCalls)
	}
}

func TestOutputMismatchIsCategorizedAndResourceIsCleaned(t *testing.T) {
	fake, server := newTestRuntime(t)
	fake.corruptOutput = true
	report, err := Run(context.Background(), testConfig(server, ScenarioTTI, ModeSequential, 1))
	if err == nil {
		t.Fatal("output mismatch unexpectedly passed")
	}
	if report.Summary.Errors["output_mismatch"] != 1 || report.Summary.CleanupFailed != 0 {
		t.Fatalf("summary = %#v", report.Summary)
	}
	if report.Samples[0].FailureStage != "measure" || report.Samples[0].ResourceOutcome != "deleted" {
		t.Fatalf("sample = %#v", report.Samples[0])
	}
}

func TestUnknownCreateOutcomeIsReportedAsCleanupRisk(t *testing.T) {
	fake, server := newTestRuntime(t)
	fake.createStatus = http.StatusServiceUnavailable
	report, err := Run(context.Background(), testConfig(server, ScenarioTTI, ModeSequential, 1))
	if err == nil {
		t.Fatal("uncertain create unexpectedly passed")
	}
	if report.Summary.Errors["http_5xx"] != 1 || report.Summary.Errors["cleanup:unknown_create_outcome"] != 1 || report.Summary.CleanupFailed != 1 {
		t.Fatalf("summary = %#v", report.Summary)
	}
	if report.Samples[0].ResourceOutcome != "unknown" || report.Samples[0].CleanupSucceeded {
		t.Fatalf("sample = %#v", report.Samples[0])
	}
}

func TestCleanupRetriesWithoutChangingMeasuredSuccess(t *testing.T) {
	fake, server := newTestRuntime(t)
	fake.deleteFailures = 2
	report, err := Run(context.Background(), testConfig(server, ScenarioTTI, ModeSequential, 1))
	if err != nil {
		t.Fatal(err)
	}
	if !report.Samples[0].Success || !report.Samples[0].CleanupSucceeded || report.Samples[0].CleanupAttempts != 3 {
		t.Fatalf("sample = %#v", report.Samples[0])
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.deleteKeys) != 3 || fake.deleteKeys[0] == "" || fake.deleteKeys[0] != fake.deleteKeys[1] || fake.deleteKeys[1] != fake.deleteKeys[2] {
		t.Fatalf("cleanup idempotency keys = %#v", fake.deleteKeys)
	}
}

func TestStaggeredLatencyIncludesClientAdmissionDelay(t *testing.T) {
	fake, server := newTestRuntime(t)
	fake.createDelay = 30 * time.Millisecond
	config := testConfig(server, ScenarioTTI, ModeStaggered, 3)
	config.MaxInFlight = 1
	config.StaggerInterval = time.Millisecond
	report, err := Run(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Latency.P50MS <= report.Summary.ServiceLatency.P50MS {
		t.Fatalf("observed latency did not include queueing: %#v", report.Summary)
	}
	if report.Summary.TimeToFirstSuccessNS <= 0 || report.Summary.MeasurementWindowNS <= 0 {
		t.Fatalf("concurrent timing summary = %#v", report.Summary)
	}
	if report.Samples[1].ScheduleDelayNS <= 0 || report.Samples[1].ObservedNS < report.Samples[1].ScheduleDelayNS+report.Samples[1].MeasuredNS {
		t.Fatalf("queued sample = %#v", report.Samples[1])
	}
}

func TestDeleteNotFoundVerifiesCleanupAbsence(t *testing.T) {
	fake, server := newTestRuntime(t)
	fake.deleteNotFound = true
	report, err := Run(context.Background(), testConfig(server, ScenarioTTI, ModeSequential, 1))
	if err != nil {
		t.Fatal(err)
	}
	if !report.Samples[0].CleanupSucceeded || report.Samples[0].ResourceOutcome != "absent" || report.Summary.CleanupSucceeded != 1 {
		t.Fatalf("cleanup result = %#v, summary = %#v", report.Samples[0], report.Summary)
	}
}

func TestCommandStreamMustBeOrdered(t *testing.T) {
	fake, server := newTestRuntime(t)
	fake.exitBeforeStart = true
	report, err := Run(context.Background(), testConfig(server, ScenarioTTI, ModeSequential, 1))
	if err == nil {
		t.Fatal("out-of-order command stream unexpectedly passed")
	}
	if report.Summary.Errors["command_stream"] != 1 || report.Samples[0].FailureStage != "measure" {
		t.Fatalf("result = %#v", report)
	}
}

func TestDefinitiveRejectedCreateNeedsNoCleanup(t *testing.T) {
	fake, server := newTestRuntime(t)
	fake.createStatus = http.StatusBadRequest
	report, err := Run(context.Background(), testConfig(server, ScenarioTTI, ModeSequential, 1))
	if err == nil {
		t.Fatal("rejected create unexpectedly passed")
	}
	if report.Samples[0].ResourceOutcome != "none" || report.Summary.CleanupNotRequired != 1 || report.Summary.CleanupFailed != 0 {
		t.Fatalf("result = %#v", report)
	}
}

func TestAmbiguousClientTimeoutKeepsCleanupRiskVisible(t *testing.T) {
	fake, server := newTestRuntime(t)
	fake.createStatus = http.StatusRequestTimeout
	report, err := Run(context.Background(), testConfig(server, ScenarioTTI, ModeSequential, 1))
	if err == nil {
		t.Fatal("ambiguous create unexpectedly passed")
	}
	if report.Samples[0].ResourceOutcome != "unknown" || report.Summary.CleanupFailed != 1 || report.Summary.Errors["cleanup:unknown_create_outcome"] != 1 {
		t.Fatalf("result = %#v", report)
	}
}

func TestValidationRequiresExplicitAndTruthfulMetadata(t *testing.T) {
	_, server := newTestRuntime(t)
	config := testConfig(server, ScenarioTTI, ModeSequential, 1)
	config.Execute = false
	if _, err := Run(context.Background(), config); err == nil || !strings.Contains(err.Error(), "explicit") {
		t.Fatalf("Execute=false error = %v", err)
	}
	config.Execute = true
	config.EvidenceClass = "marketing"
	if _, err := Run(context.Background(), config); err == nil || !strings.Contains(err.Error(), "evidence class") {
		t.Fatalf("invalid evidence class error = %v", err)
	}
}

func TestPlaintextRemoteControlAPIIsRejected(t *testing.T) {
	config := Config{
		BaseURL: "http://brezel.example.com", Token: "0123456789abcdef0123456789abcdef",
		ProjectID: "benchmark-project", BackendTemplate: "base", Target: "remote",
		RuntimeRevision: "revision", EvidenceClass: EvidenceHostedEndToEnd,
		CacheState: CacheUnknown, Scenario: ScenarioTTI, Mode: ModeSequential,
		Runs: 1, MaxInFlight: 1, AttemptTimeout: time.Second, CleanupTimeout: time.Second, Execute: true,
	}
	if _, err := Run(context.Background(), config); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("remote plaintext error = %v", err)
	}
}
