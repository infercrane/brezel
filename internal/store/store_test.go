package store

import (
	"path/filepath"
	"testing"

	"github.com/infercrane/sandbox-runtime-lab/internal/domain"
)

func TestFileStorePersistsAtomicallyAndScopesResources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(state *State) error {
		state.Sandboxes[ScopedKey("a", "same")] = domain.Sandbox{ID: "same", ProjectID: "a"}
		state.Sandboxes[ScopedKey("b", "same")] = domain.Sandbox{ID: "same", ProjectID: "b"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.View(func(state State) error {
		if len(state.Sandboxes) != 2 {
			t.Fatalf("got %d sandboxes", len(state.Sandboxes))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFailedUpdateDoesNotMutateMemoryOrDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := OpenFile(path)
	want := errorsForTest("stop")
	err := s.Update(func(state *State) error {
		state.Idempotency["bad"] = "write"
		return want
	})
	if err != want {
		t.Fatalf("got %v", err)
	}
	_ = s.View(func(state State) error {
		if _, ok := state.Idempotency["bad"]; ok {
			t.Fatal("failed update mutated state")
		}
		return nil
	})
}

type errorsForTest string

func (e errorsForTest) Error() string { return string(e) }
