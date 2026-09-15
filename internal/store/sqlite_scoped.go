package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/infercrane/brezel/internal/domain"
)

// UpdateRows executes a lifecycle mutation without decoding or re-encoding
// unrelated ledger history. Idempotency rows bring their referenced operation
// and resource into the callback as read-only rows so existing replay branches
// retain the same behavior as Store.Update.
func (s *SQLiteStore) UpdateRows(scope MutationScope, fn func(*State) error) error {
	if fn == nil {
		return errors.New("state update callback is required")
	}
	prepared, err := prepareMutationScope(scope)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("state store is closed")
	}
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	state, readOnly, err := loadMutationStateTx(ctx, tx, prepared)
	if err != nil {
		return err
	}
	before, err := encodeState(state)
	if err != nil {
		return err
	}
	if err := fn(&state); err != nil {
		return err
	}
	if err := validateState(state); err != nil {
		return fmt.Errorf("validate updated state: %w", err)
	}
	after, err := encodeState(state)
	if err != nil {
		return err
	}
	if err := validateScopedChanges(prepared, readOnly, before, after); err != nil {
		return err
	}
	if err := syncEncodedState(ctx, tx, before, after); err != nil {
		return err
	}
	return tx.Commit()
}

type preparedMutationScope struct {
	resources   map[string]map[string]struct{}
	idempotency map[string]struct{}
	events      map[string]EventStream
}

func prepareMutationScope(scope MutationScope) (preparedMutationScope, error) {
	prepared := preparedMutationScope{
		resources: map[string]map[string]struct{}{
			resourceEnvironment: {}, resourceSandbox: {}, resourceOperation: {},
			resourceCheckpoint: {}, resourceWorkspace: {}, resourceConnector: {},
		},
		idempotency: map[string]struct{}{},
		events:      map[string]EventStream{},
	}
	for _, values := range []struct {
		kind string
		keys []string
	}{
		{resourceEnvironment, scope.Environments}, {resourceSandbox, scope.Sandboxes},
		{resourceOperation, scope.Operations}, {resourceCheckpoint, scope.Checkpoints},
		{resourceWorkspace, scope.Workspaces}, {resourceConnector, scope.Connectors},
	} {
		for _, key := range values.keys {
			if err := validateScopedResourceKey(key); err != nil {
				return preparedMutationScope{}, fmt.Errorf("invalid %s mutation scope: %w", values.kind, err)
			}
			prepared.resources[values.kind][key] = struct{}{}
		}
	}
	for _, key := range scope.Idempotency {
		if err := validateIdempotencyIndexKey(key); err != nil {
			return preparedMutationScope{}, fmt.Errorf("invalid idempotency mutation scope: %w", err)
		}
		prepared.idempotency[key] = struct{}{}
	}
	for _, stream := range scope.EventStreams {
		if err := domain.ValidateProjectID(stream.ProjectID); err != nil {
			return preparedMutationScope{}, errors.New("invalid event stream project identity")
		}
		if stream.ResourceID == "" || strings.ContainsAny(stream.ResourceID, "\x00\r\n") {
			return preparedMutationScope{}, errors.New("invalid event stream resource identity")
		}
		prepared.events[ScopedKey(stream.ProjectID, stream.ResourceID)] = stream
	}
	return prepared, nil
}

func validateScopedResourceKey(key string) error {
	parts := strings.Split(key, "\x00")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return errors.New("resource key must contain one project and resource identity")
	}
	if err := domain.ValidateProjectID(parts[0]); err != nil {
		return errors.New("resource key has invalid project identity")
	}
	if strings.ContainsAny(parts[1], "\r\n") {
		return errors.New("resource key has invalid resource identity")
	}
	return nil
}

func validateIdempotencyIndexKey(key string) error {
	parts := strings.Split(key, "\x00")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return errors.New("key must contain project, operation kind, and request key")
	}
	if err := domain.ValidateProjectID(parts[0]); err != nil {
		return errors.New("key has invalid project identity")
	}
	if strings.ContainsAny(parts[1]+parts[2], "\r\n") {
		return errors.New("key has invalid operation identity")
	}
	return nil
}

func loadMutationStateTx(ctx context.Context, tx *sql.Tx, scope preparedMutationScope) (State, encodedState, error) {
	state := NewState()
	readOnly := emptyEncodedState()
	for kind, keys := range scope.resources {
		for key := range keys {
			if err := loadResourceRowTx(ctx, tx, &state, kind, key); err != nil {
				return State{}, encodedState{}, err
			}
		}
	}
	for key := range scope.idempotency {
		var operationID, digest string
		err := tx.QueryRowContext(ctx, `SELECT operation_id, digest FROM idempotency WHERE idempotency_key = ?`, []byte(key)).Scan(&operationID, &digest)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return State{}, encodedState{}, err
		}
		state.Idempotency[key] = operationID
		if digest != "" {
			state.IdempotencyDigests[key] = digest
		}
		parts := strings.Split(key, "\x00")
		operationKey := ScopedKey(parts[0], operationID)
		if _, loaded := state.Operations[operationKey]; !loaded {
			if err := loadResourceRowTx(ctx, tx, &state, resourceOperation, operationKey); err != nil {
				return State{}, encodedState{}, err
			}
		}
		if operation, ok := state.Operations[operationKey]; ok {
			for _, kind := range []string{resourceEnvironment, resourceSandbox, resourceCheckpoint, resourceWorkspace, resourceConnector} {
				resourceKey := ScopedKey(parts[0], operation.ResourceID)
				if _, loaded := resourceFromState(state, kind, resourceKey); loaded {
					continue
				}
				if err := loadResourceRowTx(ctx, tx, &state, kind, resourceKey); err != nil {
					return State{}, encodedState{}, err
				}
			}
		}
	}
	for _, stream := range scope.events {
		var payload []byte
		err := tx.QueryRowContext(ctx, `SELECT payload FROM sandbox_events WHERE project_id = ? AND resource_id = ? ORDER BY sequence DESC LIMIT 1`, stream.ProjectID, stream.ResourceID).Scan(&payload)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return State{}, encodedState{}, err
		}
		var event domain.Event
		if err := json.Unmarshal(payload, &event); err != nil {
			return State{}, encodedState{}, fmt.Errorf("decode event: %w", err)
		}
		state.Events = append(state.Events, event)
	}
	if err := validateState(state); err != nil {
		return State{}, encodedState{}, fmt.Errorf("validate scoped state: %w", err)
	}
	loaded, err := encodeState(state)
	if err != nil {
		return State{}, encodedState{}, err
	}
	for kind, rows := range loaded.resources {
		for key, payload := range rows {
			if _, writable := scope.resources[kind][key]; !writable {
				readOnly.resources[kind][key] = slices.Clone(payload)
			}
		}
	}
	return state, readOnly, nil
}

func loadResourceRowTx(ctx context.Context, tx *sql.Tx, state *State, kind, key string) error {
	var payload []byte
	err := tx.QueryRowContext(ctx, `SELECT payload FROM resources WHERE kind = ? AND resource_key = ?`, kind, []byte(key)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := decodeResource(state, kind, key, payload); err != nil {
		return fmt.Errorf("decode %s resource: %w", kind, err)
	}
	return nil
}

func emptyEncodedState() encodedState {
	return encodedState{
		resources: map[string]map[string][]byte{
			resourceEnvironment: {}, resourceSandbox: {}, resourceOperation: {},
			resourceCheckpoint: {}, resourceWorkspace: {}, resourceConnector: {},
		},
		events:      map[string][]byte{},
		idempotency: map[string]idempotencyRow{},
	}
}

func validateScopedChanges(scope preparedMutationScope, readOnly, before, after encodedState) error {
	for kind, afterRows := range after.resources {
		beforeRows := before.resources[kind]
		for key := range beforeRows {
			if _, exists := afterRows[key]; !exists {
				if _, writable := scope.resources[kind][key]; !writable {
					return errors.New("row-scoped mutation changed an undeclared resource")
				}
			}
		}
		for key, payload := range afterRows {
			if string(beforeRows[key]) == string(payload) {
				continue
			}
			if _, writable := scope.resources[kind][key]; !writable {
				return errors.New("row-scoped mutation changed an undeclared resource")
			}
		}
	}
	// Idempotency rows point at operation IDs. Keeping an existing operation's
	// identity and idempotency key immutable means a scoped operation update
	// cannot invalidate an unmaterialized reverse reference elsewhere in the
	// ledger. Lifecycle code only advances state, result, and timestamps.
	for key, payload := range before.resources[resourceOperation] {
		nextPayload, exists := after.resources[resourceOperation][key]
		if !exists {
			return errors.New("row-scoped mutation cannot delete an existing operation")
		}
		var current, next domain.Operation
		if err := json.Unmarshal(payload, &current); err != nil {
			return err
		}
		if err := json.Unmarshal(nextPayload, &next); err != nil {
			return err
		}
		if current.ProjectID != next.ProjectID || current.ID != next.ID || current.Kind != next.Kind || current.IdempotencyKey != next.IdempotencyKey || !current.CreatedAt.Equal(next.CreatedAt) {
			return errors.New("row-scoped mutation changed immutable operation identity")
		}
	}
	for kind, rows := range readOnly.resources {
		for key, payload := range rows {
			if string(after.resources[kind][key]) != string(payload) {
				return errors.New("row-scoped mutation changed an idempotency replay dependency")
			}
		}
	}
	for key, row := range before.idempotency {
		if afterRow, exists := after.idempotency[key]; !exists || afterRow != row {
			return errors.New("row-scoped mutation changed an immutable idempotency claim")
		}
	}
	for key, row := range after.idempotency {
		if beforeRow, exists := before.idempotency[key]; exists && beforeRow == row {
			continue
		}
		if _, writable := scope.idempotency[key]; !writable {
			return errors.New("row-scoped mutation changed undeclared idempotency state")
		}
	}
	for key, payload := range before.events {
		if string(after.events[key]) != string(payload) {
			return errors.New("row-scoped mutation changed existing event history")
		}
	}
	lastSequence := map[string]int64{}
	for key := range before.events {
		projectID, resourceID, sequence, err := parseEventRowKey(key)
		if err != nil {
			return err
		}
		lastSequence[ScopedKey(projectID, resourceID)] = sequence
	}
	added := map[string][]domain.Event{}
	for key, payload := range after.events {
		if _, exists := before.events[key]; exists {
			continue
		}
		var event domain.Event
		if err := json.Unmarshal(payload, &event); err != nil {
			return err
		}
		streamKey := ScopedKey(event.ProjectID, event.ResourceID)
		if _, writable := scope.events[streamKey]; !writable {
			return errors.New("row-scoped mutation appended to an undeclared event stream")
		}
		added[streamKey] = append(added[streamKey], event)
	}
	for streamKey, events := range added {
		slices.SortFunc(events, func(a, b domain.Event) int {
			switch {
			case a.Sequence < b.Sequence:
				return -1
			case a.Sequence > b.Sequence:
				return 1
			default:
				return 0
			}
		})
		next := lastSequence[streamKey] + 1
		for _, event := range events {
			if event.Sequence != next {
				return errors.New("row-scoped mutation produced a non-contiguous event sequence")
			}
			next++
		}
	}
	return nil
}

func resourceFromState(state State, kind, key string) (any, bool) {
	switch kind {
	case resourceEnvironment:
		value, ok := state.Environments[key]
		return value, ok
	case resourceSandbox:
		value, ok := state.Sandboxes[key]
		return value, ok
	case resourceCheckpoint:
		value, ok := state.Checkpoints[key]
		return value, ok
	case resourceWorkspace:
		value, ok := state.Workspaces[key]
		return value, ok
	case resourceConnector:
		value, ok := state.Connectors[key]
		return value, ok
	default:
		return nil, false
	}
}
