package nodeledger

import (
	"bytes"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestOperationLeaseAllowsDrainButBlocksStandbyAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-ledger.json")
	ledger := openTestLedger(t, path)
	defer ledger.Close()
	ready := bindReady(t, ledger, "route-one", "sandbox-a", "engine-one")

	binding, lease, err := ledger.AcquireOperation("route-one", "project-a", "sandbox-a", ready.Public().Generation)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Public() != ready.Public() || binding.EngineID() != "engine-one" {
		t.Fatalf("acquire returned wrong binding: %v", binding)
	}
	draining, err := ledger.Transition("route-one", ready.Public().Generation, StateReady, StateDraining)
	if err != nil {
		t.Fatalf("ready to draining with an active operation: %v", err)
	}
	before := mustRead(t, path)
	if _, err := ledger.Transition("route-one", draining.Public().Generation, StateDraining, StateStandby); !errors.Is(err, ErrActiveOperations) {
		t.Fatalf("draining to standby error=%v", err)
	}
	if _, err := ledger.Transition("route-one", draining.Public().Generation, StateDraining, StateReleased); !errors.Is(err, ErrActiveOperations) {
		t.Fatalf("draining to released error=%v", err)
	}
	if after := mustRead(t, path); !bytes.Equal(before, after) {
		t.Fatal("a transition blocked by an operation lease changed durable state")
	}
	if _, _, err := ledger.AcquireOperation("route-one", "project-a", "sandbox-a", draining.Public().Generation); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("draining route admitted new operation: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition("route-one", draining.Public().Generation, StateDraining, StateStandby); err != nil {
		t.Fatalf("standby remained blocked after release: %v", err)
	}
}

func TestOperationLeaseRejectsWrongIdentityGenerationAndState(t *testing.T) {
	ledger := openTestLedger(t, filepath.Join(t.TempDir(), "node-ledger.json"))
	defer ledger.Close()
	attaching, err := ledger.Bind("route-one", "project-a", "sandbox-a", "engine-one", testTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ledger.AcquireOperation("route-one", "project-a", "sandbox-a", attaching.Public().Generation); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("attaching route acquire error=%v", err)
	}
	ready, err := ledger.Transition("route-one", attaching.Public().Generation, StateAttaching, StateReady)
	if err != nil {
		t.Fatal(err)
	}
	for name, acquire := range map[string]func() error{
		"wrong project": func() error {
			_, _, err := ledger.AcquireOperation("route-one", "project-b", "sandbox-a", ready.Public().Generation)
			return err
		},
		"wrong sandbox": func() error {
			_, _, err := ledger.AcquireOperation("route-one", "project-a", "sandbox-b", ready.Public().Generation)
			return err
		},
		"stale generation": func() error {
			_, _, err := ledger.AcquireOperation("route-one", "project-a", "sandbox-a", ready.Public().Generation+1)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := acquire()
			if name == "stale generation" && !errors.Is(err, ErrStaleGeneration) {
				t.Fatalf("error=%v", err)
			}
			if name != "stale generation" && !errors.Is(err, ErrNotFound) {
				t.Fatalf("error=%v", err)
			}
		})
	}

	// Rejected acquisitions must not create a hidden count that blocks normal
	// lifecycle progress.
	if _, err := ledger.Transition("route-one", ready.Public().Generation, StateReady, StateDraining); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition("route-one", ready.Public().Generation, StateDraining, StateStandby); err != nil {
		t.Fatalf("rejected acquire left an active lease: %v", err)
	}
}

func TestOperationLeaseCountsConcurrentOperationsAndReleasesIdempotently(t *testing.T) {
	ledger := openTestLedger(t, filepath.Join(t.TempDir(), "node-ledger.json"))
	defer ledger.Close()
	ready := bindReady(t, ledger, "route-one", "sandbox-a", "engine-one")

	_, first, err := ledger.AcquireOperation("route-one", "project-a", "sandbox-a", ready.Public().Generation)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := ledger.AcquireOperation("route-one", "project-a", "sandbox-a", ready.Public().Generation)
	if err != nil {
		t.Fatal(err)
	}
	firstCopy := *first
	draining, err := ledger.Transition("route-one", ready.Public().Generation, StateReady, StateDraining)
	if err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	results := make(chan error, 8)
	for index := range 8 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			if index%2 == 0 {
				results <- first.Release()
				return
			}
			results <- firstCopy.Release()
		}(index)
	}
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent idempotent release: %v", err)
		}
	}
	if _, err := ledger.Transition("route-one", draining.Public().Generation, StateDraining, StateStandby); !errors.Is(err, ErrActiveOperations) {
		t.Fatalf("second operation was not retained: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("second release was not idempotent: %v", err)
	}
	if _, err := ledger.Transition("route-one", draining.Public().Generation, StateDraining, StateReleased); err != nil {
		t.Fatalf("release remained blocked after every operation ended: %v", err)
	}
}

func TestOperationLeasePreventsCloseAndClearsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-ledger.json")
	ledger := openTestLedger(t, path)
	ready := bindReady(t, ledger, "route-one", "sandbox-a", "engine-one")
	_, lease, err := ledger.AcquireOperation("route-one", "project-a", "sandbox-a", ready.Public().Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition("route-one", ready.Public().Generation, StateReady, StateDraining); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); !errors.Is(err, ErrActiveOperations) {
		t.Fatalf("close with active operation error=%v", err)
	}
	if err := ledger.Ready(); err != nil {
		t.Fatalf("failed close made ledger unusable: %v", err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("failed close released the process lock")
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}

	ledger = openTestLedger(t, path)
	defer ledger.Close()
	// Operation leases are deliberately not durable. After process exit every
	// operation from that process is gone, so the persisted draining route can
	// complete its transition without a stale lease wedging recovery.
	if _, err := ledger.Transition("route-one", ready.Public().Generation, StateDraining, StateStandby); err != nil {
		t.Fatalf("reopen retained a stale in-memory operation: %v", err)
	}
}

func TestNilOperationLeaseFailsClosed(t *testing.T) {
	var lease *OperationLease
	if err := lease.Release(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil operation lease error=%v", err)
	}
}
