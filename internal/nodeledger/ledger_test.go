package nodeledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var testTime = time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

func TestLedgerPersistsGenerationAndRedactsEngineIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-ledger.json")
	ledger := openTestLedger(t, path)
	binding, err := ledger.Bind("route-one", "project-a", "sandbox-a", "engine-secret-one", testTime)
	if err != nil {
		t.Fatal(err)
	}
	if got := binding.Public(); got.Generation != 1 || got.State != StateAttaching || got.LastActivity != testTime {
		t.Fatalf("unexpected public binding: %#v", got)
	}
	for name, representation := range map[string]string{
		"json":   string(mustJSON(t, binding)),
		"string": fmt.Sprintf("%v", binding),
		"go":     fmt.Sprintf("%#v", binding),
	} {
		if strings.Contains(representation, "engine-secret-one") || strings.Contains(representation, "engine_id") {
			t.Fatalf("%s representation exposed engine identity: %s", name, representation)
		}
	}
	if binding.EngineID() != "engine-secret-one" {
		t.Fatal("trusted binding lost engine identity")
	}

	binding, err = ledger.Transition("route-one", 1, StateAttaching, StateReady)
	if err != nil {
		t.Fatal(err)
	}
	binding, err = ledger.Touch("route-one", 1, StateReady, testTime.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if binding.Public().LastActivity != testTime.Add(time.Minute) {
		t.Fatal("activity was not updated")
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	ledger = openTestLedger(t, path)
	defer ledger.Close()
	binding, err = ledger.Resolve("route-one")
	if err != nil {
		t.Fatal(err)
	}
	if binding.Public().Generation != 1 || binding.Public().State != StateReady || binding.EngineID() != "engine-secret-one" {
		t.Fatalf("binding did not survive reopen: %v", binding)
	}
	if binding.Public().LastActivity != testTime.Add(time.Minute) {
		t.Fatal("activity did not survive reopen")
	}
}

func TestLedgerGenerationPreventsABAAfterRebindAndRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-ledger.json")
	ledger := openTestLedger(t, path)
	defer ledger.Close()

	first := bindReady(t, ledger, "route-one", "sandbox-a", "engine-one")
	draining, err := ledger.Transition("route-one", first.Public().Generation, StateReady, StateDraining)
	if err != nil {
		t.Fatal(err)
	}
	standby, err := ledger.Transition("route-one", draining.Public().Generation, StateDraining, StateStandby)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ledger.Rebind("route-one", standby.Public().Generation, StateStandby, "engine-two", testTime.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.Public().Generation <= first.Public().Generation {
		t.Fatal("rebind did not increase generation")
	}
	if _, err := ledger.Transition("route-one", first.Public().Generation, StateAttaching, StateReady); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale transition error=%v", err)
	}
	resolved, err := ledger.Resolve("route-one")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.EngineID() != "engine-two" || resolved.Public().State != StateAttaching {
		t.Fatalf("stale transition changed target: %v", resolved)
	}
	if _, err := ledger.ResolveCurrent("route-one", "project-a", "sandbox-a", first.Public().Generation, StateReady); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale data-path resolution error=%v", err)
	}
	if _, err := ledger.ResolveCurrent("route-one", "project-b", "sandbox-a", second.Public().Generation, StateAttaching); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project data-path resolution error=%v", err)
	}
	if current, err := ledger.ResolveCurrent("route-one", "project-a", "sandbox-a", second.Public().Generation, StateAttaching); err != nil || current.EngineID() != "engine-two" {
		t.Fatalf("current data-path resolution binding=%v error=%v", current, err)
	}

	ready, err := ledger.Transition("route-one", second.Public().Generation, StateAttaching, StateReady)
	if err != nil {
		t.Fatal(err)
	}
	draining, err = ledger.Transition("route-one", ready.Public().Generation, StateReady, StateDraining)
	if err != nil {
		t.Fatal(err)
	}
	released, err := ledger.Transition("route-one", draining.Public().Generation, StateDraining, StateReleased)
	if err != nil {
		t.Fatal(err)
	}
	if released.EngineID() != "" {
		t.Fatal("released binding retained engine identity")
	}
	if err := ledger.Remove("route-one", released.Public().Generation, StateReleased); err != nil {
		t.Fatal(err)
	}
	third, err := ledger.Bind("route-one", "project-a", "sandbox-b", "engine-three", testTime.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if third.Public().Generation <= second.Public().Generation {
		t.Fatalf("route reuse reset generation: first=%d second=%d third=%d", first.Public().Generation, second.Public().Generation, third.Public().Generation)
	}
}

func TestLedgerGenerationClockPersistsAfterEveryRouteIsRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-ledger.json")
	ledger := openTestLedger(t, path)
	binding := bindReady(t, ledger, "route-one", "sandbox-a", "engine-one")
	draining, err := ledger.Transition("route-one", binding.Public().Generation, StateReady, StateDraining)
	if err != nil {
		t.Fatal(err)
	}
	released, err := ledger.Transition("route-one", draining.Public().Generation, StateDraining, StateReleased)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Remove("route-one", released.Public().Generation, StateReleased); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	ledger = openTestLedger(t, path)
	defer ledger.Close()
	newBinding, err := ledger.Bind("route-one", "project-a", "sandbox-a", "engine-two", testTime.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if newBinding.Public().Generation <= released.Public().Generation {
		t.Fatalf("durable generation clock reset: previous=%d new=%d", released.Public().Generation, newBinding.Public().Generation)
	}
}

func TestLedgerRejectsStaleStateAndIllegalTransitionsWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-ledger.json")
	ledger := openTestLedger(t, path)
	defer ledger.Close()
	binding, err := ledger.Bind("route-one", "project-a", "sandbox-a", "engine-one", testTime)
	if err != nil {
		t.Fatal(err)
	}
	before := mustRead(t, path)
	if _, err := ledger.Transition("route-one", binding.Public().Generation, StateReady, StateStandby); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("wrong-state transition error=%v", err)
	}
	if _, err := ledger.Transition("route-one", binding.Public().Generation, StateAttaching, StateDraining); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("illegal transition error=%v", err)
	}
	if _, err := ledger.Touch("route-one", binding.Public().Generation, StateReady, testTime.Add(time.Second)); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("wrong-state touch error=%v", err)
	}
	if _, err := ledger.Rebind("route-one", binding.Public().Generation, StateReady, "engine-two", testTime.Add(time.Second)); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("illegal rebind error=%v", err)
	}
	if after := mustRead(t, path); !bytes.Equal(before, after) {
		t.Fatal("rejected mutations changed durable state")
	}
	resolved, err := ledger.Resolve("route-one")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Public().State != StateAttaching || resolved.EngineID() != "engine-one" {
		t.Fatalf("rejected mutation changed memory: %v", resolved)
	}
}

func TestLedgerRejectsBackwardActivity(t *testing.T) {
	ledger := openTestLedger(t, filepath.Join(t.TempDir(), "node-ledger.json"))
	defer ledger.Close()
	binding := bindReady(t, ledger, "route-one", "sandbox-a", "engine-one")
	if _, err := ledger.Touch("route-one", binding.Public().Generation, StateReady, testTime.Add(-time.Second)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("backward activity error=%v", err)
	}
}

func TestLedgerRejectsDuplicateLiveAssignments(t *testing.T) {
	ledger := openTestLedger(t, filepath.Join(t.TempDir(), "node-ledger.json"))
	defer ledger.Close()
	if _, err := ledger.Bind("route-one", "project-a", "sandbox-a", "engine-one", testTime); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Bind("route-two", "project-a", "sandbox-a", "engine-two", testTime); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate sandbox assignment error=%v", err)
	}
	if _, err := ledger.Bind("route-two", "project-a", "sandbox-b", "engine-one", testTime); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate engine assignment error=%v", err)
	}
}

func TestLedgerConditionalTransitionHasOneWinner(t *testing.T) {
	ledger := openTestLedger(t, filepath.Join(t.TempDir(), "node-ledger.json"))
	defer ledger.Close()
	binding := bindReady(t, ledger, "route-one", "sandbox-a", "engine-one")

	var wait sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := ledger.Transition("route-one", binding.Public().Generation, StateReady, StateDraining)
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	var succeeded, conflicted int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrStateConflict):
			conflicted++
		default:
			t.Fatalf("unexpected transition result: %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("succeeded=%d conflicted=%d", succeeded, conflicted)
	}
}

func TestLedgerRejectsConcurrentOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-ledger.json")
	first := openTestLedger(t, path)
	defer first.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("second process authority opened the ledger")
	}
}

func TestLedgerRejectsUnsafeFilesAndDirectory(t *testing.T) {
	t.Run("directory permissions", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(filepath.Join(dir, "ledger.json")); err == nil {
			t.Fatal("non-private ledger directory was accepted")
		}
	})
	t.Run("state permissions", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ledger.json")
		ledger := openTestLedger(t, path)
		if err := ledger.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path); err == nil {
			t.Fatal("non-private ledger file was accepted")
		}
	})
	t.Run("lock permissions", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ledger.json")
		ledger := openTestLedger(t, path)
		if err := ledger.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path+".lock", 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path); err == nil {
			t.Fatal("non-private ledger lock was accepted")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte("not-a-ledger"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, "ledger.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(filepath.Join(dir, "ledger.json")); err == nil {
			t.Fatal("symlink ledger was accepted")
		}
	})
	t.Run("hard link", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ledger.json")
		ledger := openTestLedger(t, path)
		if err := ledger.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(path, path+".alias"); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path); err == nil {
			t.Fatal("hard-linked ledger was accepted")
		}
	})
}

func TestLedgerFailsClosedOnChecksumAndInvariantCorruption(t *testing.T) {
	t.Run("checksum", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ledger.json")
		ledger := openTestLedger(t, path)
		if _, err := ledger.Bind("route-one", "project-a", "sandbox-a", "engine-one", testTime); err != nil {
			t.Fatal(err)
		}
		if err := ledger.Close(); err != nil {
			t.Fatal(err)
		}
		data := mustRead(t, path)
		data[bytes.Index(data, []byte("engine-one"))] = 'E'
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("checksum corruption error=%v", err)
		}
	})
	t.Run("invariant with valid checksum", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ledger.json")
		ledger := openTestLedger(t, path)
		if _, err := ledger.Bind("route-one", "project-a", "sandbox-a", "engine-one", testTime); err != nil {
			t.Fatal(err)
		}
		if err := ledger.Close(); err != nil {
			t.Fatal(err)
		}
		envelope := readEnvelope(t, path)
		entry := envelope.State.Routes["route-one"]
		delete(envelope.State.Routes, "route-one")
		envelope.State.Routes["wrong-key"] = entry
		envelope.Checksum = mustChecksum(t, envelope.State)
		writeEnvelope(t, path, envelope)
		if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("invariant corruption error=%v", err)
		}
	})
	t.Run("schema", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ledger.json")
		ledger := openTestLedger(t, path)
		if err := ledger.Close(); err != nil {
			t.Fatal(err)
		}
		envelope := readEnvelope(t, path)
		envelope.State.SchemaVersion++
		envelope.Checksum = mustChecksum(t, envelope.State)
		writeEnvelope(t, path, envelope)
		if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("schema corruption error=%v", err)
		}
	})
	t.Run("unknown field", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ledger.json")
		ledger := openTestLedger(t, path)
		if err := ledger.Close(); err != nil {
			t.Fatal(err)
		}
		data := mustRead(t, path)
		data = bytes.Replace(data, []byte(`"checksum"`), []byte(`"unknown":true,"checksum"`), 1)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("unknown field error=%v", err)
		}
	})
}

func TestLedgerBounds(t *testing.T) {
	t.Run("file bytes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ledger.json")
		if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(path, maxLedgerBytes+1); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("oversized ledger error=%v", err)
		}
	})
	t.Run("route count", func(t *testing.T) {
		state := newDiskState()
		state.LastGeneration = maxRoutes + 1
		for index := 1; index <= maxRoutes+1; index++ {
			routeID := fmt.Sprintf("route-%d", index)
			state.Routes[routeID] = diskEntry{RouteID: routeID, ProjectID: "project-a", SandboxID: fmt.Sprintf("sandbox-%d", index), EngineID: fmt.Sprintf("engine-%d", index), Generation: uint64(index), State: StateReady, LastActivity: testTime}
		}
		if err := validateDiskState(state); err == nil {
			t.Fatal("oversized route map was accepted")
		}
	})
	t.Run("identity", func(t *testing.T) {
		ledger := openTestLedger(t, filepath.Join(t.TempDir(), "ledger.json"))
		defer ledger.Close()
		if _, err := ledger.Bind(strings.Repeat("r", 129), "project-a", "sandbox-a", "engine-one", testTime); !errors.Is(err, ErrInvalid) {
			t.Fatalf("oversized route error=%v", err)
		}
		if _, err := ledger.Bind("route-one", "project-a", "sandbox-a", strings.Repeat("e", maxEngineIDBytes+1), testTime); !errors.Is(err, ErrInvalid) {
			t.Fatalf("oversized engine identity error=%v", err)
		}
	})
	t.Run("generation exhaustion", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ledger.json")
		if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		state := newDiskState()
		state.LastGeneration = ^uint64(0)
		writeEnvelope(t, path, diskEnvelope{State: state, Checksum: mustChecksum(t, state)})
		ledger := openTestLedger(t, path)
		defer ledger.Close()
		if _, err := ledger.Bind("route-one", "project-a", "sandbox-a", "engine-one", testTime); err == nil {
			t.Fatal("exhausted generation clock was accepted")
		}
		if routes, err := ledger.List(); err != nil || len(routes) != 0 {
			t.Fatalf("failed generation allocation mutated routes=%v error=%v", routes, err)
		}
	})
}

func TestLedgerIgnoresUncommittedTempAndWillNotRecoverCorruptMain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	ledger := openTestLedger(t, path)
	if _, err := ledger.Bind("route-one", "project-a", "sandbox-a", "engine-one", testTime); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	committed := mustRead(t, path)
	tempPath := filepath.Join(filepath.Dir(path), ".brezel-node-ledger-interrupted")
	if err := os.WriteFile(tempPath, []byte(`{"state":{},"checksum":"newer-but-uncommitted"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger = openTestLedger(t, path)
	if _, err := ledger.Resolve("route-one"); err != nil {
		t.Fatalf("valid committed state was not authoritative: %v", err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tempPath, committed, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt committed state recovered from an uncommitted temp file: %v", err)
	}
}

func TestLedgerReadyAndClosedBehavior(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	ledger := openTestLedger(t, path)
	if err := ledger.Ready(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Ready(); err == nil {
		t.Fatal("replaced lock path remained ready")
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Ready(); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed readiness error=%v", err)
	}
	if _, err := ledger.Resolve("route-one"); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed resolve error=%v", err)
	}
}

func openTestLedger(t *testing.T, path string) *Ledger {
	t.Helper()
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	ledger, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func bindReady(t *testing.T, ledger *Ledger, routeID, sandboxID, engineID string) Binding {
	t.Helper()
	binding, err := ledger.Bind(routeID, "project-a", sandboxID, engineID, testTime)
	if err != nil {
		t.Fatal(err)
	}
	binding, err = ledger.Transition(routeID, binding.Public().Generation, StateAttaching, StateReady)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readEnvelope(t *testing.T, path string) diskEnvelope {
	t.Helper()
	var envelope diskEnvelope
	if err := json.Unmarshal(mustRead(t, path), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func writeEnvelope(t *testing.T, path string, envelope diskEnvelope) {
	t.Helper()
	if err := os.WriteFile(path, mustJSON(t, envelope), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustChecksum(t *testing.T, state diskState) string {
	t.Helper()
	checksum, err := checksumState(state)
	if err != nil {
		t.Fatal(err)
	}
	return checksum
}
