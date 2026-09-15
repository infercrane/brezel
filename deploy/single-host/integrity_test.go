package singlehost

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/brezel/internal/backend/e2b"
)

func TestPinnedEngineAndPatchIntegrity(t *testing.T) {
	data, err := os.ReadFile("engine.lock")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	if values["commit"] != e2b.AuditedRevision {
		t.Fatalf("engine.lock commit %q differs from audited adapter revision %q", values["commit"], e2b.AuditedRevision)
	}
	patches := map[string]string{
		"api_patch_sha256":                    "0001-harden-volume-secrets-and-cleanup.patch",
		"api_build_patch_sha256":              "0002-pin-api-build-images.patch",
		"orchestrator_lifecycle_patch_sha256": "0003-acknowledge-delete-after-sandbox-teardown.patch",
		"orchestrator_cache_patch_sha256":     "0004-bound-snapshot-diff-cache.patch",
	}
	for lockKey, name := range patches {
		patchPath := filepath.Join("..", "..", "third_party", "e2b-runtime", "patches", name)
		patch, err := os.ReadFile(patchPath)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(patch)
		if got := hex.EncodeToString(digest[:]); got != values[lockKey] {
			t.Fatalf("%s digest = %s, lock = %s", name, got, values[lockKey])
		}
	}
	for lockKey, name := range map[string]string{
		"image_lock_sha256":    "engine.images.lock",
		"artifact_lock_sha256": "engine.artifacts.lock",
	} {
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		if got := hex.EncodeToString(digest[:]); got != values[lockKey] {
			t.Fatalf("%s digest = %s, lock = %s", name, got, values[lockKey])
		}
	}
}

func TestDeploymentScriptsParse(t *testing.T) {
	for _, script := range []string{"install.sh", "qualify.sh", "host-reboot-drill.sh", "engine-capabilities.sh", "capacity-contract.sh", "engine-capacity-contract.sh", "engine-auth-cache-contract.sh", "artifact-supply-chain.sh", "runtime-attestation.sh", "benchmark.sh", "host-tuning.sh"} {
		command := exec.Command("sh", "-n", script)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("sh -n %s: %v: %s", script, err, output)
		}
	}
}

func TestEngineCapacityUpdateVerifiesEffectiveLimitBeforeCommit(t *testing.T) {
	data, err := os.ReadFile("engine-capacity-contract.sh")
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	begin := strings.Index(content, "BEGIN;")
	commit := strings.Index(content, "COMMIT;")
	if begin < 0 || commit <= begin {
		t.Fatal("capacity reconciler does not contain one explicit transaction")
	}
	transaction := content[begin:commit]
	update := strings.Index(transaction, "UPDATE public.tiers")
	effective := strings.LastIndex(transaction, "FROM public.team_limits")
	if update < 0 || effective <= update || !strings.Contains(transaction[effective:], "RAISE EXCEPTION") {
		t.Fatal("effective team limit is not asserted after the tier update and before commit")
	}
}

func TestEngineAuthCacheInvalidationIsPrefixBounded(t *testing.T) {
	fakeBin := t.TempDir()
	logPath := filepath.Join(fakeBin, "redis.log")
	scanPath := filepath.Join(fakeBin, "scan-count")
	fakeRedis := filepath.Join(fakeBin, "redis-cli")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$REDIS_LOG"
case " $* " in
  *" --scan "*)
    if [ ! -e "$REDIS_SCAN_COUNT" ]; then
      printf '%s\n' 'auth:team:team-one' 'auth:team:key-hash'
      : > "$REDIS_SCAN_COUNT"
    fi
    ;;
esac
`
	if err := os.WriteFile(fakeRedis, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", "engine-auth-cache-contract.sh", "invalidate")
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REDIS_LOG="+logPath,
		"REDIS_SCAN_COUNT="+scanPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("cache invalidation failed: %v: %s", err, output)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"UNLINK auth:team:team-one", "UNLINK auth:team:key-hash"} {
		if !strings.Contains(string(log), key) {
			t.Fatalf("cache invalidation log omitted %q: %s", key, log)
		}
	}
	if strings.Contains(string(log), "FLUSH") {
		t.Fatalf("cache invalidation used an unbounded Redis operation: %s", log)
	}
}

func TestUpgradeStopsPublicAdmissionAndForcesDerivedStateGates(t *testing.T) {
	data, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	boundary := strings.Index(content, `PUBLIC_DRAIN_STARTED=true`)
	if boundary < 0 {
		t.Fatal("installer does not declare the start of its fail-closed upgrade boundary")
	}
	upgrade := content[boundary:]
	ordered := []string{
		`stop brezeld`,
		`stop brezel-node`,
		`stop ready`,
		`stop client-proxy`,
		`stop api`,
		`stop orchestrator`,
		`run --rm --no-deps host-setup`,
		`run --rm --no-deps fetch-artifacts`,
		`run --rm --no-deps brezel-orchestrator-install`,
		`rm -sf brezel-engine-auth-cache brezel-engine-capacity`,
		`up -d --wait`,
		`run --rm --no-deps brezel-engine-capacity`,
		`"$SCRIPT_DIR/qualify.sh"`,
		`INSTALL_SUCCEEDED=true`,
	}
	last := -1
	for _, marker := range ordered {
		index := strings.Index(upgrade, marker)
		if index <= last {
			t.Fatalf("installer marker %q is absent or out of order", marker)
		}
		last = index
	}
	if !strings.Contains(content, `"$INSTALL_SUCCEEDED" != true`) {
		t.Fatal("installer has no fail-closed public service cleanup")
	}
}

func TestQualificationCapacityPreflightPrecedesAnyCreate(t *testing.T) {
	data, err := os.ReadFile("qualify.sh")
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	start := strings.LastIndex(content, `preflight_capacity_contract`)
	conformance := strings.LastIndex(content, `run_conformance "$TARGET"`)
	if start < 0 || conformance <= start {
		t.Fatal("qualification does not run its capacity preflight before conformance")
	}
	if !strings.Contains(content, `MIN_READY_NETWORK_SLOTS" -lt "$MAX_ACTIVE_SANDBOXES_TOTAL`) {
		t.Fatal("qualification can weaken network readiness below active capacity")
	}
}

func TestBenchmarkEmptyProjectPreflightPrecedesEnvironmentCreation(t *testing.T) {
	data, err := os.ReadFile("benchmark.sh")
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	preflight := strings.Index(content, `-preflight-empty-project`)
	firstMutation := strings.Index(content, `-execute > "$raw_tmp"`)
	if preflight < 0 || firstMutation < 0 || preflight >= firstMutation {
		t.Fatal("benchmark does not complete its empty-project preflight before running the mutating matrix")
	}
	failureEvidence := strings.Index(content[preflight:firstMutation], `find . -type f ! -name SHA256SUMS`)
	if failureEvidence < 0 {
		t.Fatal("benchmark does not retain checksummed evidence when the project preflight rejects a run")
	}
}

func TestBenchmarkPublishesTerminalStatusAfterVerifiedChecksums(t *testing.T) {
	data, err := os.ReadFile("benchmark.sh")
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	finalChecksums := strings.LastIndex(content, `sha256sum -c SHA256SUMS`)
	failedStatus := strings.LastIndex(content, `write_status failed`)
	passedStatus := strings.LastIndex(content, `write_status passed`)
	if finalChecksums < 0 || failedStatus < 0 || passedStatus < 0 {
		t.Fatal("benchmark is missing verified checksum or terminal status publication")
	}
	if finalChecksums >= failedStatus || finalChecksums >= passedStatus {
		t.Fatal("benchmark publishes a terminal status before its evidence checksums are verified")
	}
	if strings.Contains(content, `find . -type f ! -name SHA256SUMS -print0`) {
		t.Fatal("benchmark checksum manifest still includes mutable STATUS")
	}
}

func TestEngineCapacityReconcilerIsRequiredBeforeAPIStartup(t *testing.T) {
	override, err := os.ReadFile("engine.override.yaml")
	if err != nil {
		t.Fatal(err)
	}
	content := string(override)
	for _, required := range []string{
		"brezel-engine-capacity:",
		"brezel-engine-auth-cache:",
		"BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL:",
		"BREZEL_ENGINE_CAPACITY_SCRIPT",
		"BREZEL_ENGINE_AUTH_CACHE_SCRIPT",
		"condition: service_completed_successfully",
	} {
		if !strings.Contains(content, required) {
			t.Fatalf("engine override is missing capacity invariant %q", required)
		}
	}
}

func TestCapacityContractMatchesSandboxQuotaAndHugepagePool(t *testing.T) {
	meminfo := filepath.Join(t.TempDir(), "meminfo")
	content := strings.Join([]string{
		"MemTotal:       33554432 kB",
		"HugePages_Total:    9216",
		"HugePages_Free:     9216",
		"HugePages_Rsvd:        0",
		"Hugepagesize:       2048 kB",
		"",
	}, "\n")
	if err := os.WriteFile(meminfo, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func(mode string, env ...string) ([]byte, error) {
		command := exec.Command("sh", "capacity-contract.sh", mode)
		command.Env = append(os.Environ(), append([]string{"BREZEL_TEST_MEMINFO_FILE=" + meminfo}, env...)...)
		return command.CombinedOutput()
	}

	if output, err := run("plan"); err != nil {
		t.Fatalf("default capacity plan failed: %v: %s", err, output)
	} else if !strings.Contains(string(output), `"required_hugepages_2m":9216`) {
		t.Fatalf("capacity plan did not bind quota to lifecycle headroom: %s", output)
	}
	if output, err := run("live"); err != nil {
		t.Fatalf("live capacity contract failed: %v: %s", err, output)
	}
	if output, err := run("plan", "BREZEL_ENGINE_HUGEPAGES=2048"); err == nil {
		t.Fatalf("capacity contract accepted an eight-guest hugepage pool for a 32-guest quota: %s", output)
	} else if !strings.Contains(string(output), "require at least 9216") {
		t.Fatalf("capacity failure did not explain the required pool: %s", output)
	}
	if output, err := run("plan", "BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL=33"); err == nil {
		t.Fatalf("capacity contract accepted more active guests than qualified network slots: %s", output)
	} else if !strings.Contains(string(output), "32-slot") {
		t.Fatalf("network capacity failure was unclear: %s", output)
	}
}

func TestEngineImageLockUsesExactAMD64Manifests(t *testing.T) {
	data, err := os.ReadFile("engine.images.lock")
	if err != nil {
		t.Fatal(err)
	}
	values := parseLock(t, data)
	if values["architecture"] != "linux/amd64" {
		t.Fatalf("image lock architecture = %q", values["architecture"])
	}
	for _, key := range []string{
		"BREZEL_ENGINE_POSTGRES_IMAGE", "BREZEL_ENGINE_REDIS_IMAGE",
		"BREZEL_ENGINE_CLICKHOUSE_IMAGE", "BREZEL_ENGINE_VECTOR_IMAGE",
		"E2B_DB_MIGRATOR_IMAGE", "E2B_CLIENT_PROXY_IMAGE",
		"E2B_CLICKHOUSE_MIGRATOR_IMAGE", "E2B_TOOLS_IMAGE",
		"E2B_NODE_E2B_IMAGE", "E2B_SEED_IMAGE",
	} {
		value := values[key]
		marker := "@sha256:"
		at := strings.LastIndex(value, marker)
		if at < 1 || len(value[at+len(marker):]) != 64 {
			t.Fatalf("%s is not digest-pinned: %q", key, value)
		}
		if _, err := hex.DecodeString(value[at+len(marker):]); err != nil {
			t.Fatalf("%s has an invalid digest: %v", key, err)
		}
	}

	command := exec.Command("sh", "artifact-supply-chain.sh", "image-lock", "engine.images.lock")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("image lock gate rejected the release lock: %v: %s", err, output)
	}

	bad := strings.Replace(string(data), "@sha256:63bd", ":mutable", 1)
	badPath := filepath.Join(t.TempDir(), "engine.images.lock")
	if err := os.WriteFile(badPath, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	command = exec.Command("sh", "artifact-supply-chain.sh", "image-lock", badPath)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("image lock gate accepted a mutable image reference: %s", output)
	} else if !strings.Contains(string(output), "must name one exact image manifest") {
		t.Fatalf("image lock failure did not explain the immutable-manifest requirement: %s", output)
	}
}

func TestEngineImageModesPullOrRequirePreloadedExactDigests(t *testing.T) {
	fakeBin := t.TempDir()
	logPath := filepath.Join(fakeBin, "docker.log")
	fakeDocker := filepath.Join(fakeBin, "docker")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$DOCKER_LOG\"\nexit 0\n"
	if err := os.WriteFile(fakeDocker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	imageLock, err := filepath.Abs("engine.images.lock")
	if err != nil {
		t.Fatal(err)
	}
	run := func(mode string) string {
		t.Helper()
		if err := os.WriteFile(logPath, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		command := exec.Command("sh", "artifact-supply-chain.sh", "images", imageLock, mode)
		command.Env = append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"), "DOCKER_LOG="+logPath)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("%s image mode failed: %v: %s", mode, err, output)
		}
		log, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		return string(log)
	}

	pullLog := run("pull")
	if got := strings.Count(pullLog, "pull --platform linux/amd64"); got != 10 {
		t.Fatalf("pull mode made %d exact pulls, want 10; log: %s", got, pullLog)
	}
	if got := strings.Count(pullLog, "image inspect"); got != 10 {
		t.Fatalf("pull mode made %d local inspections, want 10; log: %s", got, pullLog)
	}

	preloadedLog := run("preloaded")
	if strings.Contains(preloadedLog, "pull ") {
		t.Fatalf("preloaded mode contacted a registry: %s", preloadedLog)
	}
	if got := strings.Count(preloadedLog, "image inspect"); got != 10 {
		t.Fatalf("preloaded mode made %d local inspections, want 10; log: %s", got, preloadedLog)
	}
}

func TestArtifactSupplyChainRejectsChangedHostArtifact(t *testing.T) {
	root := t.TempDir()
	artifact := filepath.Join(root, "fc", "firecracker")
	if err := os.MkdirAll(filepath.Dir(artifact), 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte("known firecracker bytes")
	if err := os.WriteFile(artifact, content, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	lock := "architecture=linux/amd64\n"
	for _, name := range []string{"orchestrator", "envd", "firecracker", "kernel", "busybox"} {
		lock += name + "_path=/fc/firecracker\n"
		lock += name + "_sha256=" + hex.EncodeToString(digest[:]) + "\n"
	}
	lockPath := filepath.Join(root, "artifacts.lock")
	if err := os.WriteFile(lockPath, []byte(lock), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", "artifact-supply-chain.sh", "host", lockPath, root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("host artifact gate rejected exact bytes: %v: %s", err, output)
	}
	if err := os.WriteFile(artifact, []byte("changed firecracker bytes"), 0o700); err != nil {
		t.Fatal(err)
	}
	command = exec.Command("sh", "artifact-supply-chain.sh", "host", lockPath, root)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("host artifact gate accepted changed bytes: %s", output)
	} else if !strings.Contains(string(output), "digest mismatch") {
		t.Fatalf("host artifact failure did not identify the digest mismatch: %s", output)
	}
}

func TestArtifactSupplyChainVerifiesInstalledOrchestratorOverride(t *testing.T) {
	root := t.TempDir()
	artifact := filepath.Join(root, "fc", "artifact")
	if err := os.MkdirAll(filepath.Dir(artifact), 0o700); err != nil {
		t.Fatal(err)
	}
	upstream := []byte("upstream bytes")
	if err := os.WriteFile(artifact, upstream, 0o700); err != nil {
		t.Fatal(err)
	}
	upstreamDigest := sha256.Sum256(upstream)
	lock := "architecture=linux/amd64\n"
	for _, name := range []string{"orchestrator", "envd", "firecracker", "kernel", "busybox"} {
		path := "/fc/artifact"
		if name == "orchestrator" {
			path = "/fc/orchestrator"
		}
		lock += name + "_path=" + path + "\n"
		lock += name + "_sha256=" + hex.EncodeToString(upstreamDigest[:]) + "\n"
	}
	lockPath := filepath.Join(root, "artifacts.lock")
	if err := os.WriteFile(lockPath, []byte(lock), 0o600); err != nil {
		t.Fatal(err)
	}

	custom := []byte("source-built orchestrator")
	orchestrator := filepath.Join(root, "fc", "orchestrator")
	if err := os.WriteFile(orchestrator, custom, 0o700); err != nil {
		t.Fatal(err)
	}
	customDigest := sha256.Sum256(custom)
	command := exec.Command("sh", "artifact-supply-chain.sh", "host", lockPath, root, hex.EncodeToString(customDigest[:]))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("host artifact gate rejected the expected custom orchestrator: %v: %s", err, output)
	}

	badDigest := strings.Repeat("0", 64)
	command = exec.Command("sh", "artifact-supply-chain.sh", "host", lockPath, root, badDigest)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("host artifact gate accepted the wrong custom orchestrator digest: %s", output)
	}
}

func TestArtifactSupplyChainRejectsChangedSourceFile(t *testing.T) {
	root := t.TempDir()
	files := map[string][]byte{
		"embed/compose/compose.yaml":               []byte("services: {}\n"),
		"embed/compose/.env":                       []byte("PINNED=true\n"),
		"embed/compose/scripts/fetch-artifacts.sh": []byte("#!/bin/sh\nexit 0\n"),
	}
	for relative, content := range files {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	imageLock, err := filepath.Abs("engine.images.lock")
	if err != nil {
		t.Fatal(err)
	}
	artifactLock, err := filepath.Abs("engine.artifacts.lock")
	if err != nil {
		t.Fatal(err)
	}
	digestOf := func(content []byte) string {
		t.Helper()
		digest := sha256.Sum256(content)
		return hex.EncodeToString(digest[:])
	}
	imageData, err := os.ReadFile(imageLock)
	if err != nil {
		t.Fatal(err)
	}
	artifactData, err := os.ReadFile(artifactLock)
	if err != nil {
		t.Fatal(err)
	}
	lock := "commit=fixture\n" +
		"compose_sha256=" + digestOf(files["embed/compose/compose.yaml"]) + "\n" +
		"compose_env_sha256=" + digestOf(files["embed/compose/.env"]) + "\n" +
		"fetch_artifacts_sha256=" + digestOf(files["embed/compose/scripts/fetch-artifacts.sh"]) + "\n" +
		"image_lock_sha256=" + digestOf(imageData) + "\n" +
		"artifact_lock_sha256=" + digestOf(artifactData) + "\n"
	lockPath := filepath.Join(root, "engine.lock")
	if err := os.WriteFile(lockPath, []byte(lock), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", "artifact-supply-chain.sh", "source", root, lockPath, imageLock, artifactLock)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("source gate rejected exact bytes: %v: %s", err, output)
	}
	composePath := filepath.Join(root, "embed", "compose", "compose.yaml")
	if err := os.WriteFile(composePath, []byte("services:\n  changed: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command = exec.Command("sh", "artifact-supply-chain.sh", "source", root, lockPath, imageLock, artifactLock)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("source gate accepted changed compose bytes: %s", output)
	} else if !strings.Contains(string(output), "digest mismatch") {
		t.Fatalf("source gate failure did not identify the digest mismatch: %s", output)
	}
}

func TestInstallerEnforcesOwnedArtifactBoundary(t *testing.T) {
	data, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer := string(data)
	for _, required := range []string{
		"BREZEL_ENGINE_SOURCE_REPOSITORY",
		"BREZEL_ENGINE_ARTIFACT_BASE_URL",
		`image-lock "$ENGINE_IMAGE_LOCK"`,
		`images "$ENGINE_IMAGE_LOCK" "${BREZEL_ENGINE_IMAGE_MODE:-pull}"`,
		`source "$ENGINE_DIR" "$LOCK_FILE"`,
		`host "$ENGINE_ARTIFACT_LOCK" /`,
		`manifest "$INSTALL_DIR/distribution.manifest"`,
		`RUNTIME_ATTESTATION`,
		`write "$INSTALL_DIR/runtime-attestation.manifest"`,
		`verify "$INSTALL_DIR/runtime-attestation.manifest"`,
		"ENGINE_ORCHESTRATOR_PATCH",
		"BREZEL_ENGINE_ORCHESTRATOR_IMAGE",
		"BREZEL_ENGINE_ORCHESTRATOR_SHA256",
		"docker create --entrypoint /orchestrator",
		"brezel-orchestrator-install",
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("installer is missing artifact boundary %q", required)
		}
	}
	cleanSource := strings.Index(installer, `status --porcelain --untracked-files=normal`)
	drain := strings.Index(installer, `PUBLIC_DRAIN_STARTED=true`)
	runtimeWrite := strings.Index(installer, `write "$INSTALL_DIR/runtime-attestation.manifest"`)
	qualification := strings.LastIndex(installer, `"$SCRIPT_DIR/qualify.sh"`)
	if cleanSource < 0 || drain <= cleanSource {
		t.Fatal("installer does not reject a dirty product checkout before its service drain")
	}
	if runtimeWrite < 0 || qualification <= runtimeWrite {
		t.Fatal("installer does not seal the running runtime identity before qualification")
	}

	overrideData, err := os.ReadFile("engine.override.yaml")
	if err != nil {
		t.Fatal(err)
	}
	override := string(overrideData)
	if got := strings.Count(override, "\n  host-setup:\n"); got != 1 {
		t.Fatalf("engine override defines host-setup %d times, want exactly once", got)
	}
	for _, required := range []string{
		"BREZEL_ENGINE_POSTGRES_IMAGE", "BREZEL_ENGINE_REDIS_IMAGE",
		"BREZEL_ENGINE_CLICKHOUSE_IMAGE", "BREZEL_ENGINE_VECTOR_IMAGE",
		"pull_policy: never", "BREZEL_ENGINE_ARTIFACT_BASE_URL",
		"BREZEL_VM_OVERCOMMIT_MEMORY", "BREZEL_HOST_TUNING_SCRIPT",
		"BREZEL_ENGINE_HUGEPAGES", "HUGEPAGES:",
		"BUILD_CACHE_TTL", "BUILD_CACHE_MAX_BYTES", "BUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT",
		"brezel-orchestrator-install", "BREZEL_ENGINE_ORCHESTRATOR_BINARY",
		"BREZEL_ENGINE_ORCHESTRATOR_SHA256",
		"fetch-artifacts:\n        condition: service_completed_successfully",
	} {
		if !strings.Contains(override, required) {
			t.Fatalf("engine override is missing locked distribution setting %q", required)
		}
	}
}

func parseLock(t *testing.T, data []byte) map[string]string {
	t.Helper()
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" || value == "" {
			t.Fatalf("invalid lock line %q", line)
		}
		values[key] = value
	}
	return values
}

func TestPinnedEngineFastPathSourceContract(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"packages/orchestrator/pkg/sandbox/fc/client.go":                      "models.MemoryBackendBackendTypeUffd\nOperations.LoadSnapshot\nResumeVM:            false\n",
		"packages/orchestrator/pkg/sandbox/uffd/uffd.go":                      "userfaultfd.NewUserfaultfdFromFd\n",
		"packages/orchestrator/pkg/template/build/builder.go":                 "builders = append(builders, optimizeBuilder)\n",
		"packages/orchestrator/pkg/template/build/phases/optimize/builder.go": "WithPrefetch(&metadata.Prefetch\ncontinuing without prefetch\n",
		"packages/orchestrator/pkg/sandbox/sandbox.go":                        "prefetch.New(sbxLogger, memfile, fcUffd, initMapping\nrootfs.NewNBDProvider\n",
		"packages/shared/pkg/featureflags/flags.go":                           "NewStringFlag(\"resume-prefetch-source\", \"init\")\n",
		"packages/shared/pkg/storage/sandbox.go":                              "fmt.Sprintf(\"rootfs-%s-%s.cow\"\nenvDefault:\"${ORCHESTRATOR_BASE_PATH}/sandbox\"\nenvDefault:\"${ORCHESTRATOR_BASE_PATH}/template\"\n",
		"packages/orchestrator/pkg/sandbox/network/pool.go":                   "NewSlotsPoolSize    = 32\nReusedSlotsPoolSize = 100\n",
		"packages/orchestrator/pkg/factories/run.go":                          "network.NewPool(network.NewSlotsPoolSize, network.ReusedSlotsPoolSize\n",
		"packages/orchestrator/pkg/server/sandboxes.go":                       "if err := sbx.Stop(ctx); err != nil\nSandboxes.WaitLifecycle(ctx\n",
		"packages/orchestrator/pkg/sandbox/map.go":                            "func (m *Map) WaitLifecycle(ctx context.Context\n",
		"packages/orchestrator/pkg/cfg/model.go":                              "env:\"BUILD_CACHE_TTL\"\nenv:\"BUILD_CACHE_MAX_BYTES\"\nenv:\"BUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT\"\nBUILD_CACHE_TTL must be at least 1h\nBUILD_CACHE_MAX_BYTES must be zero or at least 1 GiB\nBUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT must be between 1 and 100\n",
		"packages/orchestrator/pkg/sandbox/build/cache.go":                    "func allocatedBytes(path string)\ncachePressureObservationFailed\ncachePressureAllocatedBytes\norchestrator.build.cache.pressure_evictions\n",
		"packages/orchestrator/pkg/sandbox/template/cache.go":                 "config.BuildCacheTTL\n",
		"embed/compose/compose.yaml":                                          "TEMPLATE_STORAGE_URL: file:///var/lib/e2b/storage/templates\nNBD_POOL_SIZE: \"64\"\nNETWORK_VERSION: \"1\"\n",
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	command := exec.Command("sh", "engine-capabilities.sh", "source", root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("source contract rejected complete fixture: %v: %s", err, output)
	} else if !strings.Contains(string(output), `"source_contract":"conformant"`) {
		t.Fatalf("source contract did not emit a conformant record: %s", output)
	}

	broken := filepath.Join(root, "packages", "orchestrator", "pkg", "sandbox", "fc", "client.go")
	if err := os.WriteFile(broken, []byte("models.MemoryBackendBackendTypeUffd\nResumeVM:            false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command = exec.Command("sh", "engine-capabilities.sh", "source", root)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("source contract accepted a tree without snapshot restore: %s", output)
	} else if !strings.Contains(string(output), "Firecracker snapshot restore") {
		t.Fatalf("source contract failure did not name the missing capability: %s", output)
	}
}

func TestInstallerAndQualificationFailClosedOnEngineFastPaths(t *testing.T) {
	installerData, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerData)
	for _, required := range []string{
		"ENGINE_CAPABILITY_PROBE",
		`source "$ENGINE_BUILD_DIR"`,
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("installer is missing engine source gate %q", required)
		}
	}

	qualificationData, err := os.ReadFile("qualify.sh")
	if err != nil {
		t.Fatal(err)
	}
	qualification := string(qualificationData)
	for _, required := range []string{
		"run_engine_fast_path_qualification",
		"resolve_engine_sandbox_id",
		`/bin/sh -s -- live "$engine_sandbox_id" "$min_network_slots"`,
		"engine_fast_path_conformant",
	} {
		if !strings.Contains(qualification, required) {
			t.Fatalf("qualification is missing live engine gate %q", required)
		}
	}

	probeData, err := os.ReadFile("engine-capabilities.sh")
	if err != nil {
		t.Fatal(err)
	}
	probe := string(probeData)
	for _, required := range []string{
		"Operations.LoadSnapshot",
		"NewUserfaultfdFromFd",
		"WithPrefetch",
		"rootfs.NewNBDProvider",
		"NewSlotsPoolSize",
		"anon_inode:[userfaultfd]",
		"NBD_POOL_SIZE=64",
		"NETWORK_VERSION=1",
		"TEMPLATE_STORAGE_URL=file:///var/lib/e2b/storage/templates",
		"/orchestrator/sandbox/rootfs-",
		"/orchestrator/template/",
		"/var/run/netns/ns-",
		"no usable memory prefetch mapping was produced",
		"BUILD_CACHE_TTL",
		"BUILD_CACHE_MAX_BYTES",
		"BUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT",
		"orchestrator.build.cache.pressure_evictions",
		"observation_failure",
	} {
		if !strings.Contains(probe, required) {
			t.Fatalf("engine capability probe is missing %q", required)
		}
	}
}

func TestInstallerPinsAndValidatesSnapshotDiffCachePolicy(t *testing.T) {
	installerData, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerData)
	for _, required := range []string{
		"0004-bound-snapshot-diff-cache.patch",
		"orchestrator_cache_patch_sha256",
		"BREZEL_BUILD_CACHE_TTL:-4h",
		"BREZEL_BUILD_CACHE_MAX_BYTES:-34359738368",
		"BREZEL_BUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT:-70",
		"BREZEL_BUILD_CACHE_TTL must be between 1h and 168h",
		"BREZEL_BUILD_CACHE_MAX_BYTES must be at least 1073741824",
		"BREZEL_BUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT must be between 50 and 90",
		`patch -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_CACHE_PATCH"`,
		`"$BREZEL_ENGINE_ORCHESTRATOR_SHA256" "$ENGINE_ORCHESTRATOR_PATCH_SHA256" "$ENGINE_CACHE_PATCH_SHA256"`,
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("installer is missing snapshot-diff cache invariant %q", required)
		}
	}

	supplyChainData, err := os.ReadFile("artifact-supply-chain.sh")
	if err != nil {
		t.Fatal(err)
	}
	supplyChain := string(supplyChainData)
	for _, required := range []string{
		"artifact.orchestrator.cache_patch_sha256",
		"artifact.orchestrator.snapshot_diff_cache=bounded-recoverable-cache",
		"the cache patch requires the lifecycle patch identity",
	} {
		if !strings.Contains(supplyChain, required) {
			t.Fatalf("distribution manifest writer is missing cache-patch identity %q", required)
		}
	}
}

func TestBenchmarkCapturesSnapshotDiffCacheBeforeAndAfter(t *testing.T) {
	data, err := os.ReadFile("benchmark.sh")
	if err != nil {
		t.Fatal(err)
	}
	benchmark := string(data)
	for _, required := range []string{
		"capture_engine_cache",
		"engine-cache-before.json",
		"engine-cache-after.json",
		`/bin/sh -s -- cache`,
		"Benchmark cache-policy preflight failed before any environment or VM was created",
	} {
		if !strings.Contains(benchmark, required) {
			t.Fatalf("benchmark is missing snapshot-diff cache evidence %q", required)
		}
	}
}

func TestBenchmarkRequiresEmptyProjectPostflight(t *testing.T) {
	data, err := os.ReadFile("benchmark.sh")
	if err != nil {
		t.Fatal(err)
	}
	benchmark := string(data)
	for _, required := range []string{
		"project-postflight.json",
		"project-postflight.invalid",
		"project_postflight_outcome=failed",
		`$project_postflight_outcome == "passed"`,
		`postflight:{project_inventory:"project-postflight.json",outcome:$project_postflight_outcome}`,
	} {
		if !strings.Contains(benchmark, required) {
			t.Fatalf("benchmark is missing empty-project postflight invariant %q", required)
		}
	}
}

func TestHostRebootDrillRequiresHonestStateAndWorkspaceRecovery(t *testing.T) {
	data, err := os.ReadFile("host-reboot-drill.sh")
	if err != nil {
		t.Fatal(err)
	}
	drill := string(data)
	for _, required := range []string{
		"observed_state", "workspace_replacement", "host_reboot_workspace_recovery_conformant",
		"durable workspace marker changed", "confirmed_cleanup", "--ttl 3600", "failed|deleted",
	} {
		if !strings.Contains(drill, required) {
			t.Fatalf("host reboot drill is missing %q", required)
		}
	}
}

func TestInstallerDetectsUFWGuestNetworkBoundary(t *testing.T) {
	data, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer := string(data)
	for _, required := range []string{
		"check_ufw_guest_network",
		"10.11.0.0/24",
		"5010:5018",
		"ufw route allow out",
		"BREZEL_SKIP_UFW_PREFLIGHT=true",
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("installer is missing UFW boundary instruction %q", required)
		}
	}
}

func TestWorkspaceMountUsesHostNamespacePath(t *testing.T) {
	data, err := os.ReadFile("engine.override.yaml")
	if err != nil {
		t.Fatal(err)
	}

	override := string(data)
	want := "PERSISTENT_VOLUME_MOUNTS: brezel-local:${BREZEL_WORKSPACE_DIR:"
	if !strings.Contains(override, want) {
		t.Fatalf("engine override must pass BREZEL_WORKSPACE_DIR to the host-namespace orchestrator")
	}
	if strings.Contains(override, "target: /var/lib/brezel-workspaces") {
		t.Fatalf("engine override contains a container-only workspace mount target")
	}
}
