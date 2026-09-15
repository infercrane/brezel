package service

import (
	"fmt"

	"github.com/infercrane/brezel/internal/backend"
	"github.com/infercrane/brezel/internal/domain"
	"github.com/infercrane/brezel/internal/store"
)

// admitSandboxCreate resolves the exact dependency set for a sandbox create or
// filesystem-checkpoint restore. SQLite uses indexed reads; FileStore and
// custom stores retain their existing complete-state snapshot behavior.
func (s *Service) admitSandboxCreate(projectID, idempotencyKey, inputDigest string, in *CreateSandboxInput) (domain.Environment, domain.Checkpoint, []backend.WorkspaceMount, domain.Operation, error) {
	query := store.SandboxAdmissionQuery{
		ProjectID:           projectID,
		IdempotencyKind:     "create_sandbox",
		IdempotencyKey:      idempotencyKey,
		EnvironmentRevision: in.EnvironmentRevision,
		CheckpointID:        in.CheckpointID,
		ConnectorRevisions:  append([]string(nil), in.ConnectorRevisions...),
		WorkspaceIDs:        make([]string, 0, len(in.WorkspaceMounts)),
	}
	for _, mount := range in.WorkspaceMounts {
		query.WorkspaceIDs = append(query.WorkspaceIDs, mount.WorkspaceID)
	}

	var snapshot store.SandboxAdmissionSnapshot
	var err error
	if reader, ok := s.store.(store.SandboxAdmissionReader); ok {
		snapshot, err = reader.ReadSandboxAdmission(query)
	} else {
		snapshot, err = s.readSandboxAdmissionFallback(query)
	}
	if err != nil {
		return domain.Environment{}, domain.Checkpoint{}, nil, domain.Operation{}, translateStore(err)
	}
	if snapshot.Idempotency.Found {
		if snapshot.Idempotency.Digest != "" && snapshot.Idempotency.Digest != inputDigest {
			return domain.Environment{}, domain.Checkpoint{}, nil, domain.Operation{}, fmt.Errorf("%w: Idempotency-Key was already used with a different request", ErrConflict)
		}
		return domain.Environment{}, domain.Checkpoint{}, nil, snapshot.Idempotency.Operation, nil
	}
	if snapshot.ActiveForProject >= s.limits.MaxActiveSandboxesPerProject {
		return domain.Environment{}, domain.Checkpoint{}, nil, domain.Operation{}, ErrQuota
	}
	if snapshot.ActiveTotal >= s.limits.MaxActiveSandboxesTotal {
		return domain.Environment{}, domain.Checkpoint{}, nil, domain.Operation{}, ErrCapacity
	}

	var checkpoint domain.Checkpoint
	if in.CheckpointID != "" {
		if !snapshot.CheckpointFound {
			return domain.Environment{}, domain.Checkpoint{}, nil, domain.Operation{}, ErrNotFound
		}
		checkpoint = snapshot.Checkpoint
		if checkpoint.Kind != domain.CheckpointFilesystem || checkpoint.BackendRef == "" {
			return domain.Environment{}, domain.Checkpoint{}, nil, domain.Operation{}, fmt.Errorf("%w: checkpoint cannot create a new sandbox", ErrDenied)
		}
		in.EnvironmentRevision = checkpoint.EnvironmentRevision
	}
	if !snapshot.EnvironmentFound || snapshot.Environment.RevisionID != in.EnvironmentRevision {
		return domain.Environment{}, domain.Checkpoint{}, nil, domain.Operation{}, ErrNotFound
	}
	for _, revision := range in.ConnectorRevisions {
		if _, ok := snapshot.Connectors[revision]; !ok {
			return domain.Environment{}, domain.Checkpoint{}, nil, domain.Operation{}, ErrNotFound
		}
	}
	workspaceMounts := make([]backend.WorkspaceMount, 0, len(in.WorkspaceMounts))
	for _, mount := range in.WorkspaceMounts {
		workspace, ok := snapshot.Workspaces[mount.WorkspaceID]
		if !ok {
			return domain.Environment{}, domain.Checkpoint{}, nil, domain.Operation{}, ErrNotFound
		}
		if workspace.State != domain.WorkspaceReady || workspace.BackendName == "" {
			return domain.Environment{}, domain.Checkpoint{}, nil, domain.Operation{}, fmt.Errorf("%w: workspace %s is not ready", ErrConflict, mount.WorkspaceID)
		}
		if snapshot.AttachedWorkspaceIDs[mount.WorkspaceID] {
			return domain.Environment{}, domain.Checkpoint{}, nil, domain.Operation{}, fmt.Errorf("%w: workspace %s is already attached", ErrConflict, mount.WorkspaceID)
		}
		workspaceMounts = append(workspaceMounts, backend.WorkspaceMount{Name: workspace.BackendName, Path: mount.Path})
	}
	return snapshot.Environment, checkpoint, workspaceMounts, domain.Operation{}, nil
}

func (s *Service) readSandboxAdmissionFallback(query store.SandboxAdmissionQuery) (store.SandboxAdmissionSnapshot, error) {
	out := store.SandboxAdmissionSnapshot{
		Connectors:           make(map[string]domain.Connector, len(query.ConnectorRevisions)),
		Workspaces:           make(map[string]domain.Workspace, len(query.WorkspaceIDs)),
		AttachedWorkspaceIDs: make(map[string]bool, len(query.WorkspaceIDs)),
	}
	err := s.store.View(func(state store.State) error {
		if operation, ok := idempotentOperation(state, query.ProjectID, query.IdempotencyKind, query.IdempotencyKey); ok {
			indexKey := store.IdempotencyKey(query.ProjectID, query.IdempotencyKind, query.IdempotencyKey)
			out.Idempotency = store.IdempotencyLookup{Operation: operation, Digest: state.IdempotencyDigests[indexKey], Found: true}
			return nil
		}
		for _, sandbox := range state.Sandboxes {
			if terminal(sandbox.State) {
				continue
			}
			out.ActiveTotal++
			if sandbox.ProjectID != query.ProjectID {
				continue
			}
			out.ActiveForProject++
			for _, attached := range sandbox.WorkspaceMounts {
				out.AttachedWorkspaceIDs[attached.WorkspaceID] = true
			}
		}
		environmentRevision := query.EnvironmentRevision
		if query.CheckpointID != "" {
			out.Checkpoint, out.CheckpointFound = state.Checkpoints[store.ScopedKey(query.ProjectID, query.CheckpointID)]
			if out.CheckpointFound {
				environmentRevision = out.Checkpoint.EnvironmentRevision
			}
		}
		if environmentRevision != "" {
			out.Environment, out.EnvironmentFound = state.Environments[store.ScopedKey(query.ProjectID, environmentRevision)]
		}
		for _, revision := range query.ConnectorRevisions {
			if connector, ok := state.Connectors[store.ScopedKey(query.ProjectID, revision)]; ok {
				out.Connectors[revision] = connector
			}
		}
		for _, workspaceID := range query.WorkspaceIDs {
			if workspace, ok := state.Workspaces[store.ScopedKey(query.ProjectID, workspaceID)]; ok {
				out.Workspaces[workspaceID] = workspace
			}
		}
		return nil
	})
	return out, err
}
