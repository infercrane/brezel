package service

import (
	"errors"
	"testing"
)

func TestRequireMutationRejectsCompositeKeyDelimitersAndControls(t *testing.T) {
	for _, key := range []string{
		"short",
		"unsafe/key",
		"unsafe key",
		"unsafe\x00key",
		"unsafe\nkey",
	} {
		if err := requireMutation("project-a", key); !errors.Is(err, ErrInvalid) {
			t.Fatalf("key %q error=%v", key, err)
		}
	}
	if err := requireMutation("project-a", "request-1234:attempt-1"); err != nil {
		t.Fatalf("safe key rejected: %v", err)
	}
}
