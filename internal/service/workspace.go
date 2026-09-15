package service

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/infercrane/brezel/internal/backend"
	"github.com/infercrane/brezel/internal/domain"
	"github.com/infercrane/brezel/internal/store"
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
	existing, active, err := s.admitWorkspaceCreate(projectID, idempotencyKey, inputDigest)
	if err != nil {
		return domain.Workspace{}, domain.Operation{}, err
	}
	if existing.ID != "" {
		workspace, err := s.getWorkspace(projectID, existing.ResourceID)
		return workspace, existing, err
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
	if err := store.UpdateRows(s.store, store.MutationScope{
		Workspaces:  []string{store.ScopedKey(projectID, workspaceID)},
		Operations:  []string{store.ScopedKey(projectID, opID)},
		Idempotency: []string{store.IdempotencyKey(projectID, "create_workspace", idempotencyKey)},
	}, func(state *store.State) error {
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
		_ = store.UpdateRows(s.store, store.MutationScope{
			Workspaces: []string{store.ScopedKey(projectID, workspace.ID)},
			Operations: []string{store.ScopedKey(projectID, op.ID)},
		}, func(state *store.State) error {
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
		_ = store.UpdateRows(s.store, store.MutationScope{
			Workspaces: []string{store.ScopedKey(projectID, workspace.ID)},
			Operations: []string{store.ScopedKey(projectID, op.ID)},
		}, func(state *store.State) error {
			state.Workspaces[store.ScopedKey(projectID, workspace.ID)] = workspace
			state.Operations[store.ScopedKey(projectID, op.ID)] = op
			return nil
		})
		return workspace, op, fmt.Errorf("%w: create workspace", ErrBackend)
	}

	now = s.now()
	workspace.BackendID, workspace.State, workspace.UpdatedAt, workspace.Failure = remote.ID, domain.WorkspaceReady, now, nil
	op.State, op.UpdatedAt, op.Failure = domain.OperationSucceeded, now, nil
	err = store.UpdateRows(s.store, store.MutationScope{
		Workspaces: []string{store.ScopedKey(projectID, workspace.ID)},
		Operations: []string{store.ScopedKey(projectID, op.ID)},
	}, func(state *store.State) error {
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
	if _, active := s.activeWorkspaceMutations[store.ScopedKey(projectID, id)]; active {
		return workspace, domain.Operation{}, fmt.Errorf("%w: workspace has an active backend mutation", ErrConflict)
	}
	if terminalWorkspace(workspace.State) {
		return workspace, domain.Operation{}, fmt.Errorf("%w: workspace is already terminal", ErrConflict)
	}
	attached, err := s.workspaceAttachedLocked(projectID, id)
	if err != nil {
		return workspace, domain.Operation{}, err
	}
	if attached {
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
	if err := store.UpdateRows(s.store, store.MutationScope{
		Workspaces:  []string{store.ScopedKey(projectID, id)},
		Operations:  []string{store.ScopedKey(projectID, opID)},
		Idempotency: []string{store.IdempotencyKey(projectID, kind, idempotencyKey)},
	}, func(state *store.State) error {
		state.Workspaces[store.ScopedKey(projectID, id)] = workspace
		state.Operations[store.ScopedKey(projectID, opID)] = op
		state.Idempotency[store.IdempotencyKey(projectID, kind, idempotencyKey)] = opID
		return nil
	}); err != nil {
		return workspace, op, err
	}
	return s.reconcileWorkspaceCleanupLocked(ctx, runtime, workspace, op)
}

func (s *Service) reconcileWorkspaces(ctx context.Context, operationsByResource map[string][]domain.Operation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	runtime, ok := s.backend.(backend.WorkspaceRuntime)
	if !ok || !s.Capabilities().DurableWorkspaces {
		return nil
	}
	workspaces, err := store.ListWorkspacesForReconcile(s.store)
	if err != nil {
		return err
	}
	var reconcileErr error
	for _, workspace := range workspaces {
		if err := ctx.Err(); err != nil {
			return errors.Join(reconcileErr, err)
		}
		key := store.ScopedKey(workspace.ProjectID, workspace.ID)
		if err := s.reconcileWorkspace(ctx, runtime, workspace.ProjectID, workspace.ID, operationsByResource[key]); err != nil {
			reconcileErr = errors.Join(reconcileErr, err)
		}
	}
	return reconcileErr
}

func (s *Service) reconcileWorkspace(ctx context.Context, runtime backend.WorkspaceRuntime, projectID, workspaceID string, operationCandidates []domain.Operation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := store.ScopedKey(projectID, workspaceID)
	s.mu.Lock()
	current, err := s.getWorkspace(projectID, workspaceID)
	if err != nil {
		s.mu.Unlock()
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if terminalWorkspace(current.State) {
		s.mu.Unlock()
		return nil
	}
	if _, active := s.activeWorkspaceMutations[key]; active {
		s.mu.Unlock()
		return nil
	}
	expected := current
	s.activeWorkspaceMutations[key] = struct{}{}
	s.mu.Unlock()

	cleanup := expected.CleanupTarget == domain.WorkspaceDeleted || expected.State == domain.WorkspaceDeleting
	observed := expected
	failureCode := ""
	var backendErr error
	if cleanup {
		if observed.BackendID == "" {
			remote, findErr := runtime.FindWorkspace(ctx, observed.BackendName)
			switch {
			case findErr == nil && validWorkspaceIdentity(observed, remote):
				observed.BackendID = remote.ID
			case findErr == nil:
				failureCode, backendErr = "backend_workspace_identity_invalid", errors.New("workspace backend returned an invalid identity")
			case errors.Is(findErr, backend.ErrNotFound):
				// Durable deletion intent makes confirmed absence successful.
			default:
				failureCode, backendErr = "backend_workspace_cleanup_recovery_failed", findErr
			}
		}
		if backendErr == nil && observed.BackendID != "" {
			if deleteErr := runtime.DeleteWorkspace(ctx, observed.BackendID); deleteErr != nil && !errors.Is(deleteErr, backend.ErrNotFound) {
				failureCode, backendErr = "backend_workspace_delete_unconfirmed", deleteErr
			}
		}
	} else {
		remote, findErr := runtime.FindWorkspace(ctx, observed.BackendName)
		switch {
		case findErr == nil && validWorkspaceIdentity(observed, remote):
			observed.BackendID, observed.BackendName = remote.ID, remote.Name
		case findErr == nil:
			failureCode, backendErr = "backend_workspace_identity_invalid", errors.New("workspace backend returned an invalid identity")
		case errors.Is(findErr, backend.ErrNotFound):
			failureCode, backendErr = "backend_workspace_missing", backend.ErrNotFound
		default:
			failureCode, backendErr = "backend_workspace_reconcile_failed", findErr
		}
	}

	s.mu.Lock()
	delete(s.activeWorkspaceMutations, key)
	if ctxErr := ctx.Err(); ctxErr != nil {
		s.mu.Unlock()
		return ctxErr
	}
	latest, err := s.getWorkspace(projectID, workspaceID)
	if err != nil {
		s.mu.Unlock()
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if !sameWorkspaceReconcileVersion(latest, expected) || terminalWorkspace(latest.State) {
		s.mu.Unlock()
		return nil
	}

	now := s.now()
	var operations []domain.Operation
	var operationState domain.OperationState
	switch {
	case cleanup && backendErr == nil:
		observed.State, observed.UpdatedAt, observed.Failure = domain.WorkspaceDeleted, now, nil
		operations = latestIncompleteOperations(operationCandidates, "delete_workspace:")
		operationState = domain.OperationSucceeded
	case cleanup:
		failure := &domain.Failure{Code: failureCode, Message: "workspace engine did not confirm cleanup", Retryable: true}
		observed.State, observed.UpdatedAt, observed.Failure = domain.WorkspaceUnknown, now, failure
		operations = latestIncompleteOperations(operationCandidates, "delete_workspace:")
		operationState = domain.OperationFailed
	case errors.Is(backendErr, backend.ErrNotFound):
		failure := &domain.Failure{Code: failureCode, Message: "workspace engine confirmed the resource is absent", Retryable: false}
		observed.State, observed.UpdatedAt, observed.Failure = domain.WorkspaceFailed, now, failure
		operations = latestIncompleteOperations(operationCandidates, "create_workspace")
		operationState = domain.OperationFailed
	case backendErr != nil:
		failure := &domain.Failure{Code: failureCode, Message: "workspace engine state could not be confirmed", Retryable: true}
		observed.State, observed.UpdatedAt, observed.Failure = domain.WorkspaceUnknown, now, failure
	case backendErr == nil:
		observed.State, observed.UpdatedAt, observed.Failure = domain.WorkspaceReady, now, nil
		operations = latestIncompleteOperations(operationCandidates, "create_workspace")
		operationState = domain.OperationSucceeded
	}
	persistErr := s.persistWorkspaceObservationLocked(latest, observed, operations, operationState)
	s.mu.Unlock()
	if errors.Is(persistErr, errReconcileObservationSuperseded) {
		return nil
	}
	if backendErr != nil && !errors.Is(backendErr, backend.ErrNotFound) {
		return errors.Join(fmt.Errorf("%w: %s: %v", ErrBackend, failureCode, backendErr), persistErr)
	}
	return persistErr
}

func validWorkspaceIdentity(expected domain.Workspace, remote backend.Workspace) bool {
	return remote.ID != "" && remote.Name == expected.BackendName
}

func sameWorkspaceReconcileVersion(current, expected domain.Workspace) bool {
	return current.ID == expected.ID && current.ProjectID == expected.ProjectID &&
		current.BackendID == expected.BackendID && current.BackendName == expected.BackendName &&
		current.State == expected.State && current.CleanupTarget == expected.CleanupTarget &&
		current.UpdatedAt.Equal(expected.UpdatedAt) && sameFailure(current.Failure, expected.Failure)
}

func (s *Service) persistWorkspaceObservationLocked(current, observed domain.Workspace, operations []domain.Operation, operationState domain.OperationState) error {
	key := store.ScopedKey(current.ProjectID, current.ID)
	return store.UpdateRows(s.store, store.MutationScope{
		Workspaces: []string{key}, Operations: operationScope(operations),
	}, func(state *store.State) error {
		persisted, ok := state.Workspaces[key]
		if !ok || !sameWorkspaceReconcileVersion(persisted, current) {
			return errReconcileObservationSuperseded
		}
		state.Workspaces[key] = observed
		for _, operation := range operations {
			operationKey := store.ScopedKey(operation.ProjectID, operation.ID)
			persistedOperation, ok := state.Operations[operationKey]
			if !ok || !sameOperationVersion(persistedOperation, operation) {
				return errReconcileObservationSuperseded
			}
			operation.State, operation.UpdatedAt = operationState, observed.UpdatedAt
			if operationState == domain.OperationSucceeded {
				operation.Failure = nil
			} else {
				operation.Failure = observed.Failure
			}
			state.Operations[operationKey] = operation
		}
		return nil
	})
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
	var err error
	if op.ID != "" {
		err = store.UpdateRows(s.store, store.MutationScope{
			Workspaces: []string{store.ScopedKey(workspace.ProjectID, workspace.ID)},
			Operations: []string{store.ScopedKey(op.ProjectID, op.ID)},
		}, func(state *store.State) error {
			state.Workspaces[store.ScopedKey(workspace.ProjectID, workspace.ID)] = workspace
			state.Operations[store.ScopedKey(op.ProjectID, op.ID)] = op
			return nil
		})
	} else {
		err = s.store.Update(func(state *store.State) error {
			state.Workspaces[store.ScopedKey(workspace.ProjectID, workspace.ID)] = workspace
			completeLatestOperation(state, workspace.ProjectID, workspace.ID, "delete_workspace:", now)
			return nil
		})
	}
	return workspace, op, err
}

func (s *Service) failWorkspaceCleanupLocked(workspace domain.Workspace, op domain.Operation, code string) (domain.Workspace, domain.Operation, error) {
	now := s.now()
	failure := &domain.Failure{Code: code, Message: "workspace engine did not confirm cleanup", Retryable: true}
	workspace.State, workspace.UpdatedAt, workspace.Failure = domain.WorkspaceUnknown, now, failure
	if op.ID != "" {
		op.State, op.UpdatedAt, op.Failure = domain.OperationFailed, now, failure
	}
	var persistErr error
	if op.ID != "" {
		persistErr = store.UpdateRows(s.store, store.MutationScope{
			Workspaces: []string{store.ScopedKey(workspace.ProjectID, workspace.ID)},
			Operations: []string{store.ScopedKey(op.ProjectID, op.ID)},
		}, func(state *store.State) error {
			state.Workspaces[store.ScopedKey(workspace.ProjectID, workspace.ID)] = workspace
			state.Operations[store.ScopedKey(op.ProjectID, op.ID)] = op
			return nil
		})
	} else {
		persistErr = s.store.Update(func(state *store.State) error {
			state.Workspaces[store.ScopedKey(workspace.ProjectID, workspace.ID)] = workspace
			return nil
		})
	}
	return workspace, op, errors.Join(fmt.Errorf("%w: delete workspace", ErrBackend), persistErr)
}

func (s *Service) getWorkspace(projectID, id string) (domain.Workspace, error) {
	if reader, ok := s.store.(store.WorkspaceReader); ok {
		workspace, err := reader.GetWorkspace(projectID, id)
		return workspace, translateStore(err)
	}
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

func (s *Service) workspaceAttachedLocked(projectID, workspaceID string) (bool, error) {
	if reader, ok := s.store.(store.WorkspaceAttachmentReader); ok {
		return reader.WorkspaceAttached(projectID, workspaceID)
	}
	attached := false
	err := s.store.View(func(state store.State) error {
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
	return attached, err
}

func (s *Service) admitWorkspaceCreate(projectID, idempotencyKey, inputDigest string) (domain.Operation, int, error) {
	const kind = "create_workspace"
	if reader, ok := s.store.(store.WorkspaceAdmissionReader); ok {
		snapshot, err := reader.ReadWorkspaceAdmission(store.WorkspaceAdmissionQuery{
			ProjectID: projectID, IdempotencyKind: kind, IdempotencyKey: idempotencyKey,
		})
		if err != nil {
			return domain.Operation{}, 0, err
		}
		if snapshot.Idempotency.Found {
			if snapshot.Idempotency.Digest != "" && snapshot.Idempotency.Digest != inputDigest {
				return domain.Operation{}, 0, fmt.Errorf("%w: Idempotency-Key was already used with a different request", ErrConflict)
			}
			return snapshot.Idempotency.Operation, 0, nil
		}
		return domain.Operation{}, snapshot.ActiveForProject, nil
	}
	var existing domain.Operation
	active := 0
	var lookupErr error
	err := s.store.View(func(state store.State) error {
		existing, _ = idempotentOperation(state, projectID, kind, idempotencyKey)
		if existing.ID != "" {
			lookupErr = requireIdempotencyDigest(state, projectID, kind, idempotencyKey, inputDigest)
			return nil
		}
		for _, workspace := range state.Workspaces {
			if workspace.ProjectID == projectID && !terminalWorkspace(workspace.State) {
				active++
			}
		}
		return nil
	})
	if err != nil {
		return domain.Operation{}, 0, err
	}
	if lookupErr != nil {
		return domain.Operation{}, 0, lookupErr
	}
	return existing, active, nil
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
