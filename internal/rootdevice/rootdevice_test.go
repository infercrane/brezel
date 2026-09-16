package rootdevice

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSandboxIDAndDestinationRejectTraversal(t *testing.T) {
	for _, value := range []string{"ok", "sandbox-123", "a"} {
		if err := ValidateSandboxID(value); err != nil {
			t.Fatalf("%q: %v", value, err)
		}
	}
	for _, value := range []string{"", ".", "..", "UPPER", "a/b", "a_b", "a.b", " a"} {
		if err := ValidateSandboxID(value); err == nil {
			t.Fatalf("unsafe id %q accepted", value)
		}
	}
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := Destination(root, "sandbox-1")
	if err != nil || path != filepath.Join(root, SandboxesName, "sandbox-1.img") {
		t.Fatalf("path=%q err=%v", path, err)
	}
}

func TestRootRejectsSymlinkAndOpenPermissions(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateRootPath(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateRootPath(root); err == nil {
		t.Fatal("group-accessible root accepted")
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateRootPath(link); err == nil {
		t.Fatal("symlink root accepted")
	}
}

func TestDigestAndDedicatedMarker(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "data")
	if err := os.WriteFile(path, []byte("brezel"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, size, err := DigestFile(path)
	if err != nil || digest != "8804e8e34324a2e17e10f4970544756266526b7b683d9778bd594a3ab6d3fd96" || size != 6 {
		t.Fatalf("digest=%s size=%d err=%v", digest, size, err)
	}
	if err := WriteDiskFullMarker(root); err != nil {
		t.Fatal(err)
	}
	if err := ValidateDiskFullMarker(root); err != nil {
		t.Fatal(err)
	}
}
