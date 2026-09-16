// Package rootdevice contains an independent qualification candidate for
// reflink-backed sandbox roots. It is not selected by the production runtime.
package rootdevice

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var ErrUnsupported = errors.New("reflink root-device qualification is unsupported on this platform")

const (
	BaseName       = "base.img"
	SandboxesName  = "sandboxes"
	DiskFullMarker = ".brezel-rootdevice-dedicated"
	markerContents = "BREZEL DEDICATED ROOTDEVICE QUALIFICATION FILESYSTEM\n"
)

type Filesystem struct {
	Type            string `json:"type"`
	Magic           uint64 `json:"magic"`
	ReflinkCapable  bool   `json:"reflink_capable"`
	ProbeDurationNS int64  `json:"probe_duration_ns"`
}

type Base struct {
	Path          string `json:"path"`
	Size          int64  `json:"size"`
	SHA256        string `json:"sha256"`
	ImmutableFlag bool   `json:"immutable_flag"`
}

type CloneResult struct {
	Path       string `json:"path"`
	DurationNS int64  `json:"duration_ns"`
}

func ValidateSandboxID(value string) error {
	if value == "" || len(value) > 64 || value == "." || value == ".." {
		return errors.New("sandbox id must contain 1 to 64 safe characters")
	}
	for _, current := range value {
		if (current >= 'a' && current <= 'z') || (current >= '0' && current <= '9') || current == '-' {
			continue
		}
		return errors.New("sandbox id must contain only lowercase letters, digits, and hyphens")
	}
	return nil
}

func ValidateRootPath(root string) (string, error) {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || strings.ContainsRune(root, '\x00') {
		return "", errors.New("qualification root must be a clean absolute path")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("qualification root must be a directory, not a symlink")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("qualification root must not be accessible by group or other")
	}
	return root, nil
}

func DigestFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func validateDigest(value string) error {
	if len(value) != sha256.Size*2 {
		return errors.New("base digest must be a lowercase SHA-256 digest")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || hex.EncodeToString(decoded) != value {
		return errors.New("base digest must be a lowercase SHA-256 digest")
	}
	return nil
}

func Destination(root, sandboxID string) (string, error) {
	if _, err := ValidateRootPath(root); err != nil {
		return "", err
	}
	if err := ValidateSandboxID(sandboxID); err != nil {
		return "", err
	}
	return filepath.Join(root, SandboxesName, sandboxID+".img"), nil
}

func WriteDiskFullMarker(root string) error {
	if _, err := ValidateRootPath(root); err != nil {
		return err
	}
	path := filepath.Join(root, DiskFullMarker)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write([]byte(markerContents)); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return syncParent(path)
}

func ValidateDiskFullMarker(root string) error {
	data, err := os.ReadFile(filepath.Join(root, DiskFullMarker))
	if err != nil {
		return err
	}
	if string(data) != markerContents {
		return fmt.Errorf("%s has invalid contents", DiskFullMarker)
	}
	return nil
}

func syncParent(path string) error {
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
