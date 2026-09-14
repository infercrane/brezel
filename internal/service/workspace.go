package service

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/infercrane/sandbox-runtime-lab/internal/backend"
	"github.com/infercrane/sandbox-runtime-lab/internal/domain"
	"github.com/infercrane/sandbox-runtime-lab/internal/store"
)

type CreateWorkspaceInput struct {
	Name string `json:"name"`
}

func (s *Service) CreateWorkspace(ctx context.Context, projectID, idempotencyKey string, in CreateWorkspaceInput) (domain.Workspace, domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := requireMutation(projectID, idempotencyKey); err != nil {
		return domain.Workspace{}, domain.Operation{}, err
	}
	if err := domain.ValidateWorkspaceName(in.Name); err != nil {
		return domain.Workspace{}, domain.Operation{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	runtime, ok := s.backend.(backend.WorkspaceRuntime)
	if !ok || !s.Capabilities().DurableWorkspaces {
		return domain.Workspace{}, domain.Operation{}, fmt.Errorf("%w: durable workspaces are unavailable", ErrDenied)
	}
	inputDigest, err := mutationDigest(in)
	if err != nil {
		return domain.Workspace{}, domain.Operation{}, err
	}
	if existing, ok, lookupErr := s.lookupIdempotencyInput(projectID, "create_workspace", idempotencyKey, inputDigest); lookupErr != nil {
		return domain.Workspace{}, domain.Operation{}, lookupErr
	} else if ok {
		workspace, err := s.getWorkspace(projectID, existing.ResourceID)
		return workspace, existing, err
	}
	active := 0
	if err := s.store.View(func(state store.State) error {
		for _, workspace := range state.Workspaces {
			if workspace.ProjectID == projectID && !terminalWorkspace(workspace.State) {
				active++
			}
		}
		return nil
	}); err != nil {
		return domain.Workspace{}, domain.Operation{}, err
	}
	if active >= s.limits.MaxWorkspacesPerProject {
		return domain.Workspace{}, domain.Operation{}, ErrQuota
	}

	now := s.now()
	workspaceID, err := randomID("wrk")
	if err != nil {
		return domain.Workspace{}, domain.Operation{}, err
	}
	opID, err := randomID("op")
	if err != nil {
		return domain.Workspace{}, domain.Operation{}, err
	}
	workspace := domain.Workspace{
		ID: workspaceID, ProjectID: projectID, Name: in.Name, BackendName: workspaceID,
		State: domain.WorkspacePreparing, CreatedAt: now, UpdatedAt: now,
	}
	op := domain.Operation{
		ID: opID, ProjectID: projectID, Kind: "create_workspace", ResourceID: workspaceID,
		State: domain.OperationRunning, IdempotencyKey: idempotencyKey, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.Update(func(state *store.State) error {
		if existing, ok := idempotentOperation(*state, projectID, "create_workspace", idempotencyKey); ok {
			if err := requireIdempotencyDigest(*state, projectID, "create_workspace", idempotencyKey, inputDigest); err != nil {
				return err
			}
			op = existing
			workspace = state.Workspaces[store.ScopedKey(projectID, existing.ResourceID)]
			return nil
		}
		state.Workspaces[store.ScopedKey(projectID, workspaceID)] = workspace
		state.Operations[store.ScopedKey(projectID, opID)] = op
		state.Idempotency[store.IdempotencyKey(projectID, "create_workspace", idempotencyKey)] = opID
		state.IdempotencyDigests[store.IdempotencyKey(projectID, "create_workspace", idempotencyKey)] = inputDigest
		return nil
	}); err != nil {
		return workspace, op, err
	}
	if op.ResourceID != workspaceID {
		return workspace, op, nil
	}

	remote, backendErr := runtime.CreateWorkspace(ctx, backend.WorkspaceCreateRequest{
		LocalWorkspaceID: workspace.ID,
		ProjectID:        projectID,
		Name:             workspace.Name,
	})
	if backendErr != nil {
		failure := &domain.Failure{Code: "backend_workspace_create_unconfirmed", Message: "workspace engine did not confirm whether the resource was created", Retryable: true}
		now = s.now()
		workspace.State, workspace.Failure, workspace.UpdatedAt = domain.WorkspaceUnknown, failure, now
		op.State, op.Failure, op.UpdatedAt = domain.OperationFailed, failure, now
		_ = s.store.Update(func(state *store.State) error {
			state.Workspaces[store.ScopedKey(projectID, workspace.ID)] = workspace
			state.Operations[store.ScopedKey(projectID, op.ID)] = op
			return nil
		})
		return workspace, op, fmt.Errorf("%w: create workspace", ErrBackend)
	}
	if remote.ID == "" || remote.Name != workspace.BackendName {
		failure := &domain.Failure{Code: "backend_workspace_identity_invalid", Message: "workspace engine returned an invalid resource identity", Retryable: true}
		now = s.now()
		workspace.State, workspace.Failure, workspace.UpdatedAt = domain.WorkspaceUnknown, failure, now
		op.State, op.Failure, op.UpdatedAt = domain.OperationFailed, failure, now
		_ = s.store.Update(func(state *store.State) error {
			state.Workspaces[store.ScopedKey(projectID, workspace.ID)] = workspace
			state.Operations[store.ScopedKey(projectID, op.ID)] = op
			return nil
		})
		return workspace, op, fmt.Errorf("%w: create workspace", ErrBackend)
	}

	now = s.now()
	workspace.BackendID, workspace.State, workspace.UpdatedAt, workspace.Failure = remote.ID, domain.WorkspaceReady, now, nil
	op.State, op.UpdatedAt, op.Failure = domain.OperationSucceeded, now, nil
	err = s.store.Update(func(state *store.State) error {
		state.Workspaces[store.ScopedKey(projectID, workspace.ID)] = workspace
		state.Operations[store.ScopedKey(projectID, op.ID)] = op
		return nil
	})
	return workspace, op, err
}

func (s *Service) GetWorkspace(projectID, id string) (domain.Workspace, error) {
	if err := domain.ValidateProjectID(projectID); err != nil {
		return domain.Workspace{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return s.getWorkspace(projectID, id)
}

func (s *Service) ListWorkspaces(projectID string, includeTerminal bool) ([]domain.Workspace, error) {
	if err := domain.ValidateProjectID(projectID); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var out []domain.Workspace
	err := s.store.View(func(state store.State) error {
		for _, workspace := range state.Workspaces {
			if workspace.ProjectID != projectID || (!includeTerminal && terminalWorkspace(workspace.State)) {
				continue
			}
			out = append(out, workspace)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (s *Service) DeleteWorkspace(ctx context.Context, projectID, id, idempotencyKey string) (domain.Workspace, domain.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := requireMutation(projectID, idempotencyKey); err != nil {
		return domain.Workspace{}, domain.Operation{}, err
	}
	runtime, ok := s.backend.(backend.WorkspaceRuntime)
	if !ok || !s.Capabilities().DurableWorkspaces {
		return domain.Workspace{}, domain.Operation{}, fmt.Errorf("%w: durable workspaces are unavailable", ErrDenied)
	}
	workspace, err := s.getWorkspace(projectID, id)
	if err != nil {
		return workspace, domain.Operation{}, err
	}
	kind := "delete_workspace:" + id
	if existing, ok := s.lookupIdempotency(projectID, kind, idempotencyKey); ok {
		return workspace, existing, nil
	}
	if terminalWorkspace(workspace.State) {
		return workspace, domain.Operation{}, fmt.Errorf("%w: workspace is already terminal", ErrConflict)
	}
	if s.workspaceAttachedLocked(projectID, id) {
		return workspace, domain.Operation{}, fmt.Errorf("%w: workspace is attached to an active sandbox", ErrConflict)
	}

	now := s.now()
	opID, err := randomID("op")
	if err != nil {
		return workspace, domain.Operation{}, err
	}
	workspace.State, workspace.CleanupTarget, workspace.UpdatedAt, workspace.Failure = domain.WorkspaceDeleting, domain.WorkspaceDeleted, now, nil
	op := domain.Operation{
		ID: opID, ProjectID: projectID, Kind: kind, ResourceID: id, State: domain.OperationRunning,
		IdempotencyKey: idempotencyKey, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.Update(func(state *store.State) error {
		state.Workspaces[store.ScopedKey(projectID, id)] = workspace
		state.Operations[store.ScopedKey(projectID, opID)] = op
		state.Idempotency[store.IdempotencyKey(projectID, kind, idempotencyKey)] = opID
		return nil
	}); err != nil {
		return workspace, op, err
	}
	return s.reconcileWorkspaceCleanupLocked(ctx, runtime, workspace, op)
}

func (s *Service) reconcileWorkspacesLocked(ctx context.Context) error {
	runtime, ok := s.backend.(backend.WorkspaceRuntime)
	if !ok || !s.Capabilities().DurableWorkspaces {
		return nil
	}
	var workspaces []domain.Workspace
	if err := s.store.View(func(state store.State) error {
		for _, workspace := range state.Workspaces {
			if !terminalWorkspace(workspace.State) && workspace.State != domain.WorkspaceReady {
				workspaces = append(workspaces, workspace)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, workspace := range workspaces {
		if workspace.CleanupTarget == domain.WorkspaceDeleted || workspace.State == domain.WorkspaceDeleting {
			op := s.latestOperationLocked(workspace.ProjectID, workspace.ID, "delete_workspace:")
			_, _, _ = s.reconcileWorkspaceCleanupLocked(ctx, runtime, workspace, op)
			continue
		}
		remote, err := runtime.FindWorkspace(ctx, workspace.BackendName)
		if err != nil {
			now := s.now()
			if errors.Is(err, backend.ErrNotFound) {
				workspace.State = domain.WorkspaceFailed
				workspace.Failure = &domain.Failure{Code: "backend_workspace_missing", Message: "workspace engine confirmed the resource is absent", Retryable: false}
			} else {
				workspace.State = domain.WorkspaceUnknown
				workspace.Failure = &domain.Failure{Code: "backend_workspace_reconcile_failed", Message: "workspace engine state could not be confirmed", Retryable: true}
			}
			workspace.UpdatedAt = now
			_ = s.store.Update(func(state *store.State) error {
				state.Workspaces[store.ScopedKey(workspace.ProjectID, workspace.ID)] = workspace
				return nil
			})
			continue
		}
		now := s.now()
		workspace.BackendID, workspace.BackendName = remote.ID, remote.Name
		workspace.State, workspace.UpdatedAt, workspace.Failure = domain.WorkspaceReady, now, nil
		_ = s.store.Update(func(state *store.State) error {
			state.Workspaces[store.ScopedKey(workspace.ProjectID, workspace.ID)] = workspace
			completeLatestOperation(state, workspace.ProjectID, workspace.ID, "create_workspace", now)
			return nil
		})
	}
	return nil
}

func (s *Service) reconcileWorkspaceCleanupLocked(ctx context.Context, runtime backend.WorkspaceRuntime, workspace domain.Workspace, op domain.Operation) (domain.Workspace, domain.Operation, error) {
	if workspace.BackendID == "" {
		remote, err := runtime.FindWorkspace(ctx, workspace.BackendName)
		switch {
		case err == nil:
			workspace.BackendID = remote.ID
		case errors.Is(err, backend.ErrNotFound):
			// Absence is confirmation only after durable deletion intent exists.
		default:
			return s.failWorkspaceCleanupLocked(workspace, op, "backend_workspace_cleanup_recovery_failed")
		}
	}
	if workspace.BackendID != "" {
		if err := runtime.DeleteWorkspace(ctx, workspace.BackendID); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return s.failWorkspaceCleanupLocked(workspace, op, "backend_workspace_delete_unconfirmed")
		}
	}
	now := s.now()
	workspace.State, workspace.UpdatedAt, workspace.Failure = domain.WorkspaceDeleted, now, nil
	if op.ID != "" {
		op.State, op.UpdatedAt, op.Failure = domain.OperationSucceeded, now, nil
	}
	err := s.store.Update(func(state *store.State) error {
		state.Workspaces[store.ScopedKey(workspace.ProjectID, workspace.ID)] = workspace
		if op.ID != "" {
			state.Operations[store.ScopedKey(op.ProjectID, op.ID)] = op
		} else {
			completeLatestOperation(state, workspace.ProjectID, workspace.ID, "delete_workspace:", now)
		}
		return nil
	})
	return workspace, op, err
}

func (s *Service) failWorkspaceCleanupLocked(workspace domain.Workspace, op domain.Operation, code string) (domain.Workspace, domain.Operation, error) {
	now := s.now()
	failure := &domain.Failure{Code: code, Message: "workspace engine did not confirm cleanup", Retryable: true}
	workspace.State, workspace.UpdatedAt, workspace.Failure = domain.WorkspaceUnknown, now, failure
	if op.ID != "" {
		op.State, op.UpdatedAt, op.Failure = domain.OperationFailed, now, failure
	}
	_ = s.store.Update(func(state *store.State) error {
		state.Workspaces[store.ScopedKey(workspace.ProjectID, workspace.ID)] = workspace
		if op.ID != "" {
			state.Operations[store.ScopedKey(op.ProjectID, op.ID)] = op
		}
		return nil
	})
	return workspace, op, fmt.Errorf("%w: delete workspace", ErrBackend)
}

func (s *Service) getWorkspace(projectID, id string) (domain.Workspace, error) {
	var out domain.Workspace
	err := s.store.View(func(state store.State) error {
		var ok bool
		out, ok = state.Workspaces[store.ScopedKey(projectID, id)]
		if !ok {
			return store.ErrNotFound
		}
		return nil
	})
	return out, translateStore(err)
}

func (s *Service) workspaceAttachedLocked(projectID, workspaceID string) bool {
	attached := false
	_ = s.store.View(func(state store.State) error {
		for _, sandbox := range state.Sandboxes {
			if sandbox.ProjectID != projectID || terminal(sandbox.State) {
				continue
			}
			for _, mount := range sandbox.WorkspaceMounts {
				if mount.WorkspaceID == workspaceID {
					attached = true
					return nil
				}
			}
		}
		return nil
	})
	return attached
}

func (s *Service) latestOperationLocked(projectID, resourceID, kindPrefix string) domain.Operation {
	var selected domain.Operation
	_ = s.store.View(func(state store.State) error {
		for _, operation := range state.Operations {
			if operation.ProjectID != projectID || operation.ResourceID != resourceID || len(operation.Kind) < len(kindPrefix) || operation.Kind[:len(kindPrefix)] != kindPrefix {
				continue
			}
			if selected.ID == "" || selected.UpdatedAt.Before(operation.UpdatedAt) {
				selected = operation
			}
		}
		return nil
	})
	return selected
}

func terminalWorkspace(state domain.WorkspaceState) bool {
	return state == domain.WorkspaceDeleted || state == domain.WorkspaceFailed
}
