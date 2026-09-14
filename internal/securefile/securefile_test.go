package securefile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadAcceptsPrivateRegularFileAndRelativePath(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "secret")
	if err := os.WriteFile(path, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := Read(path, 64)
	if err != nil || string(data) != "secret\n" {
		t.Fatalf("Read() = %q, %v", data, err)
	}

	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(workingDirectory, path)
	if err != nil {
		t.Fatal(err)
	}
	relative = "." + string(filepath.Separator) + relative
	if data, err := Read(relative, 64); err != nil || string(data) != "secret\n" {
		t.Fatalf("Read(relative) = %q, %v", data, err)
	}
}

func TestReadRejectsUnsafeFileMetadata(t *testing.T) {
	directory := t.TempDir()
	private := filepath.Join(directory, "private")
	if err := os.WriteFile(private, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	public := filepath.Join(directory, "public")
	if err := os.WriteFile(public, []byte("secret"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(public, 64); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("loose permissions error = %v", err)
	}

	notReadable := filepath.Join(directory, "not-readable")
	if err := os.WriteFile(notReadable, []byte("secret"), 0o200); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(notReadable, 64); err == nil || !strings.Contains(err.Error(), "0400 or 0600") {
		t.Fatalf("missing owner-read permission error = %v", err)
	}

	executable := filepath.Join(directory, "executable")
	if err := os.WriteFile(executable, []byte("secret"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(executable, 64); err == nil || !strings.Contains(err.Error(), "0400 or 0600") {
		t.Fatalf("executable protected-file error = %v", err)
	}

	symlink := filepath.Join(directory, "symlink")
	if err := os.Symlink(private, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(symlink, 64); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v", err)
	}
	if _, err := Read(directory, 64); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory error = %v", err)
	}

	hardlink := filepath.Join(directory, "hardlink")
	if err := os.Link(private, hardlink); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(private, 64); err == nil || !strings.Contains(err.Error(), "hard link") {
		t.Fatalf("hardlink error = %v", err)
	}
}

func TestReadAcceptsOwnerReadOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("secret"), 0o400); err != nil {
		t.Fatal(err)
	}
	if data, err := Read(path, 64); err != nil || string(data) != "secret" {
		t.Fatalf("Read(0400) = %q, %v", data, err)
	}
}

func TestReadRejectsSizeEmptyAndAmbiguousPath(t *testing.T) {
	directory := t.TempDir()
	large := filepath.Join(directory, "large")
	if err := os.WriteFile(large, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(large, 4); err == nil || !strings.Contains(err.Error(), "exceeds 4") {
		t.Fatalf("size error = %v", err)
	}
	empty := filepath.Join(directory, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(empty, 4); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty error = %v", err)
	}
	for _, path := range []string{"", " " + large, large + "\n", "bad\x00path"} {
		if _, err := Read(path, 4); err == nil {
			t.Fatalf("ambiguous path %q was accepted", path)
		}
	}
	if _, err := Read(large, 0); err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("invalid limit error = %v", err)
	}
}

func TestReadDetectsPathReplacementAfterOpen(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "secret")
	replacement := filepath.Join(directory, "replacement")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := read(path, 64, func() {
		if renameErr := os.Rename(replacement, path); renameErr != nil {
			t.Fatal(renameErr)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "changed while reading") {
		t.Fatalf("replacement error = %v", err)
	}
}

func TestReadTextRequiresExactlyOneNonEmptyLine(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "token")
	write := func(value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("  one-line-token  \n")
	if value, err := ReadText(path, 64); err != nil || value != "one-line-token" {
		t.Fatalf("ReadText() = %q, %v", value, err)
	}
	for _, value := range []string{" \t\n", "line-one\nline-two", "line-one\rline-two"} {
		write(value)
		if _, err := ReadText(path, 64); err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Fatalf("ReadText(%q) error = %v", value, err)
		}
	}
}
