package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/infercrane/brezel/internal/backend"
	"github.com/infercrane/brezel/internal/domain"
	"github.com/infercrane/brezel/internal/node"
	"github.com/infercrane/brezel/internal/store"
	"github.com/infercrane/brezel/internal/telemetry"
)

const (
	MaxCommandSeconds = 60 * 60
	MaxCommandOutput  = 64 << 20
	MaxFileUpload     = 32 << 20
	MaxFileDownload   = 64 << 20
)

var environmentKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

type RunCommandInput struct {
	Argv           []string          `json:"argv"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int64             `json:"timeout_seconds,omitempty"`
}

type FileReadResult struct {
	Info backend.FileInfo
	Data []byte
}

// RunCommand streams output without persisting command arguments or content.
// Only content-free start/finish metadata enters the lifecycle event log.
func (s *Service) RunCommand(ctx context.Context, projectID, sandboxID string, in RunCommandInput, emit func(backend.CommandEvent) error) (string, error) {
	if err := validateRunCommand(in); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if emit == nil {
		return "", fmt.Errorf("%w: command event callback is required", ErrInvalid)
	}
	if !s.dataPlane.Capabilities().CommandStreaming {
		return "", fmt.Errorf("%w: command streaming is unavailable", ErrDenied)
	}
	executionID, err := randomID("exec")
	if err != nil {
		return "", err
	}
	timeout := in.TimeoutSeconds
	if timeout == 0 {
		timeout = 300
	}
	timeoutCtx, cancelTimeout := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancelTimeout()
	acquireStarted := time.Now()
	sandbox, runCtx, release, err := s.acquireGuestOperation(timeoutCtx, projectID, sandboxID, executionID)
	telemetry.Observe(s.observer, telemetry.OperationCommand, telemetry.PhaseGuestAcquire, acquireStarted, err)
	if err != nil {
		return "", err
	}
	defer release()
	dataPlane, binding, err := s.authorizedNodeDataPlane(sandbox, executionID)
	if err != nil {
		return "", err
	}
	defer binding.Revoke()
	started := s.now()
	s.appendDataEvent(sandbox, "command.started", map[string]any{"execution_id": executionID, "timeout_seconds": timeout})

	var outputBytes int64
	var exitCode *int32
	backendStarted := time.Now()
	runErr := dataPlane.Run(runCtx, binding, backend.CommandRequest{Argv: append([]string(nil), in.Argv...), Cwd: in.Cwd, Env: cloneStrings(in.Env)}, func(event backend.CommandEvent) error {
		event.ExecutionID = executionID
		if event.Type == backend.CommandStdout || event.Type == backend.CommandStderr {
			outputBytes += int64(len(event.Data))
			if outputBytes > MaxCommandOutput {
				return errors.New("command output exceeded the configured limit")
			}
		}
		if event.Type == backend.CommandExited {
			value := event.ExitCode
			exitCode = &value
		}
		return emit(event)
	})
	telemetry.Observe(s.observer, telemetry.OperationCommand, telemetry.PhaseBackendCall, backendStarted, runErr)
	details := map[string]any{
		"execution_id": executionID,
		"duration_ms":  s.now().Sub(started).Milliseconds(),
		"output_bytes": outputBytes,
	}
	if exitCode != nil {
		details["exit_code"] = *exitCode
	}
	if runErr != nil {
		details["result"] = "failed"
		s.appendDataEvent(sandbox, "command.finished", details)
		return executionID, translateGuestError(runErr)
	}
	details["result"] = "exited"
	s.appendDataEvent(sandbox, "command.finished", details)
	return executionID, nil
}

func (s *Service) WriteFile(ctx context.Context, projectID, sandboxID, path string, source io.Reader) (backend.FileInfo, error) {
	if err := validateGuestPath(path); err != nil {
		return backend.FileInfo{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if source == nil {
		return backend.FileInfo{}, fmt.Errorf("%w: file source is required", ErrInvalid)
	}
	if !s.dataPlane.Capabilities().FileReadWrite {
		return backend.FileInfo{}, fmt.Errorf("%w: file transfer is unavailable", ErrDenied)
	}
	operationID, err := randomID("fileop")
	if err != nil {
		return backend.FileInfo{}, err
	}
	acquireStarted := time.Now()
	sandbox, operationCtx, release, err := s.acquireGuestOperation(ctx, projectID, sandboxID, operationID)
	telemetry.Observe(s.observer, telemetry.OperationFileWrite, telemetry.PhaseGuestAcquire, acquireStarted, err)
	if err != nil {
		return backend.FileInfo{}, err
	}
	defer release()
	dataPlane, binding, err := s.authorizedNodeDataPlane(sandbox, operationID)
	if err != nil {
		return backend.FileInfo{}, err
	}
	defer binding.Revoke()
	data, err := io.ReadAll(io.LimitReader(source, MaxFileUpload+1))
	if err != nil {
		return backend.FileInfo{}, fmt.Errorf("%w: could not read upload", ErrInvalid)
	}
	if len(data) > MaxFileUpload {
		return backend.FileInfo{}, fmt.Errorf("%w: file exceeds %d byte upload limit", ErrInvalid, MaxFileUpload)
	}
	backendStarted := time.Now()
	info, err := dataPlane.WriteFile(operationCtx, binding, path, bytes.NewReader(data))
	telemetry.Observe(s.observer, telemetry.OperationFileWrite, telemetry.PhaseBackendCall, backendStarted, err)
	if err != nil {
		return backend.FileInfo{}, translateGuestError(err)
	}
	info.Size = int64(len(data))
	s.appendDataEvent(sandbox, "file.written", map[string]any{"bytes": info.Size})
	return info, nil
}

func (s *Service) ReadFile(ctx context.Context, projectID, sandboxID, path string) (FileReadResult, error) {
	if err := validateGuestPath(path); err != nil {
		return FileReadResult{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if !s.dataPlane.Capabilities().FileReadWrite {
		return FileReadResult{}, fmt.Errorf("%w: file transfer is unavailable", ErrDenied)
	}
	operationID, err := randomID("fileop")
	if err != nil {
		return FileReadResult{}, err
	}
	acquireStarted := time.Now()
	sandbox, operationCtx, release, err := s.acquireGuestOperation(ctx, projectID, sandboxID, operationID)
	telemetry.Observe(s.observer, telemetry.OperationFileRead, telemetry.PhaseGuestAcquire, acquireStarted, err)
	if err != nil {
		return FileReadResult{}, err
	}
	defer release()
	dataPlane, binding, err := s.authorizedNodeDataPlane(sandbox, operationID)
	if err != nil {
		return FileReadResult{}, err
	}
	defer binding.Revoke()
	var output bytes.Buffer
	limited := &boundedWriter{destination: &output, remaining: MaxFileDownload}
	backendStarted := time.Now()
	info, err := dataPlane.ReadFile(operationCtx, binding, path, limited)
	telemetry.Observe(s.observer, telemetry.OperationFileRead, telemetry.PhaseBackendCall, backendStarted, err)
	if err != nil {
		return FileReadResult{}, translateGuestError(err)
	}
	info.Size = int64(output.Len())
	s.appendDataEvent(sandbox, "file.read", map[string]any{"bytes": info.Size})
	return FileReadResult{Info: info, Data: output.Bytes()}, nil
}

// authorizedNodeDataPlane creates the in-process handoff only after
// acquireGuestOperation has completed product ownership, lifecycle, expiry,
// mutation-fence, and quota admission. It deliberately contains no guest or
// engine credential; those remain inside the backend adapter.
func (s *Service) authorizedNodeDataPlane(sandbox domain.Sandbox, operationID string) (node.DataPlane, node.SandboxBinding, error) {
	dataPlane := s.dataPlane
	bindingTarget := sandbox.BackendID
	bindingOptions := []node.BindingOption{node.WithBindingClock(s.now)}
	if routed, ok := dataPlane.(interface{ RequiresNodeAssignment() bool }); ok && routed.RequiresNodeAssignment() {
		if sandbox.NodeID == "" || sandbox.NodeRouteID == "" || sandbox.NodeGeneration == 0 {
			return nil, node.SandboxBinding{}, fmt.Errorf("%w: sandbox has no active node assignment", ErrBackend)
		}
		bindingTarget = sandbox.NodeRouteID
		bindingOptions = append(bindingOptions, node.WithNodeGeneration(sandbox.NodeGeneration))
	}
	expiresAt := s.now().Add(5 * time.Minute)
	if sandbox.ExpiresAt.Before(expiresAt) {
		expiresAt = sandbox.ExpiresAt
	}
	binding, err := node.BindAuthorizedSandbox(sandbox.ProjectID, sandbox.ID, bindingTarget, operationID, sandbox.Revision, expiresAt, s.nodeBindingIsCurrent, bindingOptions...)
	if err != nil {
		return nil, node.SandboxBinding{}, fmt.Errorf("%w: could not bind authorized node operation: %v", ErrBackend, err)
	}
	return dataPlane, binding, nil
}

func (s *Service) nodeBindingIsCurrent(projectID, sandboxID, backendID string, revision int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.getSandbox(projectID, sandboxID)
	currentTarget := current.BackendID
	if current.NodeRouteID != "" {
		currentTarget = current.NodeRouteID
	}
	if err != nil || currentTarget != backendID || current.Revision != revision || !s.now().Before(current.ExpiresAt) {
		return false
	}
	return current.State == domain.SandboxRunning || (current.State == domain.SandboxStandby && current.Lifecycle.AutoResume)
}

func (s *Service) acquireGuestOperation(ctx context.Context, projectID, sandboxID, operationID string) (domain.Sandbox, context.Context, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := domain.ValidateProjectID(projectID); err != nil {
		return domain.Sandbox{}, nil, nil, fmt.Errorf("%w: invalid project", ErrInvalid)
	}
	if strings.TrimSpace(sandboxID) == "" || strings.ContainsAny(sandboxID, "\r\n") {
		return domain.Sandbox{}, nil, nil, fmt.Errorf("%w: invalid sandbox id", ErrInvalid)
	}
	sandbox, err := s.getSandbox(projectID, sandboxID)
	if err != nil {
		return sandbox, nil, nil, err
	}
	if _, active := s.activeBackendMutations[store.ScopedKey(projectID, sandboxID)]; active {
		return sandbox, nil, nil, fmt.Errorf("%w: sandbox has an active backend mutation", ErrConflict)
	}
	if !s.now().Before(sandbox.ExpiresAt) {
		return sandbox, nil, nil, fmt.Errorf("%w: sandbox expired", ErrConflict)
	}
	if sandbox.State != domain.SandboxRunning && !(sandbox.State == domain.SandboxStandby && sandbox.Lifecycle.AutoResume) {
		return sandbox, nil, nil, fmt.Errorf("%w: sandbox is %s", ErrConflict, sandbox.State)
	}
	if sandbox.BackendID == "" {
		return sandbox, nil, nil, fmt.Errorf("%w: sandbox backend identity is unavailable", ErrBackend)
	}
	if s.activeGuestOpsByProject[projectID] >= s.limits.MaxConcurrentGuestOpsPerProject {
		return sandbox, nil, nil, ErrQuota
	}
	key := store.ScopedKey(projectID, sandboxID)
	// Persist the beginning of activity before handing work to the node. This
	// prevents a controller restart during a long guest operation from restoring
	// an old idle deadline and immediately pausing the sandbox. Release advances
	// the deadline again so execution time is never counted as idle time.
	if sandbox.Lifecycle.StandbyAfterSeconds > 0 {
		startedAt := s.now()
		sandbox, err = s.recordActivityLocked(projectID, sandboxID, startedAt)
		if err != nil {
			return sandbox, nil, nil, err
		}
		s.recentActivity[key] = startedAt
	}
	operationCtx, cancel := context.WithCancel(ctx)
	if s.activeGuestOps[key] == nil {
		s.activeGuestOps[key] = make(map[string]context.CancelFunc)
	}
	s.activeGuestOps[key][operationID] = cancel
	s.activeGuestOpsByProject[projectID]++
	var once sync.Once
	release := func() {
		once.Do(func() {
			cancel()
			s.mu.Lock()
			defer s.mu.Unlock()
			if sandbox.Lifecycle.StandbyAfterSeconds > 0 {
				releasedAt := s.now()
				s.recentActivity[key] = releasedAt
				// The guest action already occurred, so a persistence failure cannot
				// safely be returned as a retryable guest failure. The in-process
				// timestamp remains a fail-safe standby fence until another activity
				// or a successful reconciliation write.
				_, _ = s.recordActivityLocked(projectID, sandboxID, releasedAt)
			}
			delete(s.activeGuestOps[key], operationID)
			if len(s.activeGuestOps[key]) == 0 {
				delete(s.activeGuestOps, key)
			}
			if s.activeGuestOpsByProject[projectID] <= 1 {
				delete(s.activeGuestOpsByProject, projectID)
			} else {
				s.activeGuestOpsByProject[projectID]--
			}
		})
	}
	return sandbox, operationCtx, release, nil
}

func (s *Service) hasActiveGuestOperationsLocked(projectID, sandboxID string) bool {
	return len(s.activeGuestOps[store.ScopedKey(projectID, sandboxID)]) > 0
}

func (s *Service) cancelActiveGuestOperationsLocked(projectID, sandboxID string) {
	for _, cancel := range s.activeGuestOps[store.ScopedKey(projectID, sandboxID)] {
		cancel()
	}
}

func (s *Service) appendDataEvent(sandbox domain.Sandbox, eventType string, details map[string]any) {
	now := s.now()
	event := eventFor(sandbox, "", eventType, now, details)
	if appender, ok := s.store.(store.SandboxEventAppender); ok {
		_ = appender.AppendSandboxEvent(event)
		return
	}
	_ = s.store.Update(func(state *store.State) error {
		current, ok := state.Sandboxes[store.ScopedKey(sandbox.ProjectID, sandbox.ID)]
		if !ok {
			return nil
		}
		event.State = current.State
		appendEvent(state, event)
		return nil
	})
}

func validateRunCommand(in RunCommandInput) error {
	if len(in.Argv) == 0 || len(in.Argv) > 256 {
		return errors.New("argv must contain between 1 and 256 entries")
	}
	total := 0
	for _, value := range in.Argv {
		if value == "" || strings.ContainsRune(value, 0) || !utf8.ValidString(value) {
			return errors.New("argv entries must be non-empty UTF-8 without NUL bytes")
		}
		total += len(value)
	}
	if total > 32<<10 {
		return errors.New("argv exceeds 32768 bytes")
	}
	if in.Cwd != "" {
		if err := validateGuestPath(in.Cwd); err != nil {
			return fmt.Errorf("cwd: %w", err)
		}
	}
	if len(in.Env) > 128 {
		return errors.New("env cannot contain more than 128 entries")
	}
	envBytes := 0
	for key, value := range in.Env {
		if !environmentKey.MatchString(key) || strings.ContainsRune(value, 0) || !utf8.ValidString(value) {
			return errors.New("env contains an invalid key or value")
		}
		envBytes += len(key) + len(value)
	}
	if envBytes > 64<<10 {
		return errors.New("env exceeds 65536 bytes")
	}
	if in.TimeoutSeconds < 0 || in.TimeoutSeconds > MaxCommandSeconds {
		return fmt.Errorf("timeout_seconds must be between 1 and %d when set", MaxCommandSeconds)
	}
	return nil
}

func validateGuestPath(path string) error {
	if path == "" || !utf8.ValidString(path) || strings.ContainsRune(path, 0) || strings.ContainsAny(path, "\r\n") {
		return errors.New("path must be valid UTF-8 without NUL or newline bytes")
	}
	if len(path) > 4096 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("path must be a clean absolute path of at most 4096 bytes")
	}
	return nil
}

func cloneStrings(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func translateGuestError(err error) error {
	if errors.Is(err, backend.ErrNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, node.ErrStaleBinding) || errors.Is(err, node.ErrRevokedBinding) {
		return fmt.Errorf("%w: sandbox changed before the guest operation started", ErrConflict)
	}
	return fmt.Errorf("%w: sandbox guest operation failed", ErrBackend)
}

type boundedWriter struct {
	destination io.Writer
	remaining   int64
}

func (w *boundedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, errors.New("file exceeds download limit")
	}
	n, err := w.destination.Write(data)
	w.remaining -= int64(n)
	return n, err
}
