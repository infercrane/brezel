//go:build linux

package rootdevice

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPrivilegedReflinkIntegration(t *testing.T) {
	root := os.Getenv("BREZEL_ROOTDEVICE_INTEGRATION_ROOT")
	if root == "" {
		t.Skip("BREZEL_ROOTDEVICE_INTEGRATION_ROOT is not set")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("integration root must be empty")
	}
	basePath := filepath.Join(root, BaseName)
	data := make([]byte, 8<<20)
	for index := range data {
		data[index] = byte(index % 251)
	}
	if err := os.WriteFile(basePath, data, 0o400); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(basePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	digest, _, err := DigestFile(basePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = SetBaseImmutable(root, false)
		_ = os.RemoveAll(filepath.Join(root, SandboxesName))
		_ = os.Remove(basePath)
	}()
	if _, err := ValidateBase(root, digest); err == nil {
		t.Fatal("mutable base accepted")
	}
	if err := SetBaseImmutable(root, true); err != nil {
		t.Fatal(err)
	}
	if filesystem, err := Inspect(root); err != nil || !filesystem.ReflinkCapable {
		t.Fatalf("filesystem=%#v err=%v", filesystem, err)
	}
	realParent := filepath.Join(root, "real-parent")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(realParent, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "linked-parent")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(filepath.Join(linkedParent, "nested")); err == nil {
		t.Fatal("root with an intermediate symlink was accepted")
	}
	if err := os.Remove(linkedParent); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(realParent); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateBase(root, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateBase(root, "0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Fatal("base accepted with the wrong digest")
	}
	store, _, err := OpenStore(root, digest)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var wait sync.WaitGroup
	errorsChannel := make(chan error, 16)
	for index := 0; index < 16; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err := store.Clone(fmt.Sprintf("test-%02d", index))
			errorsChannel <- err
		}(index)
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
	clonePath, err := Destination(root, "test-00")
	if err != nil {
		t.Fatal(err)
	}
	clone, err := os.OpenFile(clonePath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clone.WriteAt([]byte("changed"), 0); err != nil {
		t.Fatal(err)
	}
	if err := clone.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = clone.Close()
	if _, err := ValidateBase(root, digest); err != nil {
		t.Fatal(err)
	}
	siblingPath, err := Destination(root, "test-01")
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := os.Open(siblingPath)
	if err != nil {
		t.Fatal(err)
	}
	prefix := make([]byte, len("changed"))
	if _, err := io.ReadFull(sibling, prefix); err != nil {
		sibling.Close()
		t.Fatal(err)
	}
	if err := sibling.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prefix, data[:len(prefix)]) {
		t.Fatal("clone write changed a sibling clone")
	}
	for index := 0; index < 16; index++ {
		if err := Delete(root, fmt.Sprintf("test-%02d", index)); err != nil {
			t.Fatal(err)
		}
	}
	if err := Delete(root, "test-00"); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, SandboxesName, ".clone-crash-residue")
	if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := Recover(root); err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	sentinel := filepath.Join(root, "recover-sentinel")
	if err := os.WriteFile(sentinel, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	unsafeStale := filepath.Join(root, SandboxesName, ".clone-unsafe-symlink")
	if err := os.Symlink(sentinel, unsafeStale); err != nil {
		t.Fatal(err)
	}
	if _, err := Recover(root); err == nil {
		t.Fatal("recovery followed or removed a stale symlink")
	}
	contents, err := os.ReadFile(sentinel)
	if err != nil || string(contents) != "sentinel" {
		t.Fatalf("sentinel changed: contents=%q err=%v", contents, err)
	}
	if err := os.Remove(unsafeStale); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sentinel); err != nil {
		t.Fatal(err)
	}
}
