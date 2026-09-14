package store

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/infercrane/sandbox-runtime-lab/internal/domain"
)

var benchmarkStateSink State
var benchmarkBytesSink []byte
var benchmarkSandboxSink domain.Sandbox

func BenchmarkCloneState(b *testing.B) {
	for _, resources := range []int{100, 1000} {
		state := makeBenchmarkState(resources)
		b.Run(fmt.Sprintf("resources=%d/legacy-json", resources), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				cloned, err := cloneStateWithJSON(state)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkStateSink = cloned
			}
		})
		b.Run(fmt.Sprintf("resources=%d/typed", resources), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				cloned, err := cloneState(state)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkStateSink = cloned
			}
		})
	}
}

func BenchmarkEncodeState(b *testing.B) {
	state := makeBenchmarkState(1000)
	b.Run("legacy-indented", func(b *testing.B) {
		encoded, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(len(encoded)), "output-bytes")
		b.ReportAllocs()
		for range b.N {
			encoded, err = json.MarshalIndent(state, "", "  ")
			if err != nil {
				b.Fatal(err)
			}
			benchmarkBytesSink = encoded
		}
	})
	b.Run("compact", func(b *testing.B) {
		encoded, err := json.Marshal(state)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(len(encoded)), "output-bytes")
		b.ReportAllocs()
		for range b.N {
			encoded, err = json.Marshal(state)
			if err != nil {
				b.Fatal(err)
			}
			benchmarkBytesSink = encoded
		}
	})
}

func BenchmarkUpdatePreparation(b *testing.B) {
	state := makeBenchmarkState(1000)
	b.Run("legacy-json-clone-and-indented-encode", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			cloned, err := cloneStateWithJSON(state)
			if err != nil {
				b.Fatal(err)
			}
			if err := validateState(cloned); err != nil {
				b.Fatal(err)
			}
			encoded, err := json.MarshalIndent(cloned, "", "  ")
			if err != nil {
				b.Fatal(err)
			}
			benchmarkBytesSink = encoded
		}
	})
	b.Run("typed-clone-and-compact-encode", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			cloned, err := cloneState(state)
			if err != nil {
				b.Fatal(err)
			}
			if err := validateState(cloned); err != nil {
				b.Fatal(err)
			}
			encoded, err := json.Marshal(cloned)
			if err != nil {
				b.Fatal(err)
			}
			benchmarkBytesSink = encoded
		}
	})
}

func BenchmarkFileStoreView(b *testing.B) {
	state := makeBenchmarkState(1000)
	store, err := OpenFile(b.TempDir() + "/state.json")
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	store.mu.Lock()
	store.state = state
	store.mu.Unlock()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := store.View(func(cloned State) error {
			benchmarkStateSink = cloned
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFileStoreGetSandbox(b *testing.B) {
	state := makeBenchmarkState(1000)
	store, err := OpenFile(b.TempDir() + "/state.json")
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	store.mu.Lock()
	store.state = state
	store.mu.Unlock()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		sandbox, err := store.GetSandbox("project-bench", "sbx-000500")
		if err != nil {
			b.Fatal(err)
		}
		benchmarkSandboxSink = sandbox
	}
}

func BenchmarkFileStoreUpdate(b *testing.B) {
	state := makeBenchmarkState(1000)
	store, err := OpenFile(b.TempDir() + "/state.json")
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	store.mu.Lock()
	if err := store.persist(state); err != nil {
		store.mu.Unlock()
		b.Fatal(err)
	}
	store.state = state
	store.mu.Unlock()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := store.Update(func(next *State) error {
			sandbox := next.Sandboxes[ScopedKey("project-bench", "sbx-000000")]
			sandbox.Revision++
			next.Sandboxes[ScopedKey("project-bench", "sbx-000000")] = sandbox
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFileStoreSandboxEventAppend(b *testing.B) {
	for _, benchmark := range []struct {
		name   string
		append func(*FileStore, domain.Event) error
	}{
		{
			name: "generic-update",
			append: func(store *FileStore, event domain.Event) error {
				return store.Update(func(next *State) error {
					current := next.Sandboxes[ScopedKey(event.ProjectID, event.ResourceID)]
					event.State = current.State
					event.Sequence = nextSandboxEventSequence(next.Events, event.ProjectID, event.ResourceID)
					next.Events = append(next.Events, event)
					return nil
				})
			},
		},
		{name: "specialized", append: func(store *FileStore, event domain.Event) error { return store.AppendSandboxEvent(event) }},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			state := makeBenchmarkState(1000)
			store, err := OpenFile(b.TempDir() + "/state.json")
			if err != nil {
				b.Fatal(err)
			}
			defer store.Close()
			store.mu.Lock()
			if err := store.persist(state); err != nil {
				store.mu.Unlock()
				b.Fatal(err)
			}
			store.state = state
			store.mu.Unlock()
			event := domain.Event{
				ID:         "evt-command",
				ProjectID:  "project-bench",
				ResourceID: "sbx-000000",
				Type:       "command.finished",
				At:         time.Unix(1_700_000_001, 0).UTC(),
				Details:    map[string]any{"duration_ms": 12, "output_bytes": 0, "result": "exited"},
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := benchmark.append(store, event); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func cloneStateWithJSON(in State) (State, error) {
	data, err := json.Marshal(in)
	if err != nil {
		return State{}, err
	}
	out := NewState()
	if err := json.Unmarshal(data, &out); err != nil {
		return State{}, err
	}
	return out, nil
}

func makeBenchmarkState(resources int) State {
	state := NewState()
	now := time.Unix(1_700_000_000, 0).UTC()
	for index := 0; index < resources; index++ {
		id := fmt.Sprintf("%06d", index)
		environmentID := "env-" + id
		sandboxID := "sbx-" + id
		operationID := "op-" + id
		checkpointID := "chk-" + id
		workspaceID := "ws-" + id
		connectorID := "con-" + id
		state.Environments[ScopedKey("project-bench", environmentID)] = domain.Environment{
			RevisionID: environmentID, ProjectID: "project-bench", Name: "python", Backend: "e2b", BackendTemplate: "python-313", CreatedAt: now,
		}
		state.Sandboxes[ScopedKey("project-bench", sandboxID)] = domain.Sandbox{
			ID: sandboxID, ProjectID: "project-bench", EnvironmentRevision: environmentID, Backend: "e2b", BackendID: "backend-" + id,
			State: domain.SandboxRunning, Lifecycle: domain.Lifecycle{ExpiresAfterSeconds: 3600},
			Network:            domain.NetworkPolicy{AllowOut: []string{"models.example.com"}, DenyOut: []string{"metadata.example.com"}},
			ConnectorRevisions: []string{connectorID}, WorkspaceMounts: []domain.WorkspaceMount{{WorkspaceID: workspaceID, Path: "/workspace"}},
			CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour), Revision: int64(index + 1),
		}
		state.Operations[ScopedKey("project-bench", operationID)] = domain.Operation{
			ID: operationID, ProjectID: "project-bench", Kind: "sandbox.create", ResourceID: sandboxID,
			State: domain.OperationSucceeded, CreatedAt: now, UpdatedAt: now,
		}
		state.Checkpoints[ScopedKey("project-bench", checkpointID)] = domain.Checkpoint{
			ID: checkpointID, ProjectID: "project-bench", SourceSandboxID: sandboxID, EnvironmentRevision: environmentID,
			Backend: "e2b", BackendRef: "snapshot-" + id, Kind: domain.CheckpointFilesystem, CreatedAt: now,
		}
		state.Workspaces[ScopedKey("project-bench", workspaceID)] = domain.Workspace{
			ID: workspaceID, ProjectID: "project-bench", Name: "workspace", BackendID: "volume-" + id,
			State: domain.WorkspaceReady, CreatedAt: now, UpdatedAt: now,
		}
		state.Connectors[ScopedKey("project-bench", connectorID)] = domain.Connector{
			RevisionID: connectorID, ProjectID: "project-bench", Name: "model", Destination: "https://models.example.com/v1",
			AllowedMethods: []string{"POST"}, AllowedPaths: []string{"/chat/completions"}, CredentialRef: "secret://file/model-api", Status: "active", CreatedAt: now,
		}
		state.Events = append(state.Events, domain.Event{
			ID: "evt-" + id, Sequence: int64(index + 1), ProjectID: "project-bench", ResourceID: sandboxID,
			OperationID: operationID, Type: "sandbox.running", State: domain.SandboxRunning, At: now,
			Details: map[string]any{"backend": "e2b", "attempt": index, "nested": map[string]any{"ready": true}},
		})
	}
	return state
}
