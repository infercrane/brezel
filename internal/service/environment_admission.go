package service

import (
	"fmt"

	"github.com/infercrane/brezel/internal/store"
)

func (s *Service) admitEnvironmentCreate(projectID, idempotencyKey, inputDigest, revisionID string) (store.EnvironmentAdmissionSnapshot, error) {
	const kind = "create_environment"
	if reader, ok := s.store.(store.EnvironmentAdmissionReader); ok {
		snapshot, err := reader.ReadEnvironmentAdmission(store.EnvironmentAdmissionQuery{
			ProjectID: projectID, IdempotencyKind: kind, IdempotencyKey: idempotencyKey, RevisionID: revisionID,
		})
		if err != nil {
			return store.EnvironmentAdmissionSnapshot{}, err
		}
		if snapshot.Idempotency.Found && snapshot.Idempotency.Digest != "" && snapshot.Idempotency.Digest != inputDigest {
			return store.EnvironmentAdmissionSnapshot{}, fmt.Errorf("%w: Idempotency-Key was already used with a different request", ErrConflict)
		}
		return snapshot, nil
	}

	snapshot := store.EnvironmentAdmissionSnapshot{}
	var lookupErr error
	err := s.store.View(func(state store.State) error {
		operation, ok := idempotentOperation(state, projectID, kind, idempotencyKey)
		if ok {
			lookupErr = requireIdempotencyDigest(state, projectID, kind, idempotencyKey, inputDigest)
			if lookupErr != nil {
				return nil
			}
			environment, found := state.Environments[store.ScopedKey(projectID, operation.ResourceID)]
			if !found {
				return store.ErrNotFound
			}
			snapshot.Idempotency = store.IdempotencyLookup{Operation: operation, Digest: state.IdempotencyDigests[store.IdempotencyKey(projectID, kind, idempotencyKey)], Found: true}
			snapshot.Environment, snapshot.EnvironmentFound = environment, true
			return nil
		}
		if environment, found := state.Environments[store.ScopedKey(projectID, revisionID)]; found {
			snapshot.Environment, snapshot.EnvironmentFound = environment, true
			return nil
		}
		for _, environment := range state.Environments {
			if environment.ProjectID == projectID {
				snapshot.CountForProject++
			}
		}
		return nil
	})
	if err != nil {
		return store.EnvironmentAdmissionSnapshot{}, translateStore(err)
	}
	if lookupErr != nil {
		return store.EnvironmentAdmissionSnapshot{}, lookupErr
	}
	return snapshot, nil
}
