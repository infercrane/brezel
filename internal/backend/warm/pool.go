// Package warm adds crash-recoverable, single-use warm capacity to a backend.
// A slot is created from one immutable template and network policy, claimed at
// most once, and destroyed after the claiming sandbox is deleted. Customer
// state is never returned to the pool.
package warm

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/infercrane/brezel/internal/backend"
	"github.com/infercrane/brezel/internal/domain"
	"github.com/infercrane/brezel/internal/telemetry"
	_ "modernc.org/sqlite"
)

const (
	poolProjectID  = "brezel-warm-pool"
	stateCreating  = "creating"
	stateAvailable = "available"
	stateClaiming  = "claiming"
	stateClaimed   = "claimed"
	stateDeleting  = "deleting"
	maxPoolSize    = 4096
	claimSafety    = 30 * time.Second
)

var safeTemplateID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$`)

var errWarmUnavailable = errors.New("claimed warm slot is not running")

type Config struct {
	Path             string
	Target           int
	TemplateID       string
	SlotTTL          time.Duration
	MaxClaimTTL      time.Duration
	PrimeConcurrency int
	Network          domain.NetworkPolicy
	// Strict turns the pool into a fixed physical-capacity budget. Requests
	// which cannot claim the exact clean template are rejected before touching
	// the backend instead of silently oversubscribing the host.
	Strict bool
	// Observer receives only fixed-dimensional, content-free phase metrics.
	Observer telemetry.Observer
}

type Pool struct {
	inner    backend.Backend
	config   Config
	db       *sql.DB
	lockFile *os.File
	mu       sync.Mutex
	closed   bool
}

type slot struct {
	ID             string
	BackendID      string
	LocalID        string
	State          string
	ClaimProjectID string
	ClaimSandboxID string
	CreatedAt      time.Time
	ExpiresAt      time.Time
	ClaimExpiresAt time.Time
}

func New(inner backend.Backend, config Config) (*Pool, error) {
	if inner == nil {
		return nil, errors.New("warm pool backend is required")
	}
	if _, ok := inner.(backend.TimeoutRuntime); !ok {
		return nil, errors.New("warm pool backend must support hard timeout reset")
	}
	if config.Target < 1 || config.Target > maxPoolSize {
		return nil, fmt.Errorf("warm pool target must be between 1 and %d", maxPoolSize)
	}
	if !safeTemplateID.MatchString(config.TemplateID) {
		return nil, errors.New("warm pool template id is invalid")
	}
	if config.SlotTTL < time.Minute || config.SlotTTL > 30*24*time.Hour || config.SlotTTL%time.Second != 0 {
		return nil, errors.New("warm pool slot TTL must be a whole number of seconds between one minute and 30 days")
	}
	if config.MaxClaimTTL < 30*time.Second || config.MaxClaimTTL%time.Second != 0 || config.MaxClaimTTL+claimSafety >= config.SlotTTL {
		return nil, errors.New("warm pool maximum claim TTL must leave at least 30 seconds of slot lifetime")
	}
	if config.PrimeConcurrency < 1 || config.PrimeConcurrency > config.Target {
		return nil, errors.New("warm pool prime concurrency must be between one and the target")
	}
	if err := domain.ValidateNetwork(config.Network); err != nil {
		return nil, fmt.Errorf("warm pool network: %w", err)
	}
	if config.Path == "" {
		return nil, errors.New("warm pool database path is required")
	}
	db, lockFile, err := openDatabase(config.Path)
	if err != nil {
		return nil, err
	}
	return &Pool{inner: inner, config: config, db: db, lockFile: lockFile}, nil
}

func openDatabase(path string) (*sql.DB, *os.File, error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create warm pool state directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return nil, nil, errors.New("warm pool state directory must be a private real directory")
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, nil, errors.New("warm pool database must be a private regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("inspect warm pool database: %w", err)
	} else {
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if createErr != nil {
			return nil, nil, fmt.Errorf("create warm pool database: %w", createErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return nil, nil, closeErr
		}
	}
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if err := requirePrivateFileIfPresent(sidecar, "warm pool database sidecar"); err != nil {
			return nil, nil, err
		}
	}
	lockPath := path + ".lock"
	if err := requirePrivateFileIfPresent(lockPath, "warm pool lock"); err != nil {
		return nil, nil, err
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open warm pool lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		return nil, nil, errors.New("warm pool database is already open")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		_ = lockFile.Close()
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA busy_timeout=5000",
		`CREATE TABLE IF NOT EXISTS warm_slots (
			id TEXT PRIMARY KEY,
			backend_id TEXT NOT NULL DEFAULT '',
			local_id TEXT NOT NULL UNIQUE,
			state TEXT NOT NULL CHECK (state IN ('creating','available','claiming','claimed','deleting')),
			claim_project_id TEXT NOT NULL DEFAULT '',
			claim_sandbox_id TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			claim_expires_at INTEGER NOT NULL DEFAULT 0
		) WITHOUT ROWID`,
		`CREATE UNIQUE INDEX IF NOT EXISTS warm_slots_backend_id ON warm_slots(backend_id) WHERE backend_id != ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS warm_slots_claim ON warm_slots(claim_project_id, claim_sandbox_id) WHERE claim_project_id != '' AND claim_sandbox_id != ''`,
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
			_ = lockFile.Close()
			return nil, nil, fmt.Errorf("initialize warm pool database: %w", err)
		}
	}
	for _, databaseFile := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Lstat(databaseFile); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			_ = db.Close()
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
			_ = lockFile.Close()
			return nil, nil, fmt.Errorf("inspect warm pool database file: %w", err)
		}
		if err := os.Chmod(databaseFile, 0o600); err != nil {
			_ = db.Close()
			_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
			_ = lockFile.Close()
			return nil, nil, fmt.Errorf("restrict warm pool database permissions: %w", err)
		}
	}
	return db, lockFile, nil
}

func requirePrivateFileIfPresent(path, label string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", label, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must be a private regular file", label)
	}
	return nil
}

func (p *Pool) Name() string                       { return p.inner.Name() }
func (p *Pool) Capabilities() backend.Capabilities { return p.inner.Capabilities() }

func (p *Pool) Prime(ctx context.Context) error {
	if err := p.recover(ctx); err != nil {
		return err
	}
	reserved, err := p.reserveMissing()
	if err != nil {
		return err
	}
	return p.createReserved(ctx, reserved)
}

// Maintain removes expired unused slots and restores the configured clean
// capacity. Claimed slots are customer resources and are never rotated here.
func (p *Pool) Maintain(ctx context.Context) error {
	rows, err := p.listStates(stateAvailable, stateCreating, stateDeleting)
	if err != nil {
		return err
	}
	for _, current := range rows {
		if current.State == stateAvailable && time.Until(current.ExpiresAt) >= p.config.MaxClaimTTL+claimSafety {
			continue
		}
		if current.State == stateCreating {
			// A refill may still be in flight. Only repair reservations which
			// have exceeded the complete create deadline by a wide margin.
			if time.Since(current.CreatedAt) < 5*time.Minute {
				continue
			}
			if err := p.recoverCreating(ctx, current); err != nil {
				return err
			}
			if err := p.refill(ctx); err != nil {
				return err
			}
			continue
		}
		if current.State == stateDeleting {
			if err := p.discard(ctx, current); err != nil {
				return err
			}
			if err := p.refill(ctx); err != nil {
				return err
			}
			continue
		}
		retired, err := p.retireAvailable(current.ID)
		if err != nil {
			return err
		}
		if !retired {
			// A create request claimed this slot after the maintenance
			// snapshot. Customer ownership always wins rotation.
			continue
		}
		current.State = stateDeleting
		if err := p.discard(ctx, current); err != nil {
			return err
		}
		// Rotate one slot at a time so a same-age pool does not disappear as
		// one maintenance batch near its hard deadline.
		if err := p.refill(ctx); err != nil {
			return err
		}
	}
	return p.refill(ctx)
}

func (p *Pool) Create(ctx context.Context, request backend.CreateRequest) (backend.Sandbox, error) {
	if !p.eligible(request) {
		if p.config.Strict {
			return backend.Sandbox{}, backend.ErrCapacityUnavailable
		}
		return p.inner.Create(ctx, request)
	}
	existing, found, err := p.findClaim(request.ProjectID, request.LocalSandboxID)
	if err != nil {
		return backend.Sandbox{}, err
	}
	if found {
		observed, claimErr := p.completeClaim(ctx, existing, request.ProjectID, request.LocalSandboxID)
		if !errors.Is(claimErr, errWarmUnavailable) || p.config.Strict {
			return observed, claimErr
		}
		return p.inner.Create(ctx, request)
	}
	reserveStarted := time.Now()
	current, found, err := p.beginClaim(request.ProjectID, request.LocalSandboxID, time.Duration(request.Lifecycle.ExpiresAfterSeconds)*time.Second)
	if p.config.Observer != nil {
		outcome := telemetry.OutcomeHit
		if err != nil {
			outcome = telemetry.OutcomeError
		} else if !found {
			outcome = telemetry.OutcomeMiss
		}
		p.config.Observer.ObservePhase(telemetry.OperationSandboxCreate, telemetry.PhaseWarmReserve, outcome, time.Since(reserveStarted))
	}
	if err != nil {
		return backend.Sandbox{}, err
	}
	if !found {
		if p.config.Strict {
			return backend.Sandbox{}, backend.ErrCapacityUnavailable
		}
		return p.inner.Create(ctx, request)
	}
	observed, claimErr := p.completeClaim(ctx, current, request.ProjectID, request.LocalSandboxID)
	if !errors.Is(claimErr, errWarmUnavailable) || p.config.Strict {
		return observed, claimErr
	}
	return p.inner.Create(ctx, request)
}

func (p *Pool) completeClaim(ctx context.Context, current slot, projectID, sandboxID string) (backend.Sandbox, error) {
	verifyStarted := time.Now()
	observed, err := p.inner.Inspect(ctx, current.BackendID)
	telemetry.Observe(p.config.Observer, telemetry.OperationSandboxCreate, telemetry.PhaseWarmVerify, verifyStarted, err)
	if err != nil || observed.State != domain.SandboxRunning {
		cause := err
		if cause == nil {
			cause = fmt.Errorf("observed state %q", observed.State)
		}
		if discardErr := p.discard(context.WithoutCancel(ctx), current); discardErr != nil {
			// A slot whose destruction cannot be confirmed remains quarantined in
			// deleting state. Never return it to capacity or claim it for a user.
			return backend.Sandbox{}, fmt.Errorf("discard unusable claimed warm slot: %w", errors.Join(cause, discardErr))
		}
		p.refillAfterFailure(ctx)
		return backend.Sandbox{}, fmt.Errorf("%w: %w: %v", backend.ErrCapacityUnavailable, errWarmUnavailable, cause)
	}
	if current.State == stateClaiming {
		remaining := time.Until(current.ClaimExpiresAt)
		remainingSeconds := int64((remaining + time.Second - 1) / time.Second)
		if remainingSeconds < 1 {
			remainingSeconds = 1
		}
		deadlineStarted := time.Now()
		if err := p.inner.(backend.TimeoutRuntime).SetTimeout(ctx, current.BackendID, remainingSeconds); err != nil {
			telemetry.Observe(p.config.Observer, telemetry.OperationSandboxCreate, telemetry.PhaseWarmDeadline, deadlineStarted, err)
			if discardErr := p.discard(context.WithoutCancel(ctx), current); discardErr != nil {
				return backend.Sandbox{}, fmt.Errorf("reset claimed warm slot timeout: %w", errors.Join(err, discardErr))
			}
			p.refillAfterFailure(ctx)
			return backend.Sandbox{}, fmt.Errorf("%w: reset claimed warm slot timeout", backend.ErrCapacityUnavailable)
		}
		telemetry.Observe(p.config.Observer, telemetry.OperationSandboxCreate, telemetry.PhaseWarmDeadline, deadlineStarted, nil)
		if err := p.finishClaim(current.ID, projectID, sandboxID); err != nil {
			return backend.Sandbox{}, err
		}
	}
	return observed, nil
}

func (p *Pool) refillAfterFailure(ctx context.Context) {
	refillCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	_ = p.refill(refillCtx)
}

func (p *Pool) refill(ctx context.Context) error {
	reserved, err := p.reserveMissing()
	if err != nil {
		return err
	}
	return p.createReserved(ctx, reserved)
}

func (p *Pool) eligible(request backend.CreateRequest) bool {
	return request.TemplateID == p.config.TemplateID &&
		len(request.Environment) == 0 &&
		len(request.WorkspaceMounts) == 0 &&
		request.Lifecycle.ExpiresAfterSeconds >= 30 &&
		time.Duration(request.Lifecycle.ExpiresAfterSeconds)*time.Second <= p.config.MaxClaimTTL &&
		reflect.DeepEqual(request.Network, p.config.Network)
}

func (p *Pool) Find(ctx context.Context, localID, projectID string) (backend.Sandbox, error) {
	current, found, err := p.findClaim(projectID, localID)
	if err != nil {
		return backend.Sandbox{}, err
	}
	if !found {
		return p.inner.Find(ctx, localID, projectID)
	}
	return p.completeClaim(ctx, current, projectID, localID)
}

func (p *Pool) Inspect(ctx context.Context, id string) (backend.Sandbox, error) {
	return p.inner.Inspect(ctx, id)
}

func (p *Pool) Pause(ctx context.Context, id string, kind domain.CheckpointKind) error {
	return p.inner.Pause(ctx, id, kind)
}

func (p *Pool) Resume(ctx context.Context, id string, kind domain.CheckpointKind, ttlSeconds int64) (backend.Sandbox, error) {
	return p.inner.Resume(ctx, id, kind, ttlSeconds)
}

func (p *Pool) Delete(ctx context.Context, id string) error {
	current, pooled, err := p.findBackend(id)
	if err != nil {
		return err
	}
	deleteErr := p.inner.Delete(ctx, id)
	if deleteErr != nil && !errors.Is(deleteErr, backend.ErrNotFound) {
		return deleteErr
	}
	if !pooled {
		return nil
	}
	if err := p.remove(current.ID); err != nil {
		return err
	}
	// Public deletion has already succeeded. Refill latency is paid by the
	// cleanup request so the next workload wave sees restored capacity, but a
	// refill failure must not turn confirmed customer cleanup into ambiguity.
	p.refillAfterFailure(ctx)
	return nil
}

func (p *Pool) Checkpoint(ctx context.Context, id string, kind domain.CheckpointKind, name string) (backend.Checkpoint, error) {
	return p.inner.Checkpoint(ctx, id, kind, name)
}

func (p *Pool) DeleteCheckpoint(ctx context.Context, ref string) error {
	return p.inner.DeleteCheckpoint(ctx, ref)
}

func (p *Pool) Ready(ctx context.Context) error {
	ready, ok := p.inner.(backend.ReadinessBackend)
	if !ok {
		return errors.New("warm pool backend does not expose readiness")
	}
	if err := ready.Ready(ctx); err != nil {
		return err
	}
	var one int
	return p.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

func (p *Pool) Run(ctx context.Context, sandboxID string, request backend.CommandRequest, emit func(backend.CommandEvent) error) error {
	runtime, ok := p.inner.(backend.GuestRuntime)
	if !ok {
		return backend.ErrCapabilityUnavailable
	}
	return runtime.Run(ctx, sandboxID, request, emit)
}

func (p *Pool) WriteFile(ctx context.Context, sandboxID, path string, source io.Reader) (backend.FileInfo, error) {
	runtime, ok := p.inner.(backend.GuestRuntime)
	if !ok {
		return backend.FileInfo{}, backend.ErrCapabilityUnavailable
	}
	return runtime.WriteFile(ctx, sandboxID, path, source)
}

func (p *Pool) ReadFile(ctx context.Context, sandboxID, path string, destination io.Writer) (backend.FileInfo, error) {
	runtime, ok := p.inner.(backend.GuestRuntime)
	if !ok {
		return backend.FileInfo{}, backend.ErrCapabilityUnavailable
	}
	return runtime.ReadFile(ctx, sandboxID, path, destination)
}

func (p *Pool) ValidatePort(port uint16) error {
	runtime, ok := p.inner.(backend.PortRuntime)
	if !ok {
		return backend.ErrCapabilityUnavailable
	}
	return runtime.ValidatePort(port)
}

func (p *Pool) RoundTripPort(ctx context.Context, sandboxID string, port uint16, request *http.Request) (*http.Response, error) {
	runtime, ok := p.inner.(backend.PortRuntime)
	if !ok {
		return nil, backend.ErrCapabilityUnavailable
	}
	return runtime.RoundTripPort(ctx, sandboxID, port, request)
}

func (p *Pool) CreateWorkspace(ctx context.Context, request backend.WorkspaceCreateRequest) (backend.Workspace, error) {
	runtime, ok := p.inner.(backend.WorkspaceRuntime)
	if !ok {
		return backend.Workspace{}, backend.ErrCapabilityUnavailable
	}
	return runtime.CreateWorkspace(ctx, request)
}

func (p *Pool) FindWorkspace(ctx context.Context, name string) (backend.Workspace, error) {
	runtime, ok := p.inner.(backend.WorkspaceRuntime)
	if !ok {
		return backend.Workspace{}, backend.ErrCapabilityUnavailable
	}
	return runtime.FindWorkspace(ctx, name)
}

func (p *Pool) DeleteWorkspace(ctx context.Context, id string) error {
	runtime, ok := p.inner.(backend.WorkspaceRuntime)
	if !ok {
		return backend.ErrCapabilityUnavailable
	}
	return runtime.DeleteWorkspace(ctx, id)
}

func (p *Pool) recover(ctx context.Context) error {
	rows, err := p.listStates(stateCreating, stateAvailable, stateDeleting)
	if err != nil {
		return err
	}
	for _, current := range rows {
		switch current.State {
		case stateCreating:
			if err := p.recoverCreating(ctx, current); err != nil {
				return err
			}
		case stateAvailable:
			if time.Until(current.ExpiresAt) < p.config.MaxClaimTTL+claimSafety {
				if err := p.discard(ctx, current); err != nil {
					return err
				}
				continue
			}
			observed, inspectErr := p.inner.Inspect(ctx, current.BackendID)
			if inspectErr == nil && observed.State == domain.SandboxRunning {
				continue
			}
			if inspectErr != nil && !errors.Is(inspectErr, backend.ErrNotFound) {
				return inspectErr
			}
			if err := p.discard(ctx, current); err != nil {
				return err
			}
		case stateDeleting:
			if err := p.discard(ctx, current); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Pool) recoverCreating(ctx context.Context, current slot) error {
	observed, findErr := p.inner.Find(ctx, current.LocalID, poolProjectID)
	if findErr == nil && observed.State == domain.SandboxRunning {
		return p.markAvailable(current.ID, observed.ID)
	}
	if errors.Is(findErr, backend.ErrNotFound) {
		return p.remove(current.ID)
	}
	if findErr != nil {
		return fmt.Errorf("recover creating warm slot: %w", findErr)
	}
	// A backend resource which exists but is not command-ready cannot be
	// published as warm capacity. Destroy it before replacing the slot.
	if observed.ID != "" {
		if deleteErr := p.inner.Delete(ctx, observed.ID); deleteErr != nil && !errors.Is(deleteErr, backend.ErrNotFound) {
			return fmt.Errorf("recover unusable warm slot: %w", deleteErr)
		}
	}
	return p.remove(current.ID)
}

func (p *Pool) reserveMissing() ([]slot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("warm pool is closed")
	}
	var count int
	if err := p.db.QueryRow("SELECT COUNT(*) FROM warm_slots").Scan(&count); err != nil {
		return nil, err
	}
	missing := p.config.Target - count
	if missing <= 0 {
		return nil, nil
	}
	now := time.Now().UTC()
	reserved := make([]slot, 0, missing)
	tx, err := p.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for i := 0; i < missing; i++ {
		id, err := randomID("wps")
		if err != nil {
			return nil, err
		}
		localID, err := randomID("warm")
		if err != nil {
			return nil, err
		}
		current := slot{ID: id, LocalID: localID, State: stateCreating, CreatedAt: now, ExpiresAt: now.Add(p.config.SlotTTL)}
		if _, err := tx.Exec(`INSERT INTO warm_slots(id, local_id, state, created_at, expires_at) VALUES(?,?,?,?,?)`, current.ID, current.LocalID, current.State, current.CreatedAt.UnixNano(), current.ExpiresAt.UnixNano()); err != nil {
			return nil, err
		}
		reserved = append(reserved, current)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return reserved, nil
}

func (p *Pool) createReserved(ctx context.Context, reserved []slot) error {
	if len(reserved) == 0 {
		return nil
	}
	limit := p.config.PrimeConcurrency
	if limit > len(reserved) {
		limit = len(reserved)
	}
	sem := make(chan struct{}, limit)
	errCh := make(chan error, len(reserved))
	var wg sync.WaitGroup
	for _, current := range reserved {
		current := current
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
			defer func() { <-sem }()
			if err := p.createSlot(ctx, current); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	var result error
	for err := range errCh {
		result = errors.Join(result, err)
	}
	return result
}

func (p *Pool) createSlot(ctx context.Context, current slot) error {
	remote, err := p.inner.Create(ctx, backend.CreateRequest{
		LocalSandboxID: current.LocalID,
		ProjectID:      poolProjectID,
		TemplateID:     p.config.TemplateID,
		Lifecycle:      domain.Lifecycle{ExpiresAfterSeconds: int64(p.config.SlotTTL / time.Second), StandbyCheckpoint: domain.CheckpointFullState},
		Network:        p.config.Network,
	})
	if err != nil {
		recovered, findErr := p.inner.Find(context.WithoutCancel(ctx), current.LocalID, poolProjectID)
		if findErr == nil && recovered.State == domain.SandboxRunning {
			return p.markAvailable(current.ID, recovered.ID)
		}
		if errors.Is(findErr, backend.ErrNotFound) {
			_ = p.remove(current.ID)
		}
		return fmt.Errorf("create warm slot: %w", errors.Join(err, findErr))
	}
	if remote.ID == "" || remote.State != domain.SandboxRunning {
		if remote.ID != "" {
			_ = p.inner.Delete(context.WithoutCancel(ctx), remote.ID)
		}
		_ = p.remove(current.ID)
		return errors.New("warm slot backend did not return a running sandbox")
	}
	return p.markAvailable(current.ID, remote.ID)
}

func (p *Pool) beginClaim(projectID, sandboxID string, ttl time.Duration) (slot, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	deadline := time.Now().UTC().Add(ttl + claimSafety).UnixNano()
	tx, err := p.db.Begin()
	if err != nil {
		return slot{}, false, err
	}
	defer tx.Rollback()
	var current slot
	var created, expires int64
	err = tx.QueryRow(`SELECT id, backend_id, local_id, state, created_at, expires_at FROM warm_slots WHERE state = ? AND expires_at >= ? ORDER BY created_at LIMIT 1`, stateAvailable, deadline).Scan(&current.ID, &current.BackendID, &current.LocalID, &current.State, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return slot{}, false, nil
	}
	if err != nil {
		return slot{}, false, err
	}
	current.CreatedAt, current.ExpiresAt = time.Unix(0, created).UTC(), time.Unix(0, expires).UTC()
	claimExpiresAt := time.Now().UTC().Add(ttl)
	result, err := tx.Exec(`UPDATE warm_slots SET state = ?, claim_project_id = ?, claim_sandbox_id = ?, claim_expires_at = ? WHERE id = ? AND state = ?`, stateClaiming, projectID, sandboxID, claimExpiresAt.UnixNano(), current.ID, stateAvailable)
	if err != nil {
		return slot{}, false, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return slot{}, false, errors.New("warm slot claim was not exclusive")
	}
	if err := tx.Commit(); err != nil {
		return slot{}, false, err
	}
	current.State, current.ClaimProjectID, current.ClaimSandboxID, current.ClaimExpiresAt = stateClaiming, projectID, sandboxID, claimExpiresAt
	return current, true, nil
}

func (p *Pool) finishClaim(id, projectID, sandboxID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	result, err := p.db.Exec(`UPDATE warm_slots SET state = ? WHERE id = ? AND state = ? AND claim_project_id = ? AND claim_sandbox_id = ?`, stateClaimed, id, stateClaiming, projectID, sandboxID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("warm slot claim changed before confirmation")
	}
	return nil
}

func (p *Pool) markAvailable(id, backendID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	result, err := p.db.Exec(`UPDATE warm_slots SET backend_id = ?, state = ? WHERE id = ? AND state = ?`, backendID, stateAvailable, id, stateCreating)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("warm slot reservation changed before create completed")
	}
	return nil
}

func (p *Pool) retireAvailable(id string) (bool, error) {
	return p.markDeleting(id, stateAvailable)
}

func (p *Pool) markDeleting(id, fromState string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	result, err := p.db.Exec(`UPDATE warm_slots SET state = ? WHERE id = ? AND state = ?`, stateDeleting, id, fromState)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (p *Pool) findClaim(projectID, sandboxID string) (slot, bool, error) {
	return p.queryOne(`SELECT id, backend_id, local_id, state, claim_project_id, claim_sandbox_id, created_at, expires_at, claim_expires_at FROM warm_slots WHERE claim_project_id = ? AND claim_sandbox_id = ? AND state IN (?,?)`, projectID, sandboxID, stateClaiming, stateClaimed)
}

func (p *Pool) findBackend(backendID string) (slot, bool, error) {
	return p.queryOne(`SELECT id, backend_id, local_id, state, claim_project_id, claim_sandbox_id, created_at, expires_at, claim_expires_at FROM warm_slots WHERE backend_id = ?`, backendID)
}

func (p *Pool) queryOne(query string, args ...any) (slot, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var current slot
	var created, expires, claimExpires int64
	err := p.db.QueryRow(query, args...).Scan(&current.ID, &current.BackendID, &current.LocalID, &current.State, &current.ClaimProjectID, &current.ClaimSandboxID, &created, &expires, &claimExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return slot{}, false, nil
	}
	if err != nil {
		return slot{}, false, err
	}
	current.CreatedAt, current.ExpiresAt = time.Unix(0, created).UTC(), time.Unix(0, expires).UTC()
	if claimExpires > 0 {
		current.ClaimExpiresAt = time.Unix(0, claimExpires).UTC()
	}
	return current, true, nil
}

func (p *Pool) listStates(states ...string) ([]slot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	query := `SELECT id, backend_id, local_id, state, claim_project_id, claim_sandbox_id, created_at, expires_at, claim_expires_at FROM warm_slots WHERE state IN (`
	args := make([]any, len(states))
	for i, state := range states {
		if i > 0 {
			query += ","
		}
		query += "?"
		args[i] = state
	}
	query += `) ORDER BY created_at`
	rows, err := p.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []slot
	for rows.Next() {
		var current slot
		var created, expires, claimExpires int64
		if err := rows.Scan(&current.ID, &current.BackendID, &current.LocalID, &current.State, &current.ClaimProjectID, &current.ClaimSandboxID, &created, &expires, &claimExpires); err != nil {
			return nil, err
		}
		current.CreatedAt, current.ExpiresAt = time.Unix(0, created).UTC(), time.Unix(0, expires).UTC()
		if claimExpires > 0 {
			current.ClaimExpiresAt = time.Unix(0, claimExpires).UTC()
		}
		result = append(result, current)
	}
	return result, rows.Err()
}

func (p *Pool) discard(ctx context.Context, current slot) error {
	if current.State != stateDeleting {
		quarantined, err := p.markDeleting(current.ID, current.State)
		if err != nil {
			return fmt.Errorf("quarantine warm slot before destruction: %w", err)
		}
		if !quarantined {
			return errors.New("warm slot ownership changed before destruction")
		}
	}
	if current.BackendID != "" {
		if err := p.inner.Delete(ctx, current.BackendID); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return err
		}
	}
	return p.remove(current.ID)
}

func (p *Pool) remove(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.db.Exec(`DELETE FROM warm_slots WHERE id = ?`, id)
	return err
}

func randomID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(value[:]), nil
}

func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	var result error
	if p.db != nil {
		result = p.db.Close()
	}
	if p.lockFile != nil {
		result = errors.Join(result, syscall.Flock(int(p.lockFile.Fd()), syscall.LOCK_UN), p.lockFile.Close())
	}
	return result
}

var (
	_ backend.Backend          = (*Pool)(nil)
	_ backend.ReadinessBackend = (*Pool)(nil)
	_ backend.GuestRuntime     = (*Pool)(nil)
	_ backend.PortRuntime      = (*Pool)(nil)
	_ backend.WorkspaceRuntime = (*Pool)(nil)
)
