package singlehost

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testBrezeldImage  = "1111111111111111111111111111111111111111111111111111111111111111"
	testBrezeldBinary = "2222222222222222222222222222222222222222222222222222222222222222"
	testNodeImage     = "3333333333333333333333333333333333333333333333333333333333333333"
	testNodeBinary    = "4444444444444444444444444444444444444444444444444444444444444444"
)

type runtimeAttestation struct {
	SchemaVersion int `json:"schema_version"`
	Source        struct {
		Revision string `json:"revision"`
		Clean    bool   `json:"clean"`
	} `json:"source"`
	Services map[string]struct {
		ImageSHA256  string `json:"image_sha256"`
		BinarySHA256 string `json:"binary_sha256"`
	} `json:"services"`
	VerifiedRunning bool `json:"verified_running"`
}

func prepareRuntimeAttestationFixture(t *testing.T) (script, repository, compose, manifest, fakeBin, revision string) {
	t.Helper()
	var err error
	script, err = filepath.Abs("runtime-attestation.sh")
	if err != nil {
		t.Fatal(err)
	}
	repository = filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	compose = filepath.Join(repository, "compose.yaml")
	if err := os.WriteFile(compose, []byte("services:\n  brezeld: {}\n  brezel-node: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.name", "Brezel Test"},
		{"config", "user.email", "brezel-test@example.invalid"},
		{"add", "compose.yaml"},
		{"commit", "-qm", "fixture"},
	} {
		command := exec.Command("git", args...)
		command.Dir = repository
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	revisionBytes, err := exec.Command("git", "-C", repository, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	revision = strings.TrimSpace(string(revisionBytes))
	manifest = filepath.Join(t.TempDir(), "runtime-attestation.manifest")
	fakeBin = t.TempDir()
	fakeDocker := filepath.Join(fakeBin, "docker")
	fakeDockerSource := `#!/bin/sh
set -eu
if [ "$1" = compose ]; then
  eval "service=\${$#}"
  case "$service" in
    brezeld) printf '%s\n' container-brezeld ;;
    brezel-node) printf '%s\n' container-brezel-node ;;
    *) exit 64 ;;
  esac
  exit
fi
if [ "$1" = inspect ]; then
  container=$4
  case "$3:$container" in
    '{{.State.Running}}':*) printf '%s\n' true ;;
    '{{.Image}}':container-brezeld) printf 'sha256:%s\n' "${FAKE_BREZELD_IMAGE:-` + testBrezeldImage + `}" ;;
    '{{.Image}}':container-brezel-node) printf 'sha256:%s\n' "${FAKE_NODE_IMAGE:-` + testNodeImage + `}" ;;
    *) exit 65 ;;
  esac
  exit
fi
if [ "$1" = exec ]; then
  case "$2:$4" in
    container-brezeld:/usr/local/bin/brezeld) printf '%s  %s\n' '` + testBrezeldBinary + `' "$4" ;;
    container-brezel-node:/usr/local/bin/brezel-node) printf '%s  %s\n' '` + testNodeBinary + `' "$4" ;;
    *) exit 66 ;;
  esac
  exit
fi
exit 67
`
	if err := os.WriteFile(fakeDocker, []byte(fakeDockerSource), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, repository, compose, manifest, fakeBin, revision
}

func runRuntimeAttestation(t *testing.T, fakeBin string, extraEnv []string, args ...string) ([]byte, error) {
	t.Helper()
	command := exec.Command(args[0], args[1:]...)
	command.Env = append(os.Environ(), append([]string{"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH")}, extraEnv...)...)
	return command.CombinedOutput()
}

func TestRuntimeAttestationBindsCleanRevisionAndRunningArtifacts(t *testing.T) {
	script, repository, compose, manifest, fakeBin, revision := prepareRuntimeAttestationFixture(t)
	if output, err := runRuntimeAttestation(t, fakeBin, nil, script, "write", manifest, repository, compose, revision); err != nil {
		t.Fatalf("write attestation: %v: %s", err, output)
	}
	info, err := os.Lstat(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("manifest mode = %o, want 600", info.Mode().Perm())
	}
	output, err := runRuntimeAttestation(t, fakeBin, nil, script, "verify", manifest, repository, compose, revision)
	if err != nil {
		t.Fatalf("verify attestation: %v: %s", err, output)
	}
	var report runtimeAttestation
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatalf("decode verification: %v: %s", err, output)
	}
	if report.SchemaVersion != 1 || !report.Source.Clean || report.Source.Revision != revision || !report.VerifiedRunning {
		t.Fatalf("unexpected report identity: %+v", report)
	}
	if report.Services["brezeld"].ImageSHA256 != testBrezeldImage || report.Services["brezeld"].BinarySHA256 != testBrezeldBinary {
		t.Fatalf("unexpected brezeld identity: %+v", report.Services["brezeld"])
	}
	if report.Services["brezel-node"].ImageSHA256 != testNodeImage || report.Services["brezel-node"].BinarySHA256 != testNodeBinary {
		t.Fatalf("unexpected node identity: %+v", report.Services["brezel-node"])
	}
}

func TestRuntimeAttestationRejectsDirtyOrChangedRuntime(t *testing.T) {
	t.Run("dirty source", func(t *testing.T) {
		script, repository, compose, manifest, fakeBin, revision := prepareRuntimeAttestationFixture(t)
		if err := os.WriteFile(filepath.Join(repository, "compose.yaml"), []byte("changed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		output, err := runRuntimeAttestation(t, fakeBin, nil, script, "write", manifest, repository, compose, revision)
		if err == nil || !strings.Contains(string(output), "source repository must be clean") {
			t.Fatalf("dirty source result: err=%v output=%s", err, output)
		}
	})

	t.Run("different clean revision", func(t *testing.T) {
		script, repository, compose, manifest, fakeBin, revision := prepareRuntimeAttestationFixture(t)
		if output, err := runRuntimeAttestation(t, fakeBin, nil, script, "write", manifest, repository, compose, revision); err != nil {
			t.Fatalf("write attestation: %v: %s", err, output)
		}
		if err := os.WriteFile(compose, []byte("services:\n  changed: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"add", "compose.yaml"}, {"commit", "-qm", "different revision"}} {
			command := exec.Command("git", args...)
			command.Dir = repository
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v: %s", args, err, output)
			}
		}
		output, err := runRuntimeAttestation(t, fakeBin, nil, script, "verify", manifest, repository, compose, revision)
		if err == nil || !strings.Contains(string(output), "running installation is not bound to the checked-out source revision") {
			t.Fatalf("changed revision result: err=%v output=%s", err, output)
		}
	})

	t.Run("changed running image", func(t *testing.T) {
		script, repository, compose, manifest, fakeBin, revision := prepareRuntimeAttestationFixture(t)
		if output, err := runRuntimeAttestation(t, fakeBin, nil, script, "write", manifest, repository, compose, revision); err != nil {
			t.Fatalf("write attestation: %v: %s", err, output)
		}
		changed := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		output, err := runRuntimeAttestation(t, fakeBin, []string{"FAKE_NODE_IMAGE=" + changed}, script, "verify", manifest, repository, compose, revision)
		if err == nil || !strings.Contains(string(output), "running brezel-node image does not match") {
			t.Fatalf("changed image result: err=%v output=%s", err, output)
		}
	})

	t.Run("exposed manifest", func(t *testing.T) {
		script, repository, compose, manifest, fakeBin, revision := prepareRuntimeAttestationFixture(t)
		if output, err := runRuntimeAttestation(t, fakeBin, nil, script, "write", manifest, repository, compose, revision); err != nil {
			t.Fatalf("write attestation: %v: %s", err, output)
		}
		if err := os.Chmod(manifest, 0o640); err != nil {
			t.Fatal(err)
		}
		output, err := runRuntimeAttestation(t, fakeBin, nil, script, "verify", manifest, repository, compose, revision)
		if err == nil || !strings.Contains(string(output), "must not be group- or world-accessible") {
			t.Fatalf("exposed manifest result: err=%v output=%s", err, output)
		}
	})

	t.Run("tampered manifest", func(t *testing.T) {
		script, repository, compose, manifest, fakeBin, revision := prepareRuntimeAttestationFixture(t)
		if output, err := runRuntimeAttestation(t, fakeBin, nil, script, "write", manifest, repository, compose, revision); err != nil {
			t.Fatalf("write attestation: %v: %s", err, output)
		}
		data, err := os.ReadFile(manifest)
		if err != nil {
			t.Fatal(err)
		}
		data = []byte(strings.Replace(string(data), testBrezeldBinary, strings.Repeat("b", 64), 1))
		if err := os.WriteFile(manifest, data, 0o600); err != nil {
			t.Fatal(err)
		}
		output, err := runRuntimeAttestation(t, fakeBin, nil, script, "verify", manifest, repository, compose, revision)
		if err == nil || !strings.Contains(string(output), "running brezeld executable does not match") {
			t.Fatalf("tampered manifest result: err=%v output=%s", err, output)
		}
	})
}
