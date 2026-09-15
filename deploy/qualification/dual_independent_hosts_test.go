package qualification

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func harnessPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join("dual-independent-hosts.sh")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat harness: %v", err)
	}
	return path
}

func TestDualHostHarnessParsesWithBash(t *testing.T) {
	command := exec.Command("bash", "-n", harnessPath(t))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, output)
	}
}

func TestDualHostHarnessRequiresExplicitDestructiveOptIn(t *testing.T) {
	command := exec.Command("bash", harnessPath(t))
	command.Env = append(os.Environ(), "BREZEL_DUAL_EXECUTE=")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("harness ran without destructive opt-in")
	}
	if !strings.Contains(string(output), "BREZEL_DUAL_EXECUTE=true") {
		t.Fatalf("missing fail-closed guidance: %s", output)
	}
}

func TestDualHostHarnessRejectsSymlinkedKnownHosts(t *testing.T) {
	directory := t.TempDir()
	knownHosts := filepath.Join(directory, "known-hosts")
	knownHostsLink := filepath.Join(directory, "known-hosts-link")
	identity := filepath.Join(directory, "identity")
	if err := os.WriteFile(knownHosts, []byte("host ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(knownHosts, knownHostsLink); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity, []byte("not-used"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", harnessPath(t))
	command.Env = append(os.Environ(),
		"BREZEL_DUAL_EXECUTE=true",
		"BREZEL_DUAL_HOST_A=user@host-a",
		"BREZEL_DUAL_HOST_B=user@host-b",
		"BREZEL_DUAL_SSH_KNOWN_HOSTS="+knownHostsLink,
		"BREZEL_DUAL_SSH_IDENTITY_FILE="+identity,
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("harness accepted symlinked known-hosts file")
	}
	if !strings.Contains(string(output), "must be a regular file and not a symlink") {
		t.Fatalf("unexpected error: %s", output)
	}
}

func TestDualHostHarnessRejectsGroupReadableIdentity(t *testing.T) {
	directory := t.TempDir()
	knownHosts := filepath.Join(directory, "known-hosts")
	identity := filepath.Join(directory, "identity")
	if err := os.WriteFile(knownHosts, []byte("host ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity, []byte("not-used"), 0o640); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", harnessPath(t))
	command.Env = append(os.Environ(),
		"BREZEL_DUAL_EXECUTE=true",
		"BREZEL_DUAL_HOST_A=user@host-a",
		"BREZEL_DUAL_HOST_B=user@host-b",
		"BREZEL_DUAL_SSH_KNOWN_HOSTS="+knownHosts,
		"BREZEL_DUAL_SSH_IDENTITY_FILE="+identity,
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("harness accepted group-readable SSH identity")
	}
	if !strings.Contains(string(output), "must not be group- or world-accessible") {
		t.Fatalf("unexpected error: %s", output)
	}
}

func TestDualHostHarnessEncodesHonestClaimBoundary(t *testing.T) {
	data, err := os.ReadFile(harnessPath(t))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	for _, excluded := range []string{
		`"cluster"`, `"shared control plane"`, `"multi-node scheduler"`,
		`"automatic placement"`, `"automatic failover"`, `"cross-host restore"`,
		`"replicated storage"`, `"high availability"`,
	} {
		if !strings.Contains(source, excluded) {
			t.Errorf("harness does not encode excluded claim %s", excluded)
		}
	}
	for _, required := range []string{
		"StrictHostKeyChecking=yes", "UserKnownHostsFile=", "IdentitiesOnly=yes",
		"simultaneous_conformance", "namespace_isolation", "failure_containment",
		"machine_id_sha256", "boot_id_sha256", "stop_active_jobs",
		"-preflight-empty-project",
		"runtime-attestation.sh", "runtime-attestation.manifest",
		"runtime_attestation.verified_running == true",
		"trap cleanup_partial_fixture EXIT", "remote_project_cleanliness",
		"COORDINATOR_REVISION", "sample_remote_clock", "python3", "exec ssh",
		"COORDINATOR_SHA256", "shasum -a 256",
		"--config -", "checksum_evidence", "write_status passed",
		"Checksummed evidence",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("harness missing invariant %q", required)
		}
	}
	if strings.Contains(source, "StrictHostKeyChecking=no") {
		t.Fatal("harness disables SSH host verification")
	}
	for _, forbidden := range []string{
		"trap cleanup_partial_fixture RETURN", ".dual-curl-config", "seal_evidence",
		"Partial sealed evidence", "Sealed evidence",
	} {
		if strings.Contains(source, forbidden) {
			t.Errorf("harness retains unsafe or inaccurate construct %q", forbidden)
		}
	}
}

func TestDualHostHarnessPublishesStatusAfterChecksums(t *testing.T) {
	data, err := os.ReadFile(harnessPath(t))
	if err != nil {
		t.Fatal(err)
	}
	source := string(data)
	checksum := strings.LastIndex(source, "checksum_evidence")
	passed := strings.LastIndex(source, "write_status passed")
	finalized := strings.LastIndex(source, "FINALIZED=true")
	if checksum < 0 || passed < 0 || finalized < 0 {
		t.Fatal("harness is missing final evidence publication steps")
	}
	if !(checksum < passed && passed < finalized) {
		t.Fatalf("final publication order is not checksum, passed status, finalized: checksum=%d status=%d finalized=%d", checksum, passed, finalized)
	}
}
