package store

import (
	"strings"
	"testing"
	"time"

	"github.com/infercrane/brezel/internal/domain"
)

func TestSQLiteStoreUpdateRowsKeepsExistingIdempotencyClaimImmutable(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sandbox := sqliteTestSandbox()
	op := domain.Operation{
		ID: "op-existing", ProjectID: sandbox.ProjectID, Kind: "create_sandbox", ResourceID: sandbox.ID,
		State: domain.OperationSucceeded, IdempotencyKey: "request-existing-0001", CreatedAt: sandbox.CreatedAt, UpdatedAt: sandbox.UpdatedAt,
	}
	replacement := op
	replacement.ID = "op-replacement"
	indexKey := IdempotencyKey(op.ProjectID, op.Kind, op.IdempotencyKey)
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		state.Operations[ScopedKey(op.ProjectID, op.ID)] = op
		state.Operations[ScopedKey(replacement.ProjectID, replacement.ID)] = replacement
		state.Idempotency[indexKey] = op.ID
		state.IdempotencyDigests[indexKey] = strings.Repeat("a", 64)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	scope := MutationScope{
		Operations:  []string{ScopedKey(op.ProjectID, op.ID), ScopedKey(replacement.ProjectID, replacement.ID)},
		Idempotency: []string{indexKey},
	}
	if err := s.UpdateRows(scope, func(state *State) error {
		state.Idempotency[indexKey] = replacement.ID
		return nil
	}); err == nil || !strings.Contains(err.Error(), "immutable idempotency claim") {
		t.Fatalf("operation substitution error = %v", err)
	}
	if err := s.UpdateRows(scope, func(state *State) error {
		state.IdempotencyDigests[indexKey] = strings.Repeat("b", 64)
		return nil
	}); err == nil || !strings.Contains(err.Error(), "immutable idempotency claim") {
		t.Fatalf("digest substitution error = %v", err)
	}
	if err := s.UpdateRows(scope, func(state *State) error {
		delete(state.Idempotency, indexKey)
		delete(state.IdempotencyDigests, indexKey)
		return nil
	}); err == nil || !strings.Contains(err.Error(), "immutable idempotency claim") {
		t.Fatalf("idempotency deletion error = %v", err)
	}
	lookup, err := s.LookupIdempotency(op.ProjectID, op.Kind, op.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if lookup.Operation.ID != op.ID || lookup.Digest != strings.Repeat("a", 64) {
		t.Fatalf("idempotency claim changed after rejected mutation: %#v", lookup)
	}
}

func TestSQLiteStoreUpdateRowsKeepsOperationNamespaceAndCreationTimeImmutable(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*domain.Operation)
	}{
		{name: "kind", mutate: func(operation *domain.Operation) { operation.Kind = "delete_sandbox" }},
		{name: "created-at", mutate: func(operation *domain.Operation) { operation.CreatedAt = operation.CreatedAt.Add(time.Second) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			sandbox := sqliteTestSandbox()
			op := domain.Operation{
				ID: "op-existing", ProjectID: sandbox.ProjectID, Kind: "create_sandbox", ResourceID: sandbox.ID,
				State: domain.OperationRunning, IdempotencyKey: "request-identity-0001", CreatedAt: sandbox.CreatedAt, UpdatedAt: sandbox.UpdatedAt,
			}
			key := ScopedKey(op.ProjectID, op.ID)
			if err := s.Update(func(state *State) error { state.Operations[key] = op; return nil }); err != nil {
				t.Fatal(err)
			}
			if err := s.UpdateRows(MutationScope{Operations: []string{key}}, func(state *State) error {
				changed := state.Operations[key]
				test.mutate(&changed)
				state.Operations[key] = changed
				return nil
			}); err == nil || !strings.Contains(err.Error(), "immutable operation identity") {
				t.Fatalf("immutable operation mutation error = %v", err)
			}
		})
	}
}

func TestSQLiteStoreUpdateRowsLoadsReplayResourceWhenOperationIsWritable(t *testing.T) {
	s, err := OpenSQLite(privateTestPath(t, "state.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sandbox := sqliteTestSandbox()
	op := domain.Operation{
		ID: "op-existing", ProjectID: sandbox.ProjectID, Kind: "create_sandbox", ResourceID: sandbox.ID,
		State: domain.OperationRunning, IdempotencyKey: "request-dependency-0001", CreatedAt: sandbox.CreatedAt, UpdatedAt: sandbox.UpdatedAt,
	}
	indexKey := IdempotencyKey(op.ProjectID, op.Kind, op.IdempotencyKey)
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)] = sandbox
		state.Operations[ScopedKey(op.ProjectID, op.ID)] = op
		state.Idempotency[indexKey] = op.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	scope := MutationScope{
		Operations: []string{ScopedKey(op.ProjectID, op.ID)}, Idempotency: []string{indexKey},
	}
	if err := s.UpdateRows(scope, func(state *State) error {
		if state.Sandboxes[ScopedKey(sandbox.ProjectID, sandbox.ID)].ID != sandbox.ID {
			t.Fatal("idempotency replay resource dependency was not loaded")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRows(scope, func(state *State) error {
		key := ScopedKey(sandbox.ProjectID, sandbox.ID)
		changed := state.Sandboxes[key]
		changed.Revision++
		state.Sandboxes[key] = changed
		return nil
	}); err == nil || !strings.Contains(err.Error(), "undeclared resource") {
		t.Fatalf("replay dependency mutation error = %v", err)
	}
}

func TestValidateStateBindsIdempotencyNamespaceToOperationKind(t *testing.T) {
	state := NewState()
	sandbox := sqliteTestSandbox()
	op := domain.Operation{
		ID: "op-kind", ProjectID: sandbox.ProjectID, Kind: "create_workspace", ResourceID: sandbox.ID,
		State: domain.OperationSucceeded, IdempotencyKey: "request-kind-0001", CreatedAt: sandbox.CreatedAt, UpdatedAt: sandbox.UpdatedAt,
	}
	state.Operations[ScopedKey(op.ProjectID, op.ID)] = op
	state.Idempotency[IdempotencyKey(op.ProjectID, "create_sandbox", op.IdempotencyKey)] = op.ID
	if err := validateState(state); err == nil || !strings.Contains(err.Error(), "inconsistent operation") {
		t.Fatalf("cross-kind idempotency validation error = %v", err)
	}
}
