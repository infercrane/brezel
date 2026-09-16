package singlehost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
		"api_patch_sha256":                               "0001-harden-volume-secrets-and-cleanup.patch",
		"api_build_patch_sha256":                         "0002-pin-api-build-images.patch",
		"orchestrator_lifecycle_patch_sha256":            "0003-acknowledge-delete-after-sandbox-teardown.patch",
		"orchestrator_cache_patch_sha256":                "0004-bound-snapshot-diff-cache.patch",
		"orchestrator_nfs_durability_patch_sha256":       "0005-make-nfs-writes-crash-durable.patch",
		"engine_start_admission_patch_sha256":            "0006-bound-start-admission-retries.patch",
		"engine_local_capacity_patch_sha256":             "0007-scale-local-resource-pools-and-template-shape.patch",
		"envd_process_tag_patch_sha256":                  "0008-fix-envd-process-tag-resolution.patch",
		"envd_process_replay_patch_sha256":               "0009-add-bounded-process-output-replay.patch",
		"orchestrator_cpu_topology_patch_sha256":         "0010-disable-smt-and-pin-exclusive-cpu-topology.patch",
		"orchestrator_rootfs_read_patch_sha256":          "0011-reduce-rootfs-read-amplification.patch",
		"orchestrator_nbd_multiqueue_patch_sha256":       "0012-harden-nbd-multiqueue-lifecycle.patch",
		"orchestrator_cpuset_qualification_patch_sha256": "0013-qualify-cpuset-exclusive-cpu-topology.patch",
		"orchestrator_ext4_dir_index_patch_sha256":       "0014-opt-in-ext4-dir-index.patch",
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

func TestInstallerPinsTargetedExt4DirIndexPatch(t *testing.T) {
	installerData, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerData)
	for _, required := range []string{
		"ENGINE_EXT4_DIR_INDEX_PATCH=",
		"orchestrator_ext4_dir_index_patch_sha256",
		"engine ext4-dir-index patch verification failed",
		`patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_EXT4_DIR_INDEX_PATCH"`,
		`"$ENGINE_EXT4_DIR_INDEX_PATCH_SHA256"`,
		"BREZEL_ENGINE_EXT4_DIR_INDEX_TEMPLATE_IDS",
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("installer is missing ext4-dir-index integrity binding %q", required)
		}
	}
	cpusetApply := strings.Index(installer, `-p1 < "$ENGINE_CPUSET_QUALIFICATION_PATCH"`)
	dirIndexApply := strings.Index(installer, `-p1 < "$ENGINE_EXT4_DIR_INDEX_PATCH"`)
	if cpusetApply < 0 || dirIndexApply <= cpusetApply {
		t.Fatal("ext4-dir-index patch is not applied after its pinned predecessor")
	}

	patchData, err := os.ReadFile(filepath.Join("..", "..", "third_party", "e2b-runtime", "patches", "0014-opt-in-ext4-dir-index.patch"))
	if err != nil {
		t.Fatal(err)
	}
	patch := string(patchData)
	for _, required := range []string{
		`env:"BUILD_EXT4_DIR_INDEX_TEMPLATE_IDS"`,
		"BuildExt4DirIndexForTemplate",
		"Ext4DirIndex bool",
		"featureflags.BuildExt4DirIndex",
		"DirIndex: r.buildContext.Rootfs.Ext4DirIndex",
		`const ext4DirIndexKey = "ext4-dir-index:v1"`,
		"the fallback preserves the legacy filesystem",
		"the flag can target one template without changing its siblings",
		"a build from a template inherits the parent's filesystem",
	} {
		if !strings.Contains(patch, required) {
			t.Fatalf("ext4-dir-index patch is missing contract %q", required)
		}
	}

	overrideData, err := os.ReadFile("engine.override.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(overrideData), "BUILD_EXT4_DIR_INDEX_TEMPLATE_IDS: ${BREZEL_ENGINE_EXT4_DIR_INDEX_TEMPLATE_IDS:-}") {
		t.Fatal("engine override does not pass the default-empty ext4 directory-index template allowlist")
	}

	supplyChainData, err := os.ReadFile("artifact-supply-chain.sh")
	if err != nil {
		t.Fatal(err)
	}
	supplyChain := string(supplyChainData)
	for _, required := range []string{
		"artifact.orchestrator.ext4_dir_index_patch_sha256",
		"artifact.template.ext4_dir_index=targeted-opt-in-default-disabled",
		"the ext4-dir-index patch requires the cpuset-qualification patch identity",
	} {
		if !strings.Contains(supplyChain, required) {
			t.Fatalf("artifact manifest is missing ext4-dir-index contract %q", required)
		}
	}
}

func TestDeploymentScriptsParse(t *testing.T) {
	for _, script := range []string{"install.sh", "qualify.sh", "host-reboot-drill.sh", "engine-capabilities.sh", "capacity-contract.sh", "engine-capacity-contract.sh", "engine-auth-cache-contract.sh", "artifact-supply-chain.sh", "runtime-attestation.sh", "benchmark.sh", "host-tuning.sh", "../profiles/run.sh", "../public-edge/preflight.sh", "../public-edge/up.sh"} {
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
	seedResolver := strings.Index(transaction, "WHERE name = 'local dev seed token'")
	update := strings.Index(transaction, "INSERT INTO public.project_limits")
	effective := strings.LastIndex(transaction, "FROM public.team_limits")
	if seedResolver < 0 || update <= seedResolver || effective <= update || !strings.Contains(transaction[effective:], "RAISE EXCEPTION") {
		t.Fatal("seed-team project limit is not asserted after its update and before commit")
	}
	for _, forbidden := range []string{
		"UPDATE public.tiers",
		"WHERE team_id IN (SELECT id FROM public.teams)",
	} {
		if strings.Contains(transaction, forbidden) {
			t.Fatalf("capacity reconciler mutates capacity outside the seeded service team: %q", forbidden)
		}
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
		`stop postgres`,
		`run --rm --no-deps host-setup`,
		`run --rm --no-deps fetch-artifacts`,
		`run --rm --no-deps brezel-envd-install`,
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

func TestPostgresCannotConsumeTheFirecrackerHugepagePool(t *testing.T) {
	override, err := os.ReadFile("engine.override.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(override), `command: ["postgres", "-c", "huge_pages=off"]`) {
		t.Fatal("PostgreSQL is not forced off the Firecracker hugetlb pool")
	}

	contract, err := os.ReadFile("engine-capacity-contract.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"SHOW huge_pages", `"$postgres_huge_pages" = off`} {
		if !strings.Contains(string(contract), required) {
			t.Fatalf("engine capacity contract does not verify %q", required)
		}
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
	for _, required := range []string{
		`BREZEL_GUEST_VCPUS=${BREZEL_GUEST_VCPUS:-2}`,
		`BREZEL_GUEST_MEMORY_MIB=${BREZEL_GUEST_MEMORY_MIB:-512}`,
		`BREZEL_GUEST_MIN_FREE_DISK_MIB=${BREZEL_GUEST_MIN_FREE_DISK_MIB:-512}`,
		`BREZEL_GUEST_MAX_FREE_DISK_MIB=${BREZEL_GUEST_MAX_FREE_DISK_MIB:-25600}`,
	} {
		if !strings.Contains(content, required) {
			t.Fatalf("qualification capacity preflight omitted %q", required)
		}
	}
}

func TestPublicEdgeIsNarrowAndDoesNotLogCapabilityURLs(t *testing.T) {
	caddy, err := os.ReadFile(filepath.Join("..", "public-edge", "Caddyfile"))
	if err != nil {
		t.Fatal(err)
	}
	configuration := string(caddy)
	for _, required := range []string{
		`handle @sandbox_collection`,
		`handle @sandbox_command`,
		`handle @preview`,
		`handle {`,
		`respond 404`,
		`flush_interval -1`,
	} {
		if !strings.Contains(configuration, required) {
			t.Fatalf("public edge configuration omitted %q", required)
		}
	}
	if strings.Contains(configuration, "log {") || strings.Contains(configuration, "access_log") {
		t.Fatal("public edge must not log preview capabilities or file paths from request URLs")
	}

	compose, err := os.ReadFile(filepath.Join("..", "public-edge", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"read_only: true", "cap_drop: [ALL]", "pids_limit:", "mem_limit:", "no-new-privileges:true", "max-size: 10m"} {
		if !strings.Contains(string(compose), required) {
			t.Fatalf("public edge container contract omitted %q", required)
		}
	}
	if !strings.Contains(string(compose), `user: "${BREZEL_EDGE_UID:`) || !strings.Contains(string(compose), `${BREZEL_EDGE_GID:`) {
		t.Fatal("public edge does not run as the protected directory owner")
	}

	preflight, err := os.ReadFile(filepath.Join("..", "public-edge", "preflight.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(preflight), "eval ") {
		t.Fatal("public edge preflight executes a directory variable through eval")
	}
	for _, required := range []string{"existing non-symlink directory", "group- or world-writable", "caddy validate", "BREZEL_EDGE_UID", "BREZEL_EDGE_GID"} {
		if !strings.Contains(string(preflight), required) {
			t.Fatalf("public edge preflight omitted %q", required)
		}
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
		"NBDS_MAX: ${BREZEL_ENGINE_NBD_POOL_SIZE:-64}",
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
	topology := filepath.Join(t.TempDir(), "cpu-topology.csv")
	var topologyRows strings.Builder
	for cpu := 0; cpu < 32; cpu++ {
		core := cpu % 16
		node := core / 8
		fmt.Fprintf(&topologyRows, "%d,%d,0,%d\n", cpu, node, core)
	}
	if err := os.WriteFile(topology, []byte(topologyRows.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func(mode string, env ...string) ([]byte, error) {
		command := exec.Command("sh", "capacity-contract.sh", mode)
		command.Env = append(os.Environ(), append([]string{
			"BREZEL_TEST_MEMINFO_FILE=" + meminfo,
			"BREZEL_TEST_CPU_COUNT=32",
			"BREZEL_TEST_CPU_TOPOLOGY_FILE=" + topology,
			"BREZEL_TEST_AVAILABLE_DISK_KIB=67108864",
			"BREZEL_TEST_NBD_MAX=128",
		}, env...)...)
		return command.CombinedOutput()
	}

	if output, err := run("plan"); err != nil {
		t.Fatalf("default capacity plan failed: %v: %s", err, output)
	} else if !strings.Contains(string(output), `"required_hugepages_2m":9216`) {
		t.Fatalf("capacity plan did not bind quota to lifecycle headroom: %s", output)
	} else if !strings.Contains(string(output), `"max_starting_sandboxes":3`) {
		t.Fatalf("capacity plan did not emit the local starting-sandbox limit: %s", output)
	} else if !strings.Contains(string(output), `"nbd_connections_per_device":1`) {
		t.Fatalf("capacity plan did not emit the qualified NBD queue count: %s", output)
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
	if output, err := run("plan", "BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL=4", "BREZEL_MAX_ACTIVE_SANDBOXES_PER_PROJECT=4", "BREZEL_ENGINE_MAX_STARTING_SANDBOXES=5"); err == nil {
		t.Fatalf("capacity contract accepted a starting limit above the active limit: %s", output)
	} else if !strings.Contains(string(output), "starting-sandbox limit cannot exceed") {
		t.Fatalf("starting-limit capacity failure was unclear: %s", output)
	}
	if output, err := run("plan", "BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL=4", "BREZEL_MAX_ACTIVE_SANDBOXES_PER_PROJECT=4", "BREZEL_ENGINE_MAX_STARTING_SANDBOXES=4", "BREZEL_ENGINE_HUGEPAGES=2048"); err != nil {
		t.Fatalf("capacity contract rejected a coherent four-sandbox profile: %v: %s", err, output)
	}
	if output, err := run("plan", "BREZEL_WARM_POOL_SIZE=4"); err == nil {
		t.Fatalf("capacity contract accepted strict warm capacity below the active ceiling: %s", output)
	}
	if output, err := run("plan", "BREZEL_WARM_POOL_SIZE=32", "BREZEL_WARM_POOL_PRIME_CONCURRENCY=33"); err == nil {
		t.Fatalf("capacity contract accepted excessive warm-pool prime concurrency: %s", output)
	}
	if output, err := run("plan", "BREZEL_TEST_CPU_COUNT=1"); err == nil {
		t.Fatalf("capacity contract accepted a host smaller than one guest: %s", output)
	}
	if output, err := run("plan", "BREZEL_TEST_CPU_COUNT=8", "BREZEL_GUEST_VCPUS=8", "BREZEL_MIN_SYSTEM_CPUS=8"); err == nil {
		t.Fatalf("capacity contract accepted a benchmark guest with no host CPU reserve: %s", output)
	} else if !strings.Contains(string(output), "require at least 16") {
		t.Fatalf("CPU reserve failure did not explain the required host capacity: %s", output)
	}
	if output, err := run("plan", "BREZEL_ENGINE_FIRECRACKER_EXCLUSIVE_CPU_TOPOLOGY=maybe"); err == nil {
		t.Fatalf("capacity contract accepted an invalid exclusive CPU-topology setting: %s", output)
	}
	if output, err := run("plan", "BREZEL_ENGINE_FIRECRACKER_EXCLUSIVE_CPU_TOPOLOGY=true"); err == nil {
		t.Fatalf("capacity contract accepted exclusive CPU placement without a cpuset contract: %s", output)
	} else if !strings.Contains(string(output), "BREZEL_ENGINE_FIRECRACKER_CPUSET_CPUS is required") {
		t.Fatalf("exclusive CPU-topology failure was unclear: %s", output)
	}
	exclusiveTopology := filepath.Join(t.TempDir(), "exclusive-cpu-topology.csv")
	var exclusiveTopologyRows strings.Builder
	for cpu := 0; cpu < 40; cpu++ {
		core := cpu % 20
		node := core / 10
		fmt.Fprintf(&exclusiveTopologyRows, "%d,%d,0,%d\n", cpu, node, core)
	}
	if err := os.WriteFile(exclusiveTopology, []byte(exclusiveTopologyRows.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	exclusiveContract := []string{
		"BREZEL_ENGINE_FIRECRACKER_EXCLUSIVE_CPU_TOPOLOGY=true",
		"BREZEL_ENGINE_FIRECRACKER_CPUSET_CPUS=0-8,20-28",
		"BREZEL_ENGINE_FIRECRACKER_CPUSET_MEMS=0",
		"BREZEL_ENGINE_FIRECRACKER_VCPU_CPUS=0-7",
		"BREZEL_ENGINE_FIRECRACKER_VMM_CPUS=8,28",
		"BREZEL_TEST_CPU_COUNT=40",
		"BREZEL_TEST_CPU_TOPOLOGY_FILE=" + exclusiveTopology,
		"BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL=1",
		"BREZEL_MAX_ACTIVE_SANDBOXES_PER_PROJECT=1",
		"BREZEL_ENGINE_MAX_STARTING_SANDBOXES=1",
		"BREZEL_GUEST_VCPUS=8",
		"BREZEL_MIN_SYSTEM_CPUS=8",
		"BREZEL_ENGINE_HUGEPAGES=5120",
	}
	if output, err := run("plan", exclusiveContract...); err != nil {
		t.Fatalf("capacity contract rejected a complete exclusive CPU contract: %v: %s", err, output)
	} else if !strings.Contains(string(output), `"firecracker_cpuset_cpus":"0,1,2,3,4,5,6,7,8,20,21,22,23,24,25,26,27,28"`) {
		t.Fatalf("capacity contract did not normalize the qualified CPU set: %s", output)
	}
	for _, variable := range []string{
		"BREZEL_ENGINE_FIRECRACKER_CPUSET_CPUS",
		"BREZEL_ENGINE_FIRECRACKER_CPUSET_MEMS",
		"BREZEL_ENGINE_FIRECRACKER_VCPU_CPUS",
		"BREZEL_ENGINE_FIRECRACKER_VMM_CPUS",
	} {
		malformedContract := append([]string(nil), exclusiveContract...)
		for index, value := range malformedContract {
			if strings.HasPrefix(value, variable+"=") {
				malformedContract[index] = variable + "=bad"
			}
		}
		if output, err := run("plan", malformedContract...); err == nil {
			t.Fatalf("capacity contract accepted malformed %s: %s", variable, output)
		} else if !strings.Contains(string(output), variable+" is invalid") {
			t.Fatalf("malformed %s failure was unclear: %s", variable, output)
		}
	}
	multiSandboxContract := append([]string{}, exclusiveContract...)
	for index, value := range multiSandboxContract {
		if strings.HasPrefix(value, "BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL=") {
			multiSandboxContract[index] = "BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL=2"
		}
		if strings.HasPrefix(value, "BREZEL_MAX_ACTIVE_SANDBOXES_PER_PROJECT=") {
			multiSandboxContract[index] = "BREZEL_MAX_ACTIVE_SANDBOXES_PER_PROJECT=2"
		}
	}
	if output, err := run("plan", multiSandboxContract...); err == nil {
		t.Fatalf("capacity contract accepted more than one sandbox in the exclusive profile: %s", output)
	} else if !strings.Contains(string(output), "single-sandbox profile") {
		t.Fatalf("exclusive single-sandbox failure was unclear: %s", output)
	}
	if output, err := run("plan",
		"BREZEL_ENGINE_FIRECRACKER_VCPU_CPUS=0-7",
	); err == nil {
		t.Fatalf("capacity contract accepted armed cpuset values while exclusive placement was disabled: %s", output)
	} else if !strings.Contains(string(output), "cpuset settings require exclusive CPU topology") {
		t.Fatalf("disabled exclusive CPU-topology failure was unclear: %s", output)
	}
	smtContract := append([]string{}, exclusiveContract...)
	smtContract = append(smtContract, "BREZEL_ENGINE_FIRECRACKER_SMT=true")
	if output, err := run("plan", smtContract...); err == nil {
		t.Fatalf("capacity contract accepted exclusive CPU placement with guest SMT: %s", output)
	} else if !strings.Contains(string(output), "requires guest SMT to be disabled") {
		t.Fatalf("exclusive SMT failure was unclear: %s", output)
	}
	if output, err := run("plan", "BREZEL_TEST_AVAILABLE_DISK_KIB=1024"); err == nil {
		t.Fatalf("capacity contract accepted insufficient host disk: %s", output)
	}
	if output, err := run("live", "BREZEL_TEST_NBD_MAX=32", "BREZEL_ENGINE_NBD_POOL_SIZE=64"); err == nil {
		t.Fatalf("capacity contract accepted an undersized kernel NBD ceiling: %s", output)
	}
	if output, err := run("plan", "BREZEL_ENGINE_NBD_CONNECTIONS_PER_DEVICE=5"); err == nil {
		t.Fatalf("capacity contract accepted more than four NBD queues: %s", output)
	} else if !strings.Contains(string(output), "cannot exceed 4") {
		t.Fatalf("NBD queue-count failure was unclear: %s", output)
	}
	lowFree := strings.Replace(content, "HugePages_Free:     9216", "HugePages_Free:     1024", 1)
	if err := os.WriteFile(meminfo, []byte(lowFree), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := run("live"); err == nil {
		t.Fatalf("capacity contract accepted a consumed hugepage pool: %s", output)
	}
}

func TestCapacityProfilesArePhysicallyCoherent(t *testing.T) {
	profiles := []struct {
		name      string
		memoryKiB int
		want      string
	}{
		{name: "computesdk-dax.env", memoryKiB: 48 * 1024 * 1024, want: `"guest_vcpus":8`},
		{name: "burst-100-capacity.env", memoryKiB: 64 * 1024 * 1024, want: `"warm_pool_size":100`},
	}
	for _, profile := range profiles {
		t.Run(profile.name, func(t *testing.T) {
			profilePath := filepath.Join("..", "profiles", profile.name)
			if _, err := os.Stat(profilePath); err != nil {
				t.Fatal(err)
			}
			meminfo := filepath.Join(t.TempDir(), "meminfo")
			if err := os.WriteFile(meminfo, []byte("MemTotal: "+fmt.Sprint(profile.memoryKiB)+" kB\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			topology := filepath.Join(t.TempDir(), "cpu-topology.csv")
			var topologyRows strings.Builder
			for cpu := 0; cpu < 32; cpu++ {
				core := cpu % 16
				node := core / 8
				fmt.Fprintf(&topologyRows, "%d,%d,0,%d\n", cpu, node, core)
			}
			if err := os.WriteFile(topology, []byte(topologyRows.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command("sh", "-c", `BREZEL_TEST_MEMINFO_FILE="$2" BREZEL_TEST_CPU_COUNT=32 BREZEL_TEST_CPU_TOPOLOGY_FILE="$3" BREZEL_TEST_AVAILABLE_DISK_KIB=209715200 exec sh ../profiles/run.sh "$1" sh capacity-contract.sh plan`, "profile", profilePath, meminfo, topology)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("profile rejected: %v: %s", err, output)
			}
			if !strings.Contains(string(output), profile.want) {
				t.Fatalf("profile result omitted %s: %s", profile.want, output)
			}
		})
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

func TestArtifactSupplyChainVerifiesInstalledEnvdOverride(t *testing.T) {
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
		if name == "envd" {
			path = "/fc/envd"
		}
		lock += name + "_path=" + path + "\n"
		lock += name + "_sha256=" + hex.EncodeToString(upstreamDigest[:]) + "\n"
	}
	lockPath := filepath.Join(root, "artifacts.lock")
	if err := os.WriteFile(lockPath, []byte(lock), 0o600); err != nil {
		t.Fatal(err)
	}

	custom := []byte("source-built envd")
	envd := filepath.Join(root, "fc", "envd")
	if err := os.WriteFile(envd, custom, 0o700); err != nil {
		t.Fatal(err)
	}
	customDigest := sha256.Sum256(custom)
	command := exec.Command("sh", "artifact-supply-chain.sh", "host", lockPath, root, "", hex.EncodeToString(customDigest[:]))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("host artifact gate rejected the expected custom envd: %v: %s", err, output)
	}

	badDigest := strings.Repeat("0", 64)
	command = exec.Command("sh", "artifact-supply-chain.sh", "host", lockPath, root, "", badDigest)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("host artifact gate accepted the wrong custom envd digest: %s", output)
	}
}

func TestDistributionManifestAttestsInstalledEnvdOverride(t *testing.T) {
	root := t.TempDir()
	fcRoot := filepath.Join(root, "fc")
	if err := os.MkdirAll(fcRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	upstream := []byte("upstream artifact")
	upstreamDigest := sha256.Sum256(upstream)
	upstreamPath := filepath.Join(fcRoot, "upstream")
	if err := os.WriteFile(upstreamPath, upstream, 0o700); err != nil {
		t.Fatal(err)
	}

	orchestratorBytes := []byte("source-built orchestrator")
	orchestratorDigest := sha256.Sum256(orchestratorBytes)
	if err := os.WriteFile(filepath.Join(fcRoot, "orchestrator"), orchestratorBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	envdBytes := []byte("source-built envd")
	envdDigest := sha256.Sum256(envdBytes)
	if err := os.WriteFile(filepath.Join(fcRoot, "envd"), envdBytes, 0o700); err != nil {
		t.Fatal(err)
	}

	artifactLock := "architecture=linux/amd64\n"
	for _, name := range []string{"orchestrator", "envd", "firecracker", "kernel", "busybox"} {
		path := "/fc/upstream"
		if name == "orchestrator" {
			path = "/fc/orchestrator"
		} else if name == "envd" {
			path = "/fc/envd"
		}
		artifactLock += name + "_path=" + path + "\n"
		artifactLock += name + "_sha256=" + hex.EncodeToString(upstreamDigest[:]) + "\n"
	}
	artifactLockPath := filepath.Join(root, "artifacts.lock")
	if err := os.WriteFile(artifactLockPath, []byte(artifactLock), 0o600); err != nil {
		t.Fatal(err)
	}
	engineLockPath := filepath.Join(root, "engine.lock")
	if err := os.WriteFile(engineLockPath, []byte("commit=fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	imageLockPath := filepath.Join(root, "images.lock")
	imageLock := ""
	for _, key := range []string{
		"BREZEL_ENGINE_POSTGRES_IMAGE", "BREZEL_ENGINE_REDIS_IMAGE",
		"BREZEL_ENGINE_CLICKHOUSE_IMAGE", "BREZEL_ENGINE_VECTOR_IMAGE",
		"E2B_DB_MIGRATOR_IMAGE", "E2B_CLIENT_PROXY_IMAGE",
		"E2B_CLICKHOUSE_MIGRATOR_IMAGE", "E2B_TOOLS_IMAGE",
		"E2B_NODE_E2B_IMAGE", "E2B_SEED_IMAGE",
	} {
		imageLock += key + "=fixture\n"
	}
	if err := os.WriteFile(imageLockPath, []byte(imageLock), 0o600); err != nil {
		t.Fatal(err)
	}

	manifestPath := filepath.Join(root, "distribution.manifest")
	patchDigest := strings.Repeat("1", 64)
	command := exec.Command(
		"sh", "artifact-supply-chain.sh", "manifest", manifestPath,
		engineLockPath, imageLockPath, artifactLockPath, root,
		hex.EncodeToString(orchestratorDigest[:]),
		patchDigest, patchDigest, patchDigest, patchDigest, patchDigest,
		patchDigest,
		patchDigest,
		patchDigest,
		patchDigest,
		hex.EncodeToString(envdDigest[:]), patchDigest,
		patchDigest,
		patchDigest,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("distribution manifest rejected source-built envd: %v: %s", err, output)
	}
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"artifact.envd.upstream_sha256=" + hex.EncodeToString(upstreamDigest[:]),
		"artifact.envd.sha256=" + hex.EncodeToString(envdDigest[:]),
		"artifact.envd.process_tag_patch_sha256=" + patchDigest,
		"artifact.envd.live_tag_resolution=complete-map-scan",
		"artifact.envd.process_replay_patch_sha256=" + patchDigest,
		"artifact.envd.process_output_recovery=generation-bound-cursor-journal",
		"artifact.orchestrator.cpu_topology_patch_sha256=" + patchDigest,
		"artifact.orchestrator.guest_smt=operator-configured-default-disabled",
		"artifact.orchestrator.exclusive_cpu_topology=disabled-by-default",
		"artifact.orchestrator.rootfs_read_patch_sha256=" + patchDigest,
		"artifact.orchestrator.rootfs_read_path=allocation-free-local-and-whole-writable-range",
		"artifact.orchestrator.nbd_multiqueue_patch_sha256=" + patchDigest,
		"artifact.orchestrator.nbd_connections_per_device=operator-configured-default-one-range-one-to-four",
		"artifact.orchestrator.nbd_lifecycle=attempt-owned-idempotent-cleanup",
		"artifact.orchestrator.cpuset_qualification_patch_sha256=" + patchDigest,
		"artifact.orchestrator.exclusive_cpu_isolation=qualified-only-when-explicitly-configured",
		"artifact.orchestrator.ext4_dir_index_patch_sha256=" + patchDigest,
		"artifact.template.ext4_dir_index=targeted-opt-in-default-disabled",
	} {
		if !strings.Contains(string(manifest), expected) {
			t.Fatalf("distribution manifest omitted %q: %s", expected, manifest)
		}
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
		"ENGINE_ENVD_PROCESS_TAG_PATCH",
		"BREZEL_ENGINE_ENVD_IMAGE",
		"BREZEL_ENGINE_ENVD_SHA256",
		"docker create --entrypoint /envd",
		"brezel-envd-install",
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
		"brezel-envd-install", "BREZEL_ENGINE_ENVD_BINARY",
		"BREZEL_ENGINE_ENVD_SHA256",
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
		"packages/orchestrator/pkg/cfg/model.go":                              "env:\"MAX_STARTING_INSTANCES_PER_NODE\"\nenv:\"NETWORK_NEW_SLOTS_POOL_SIZE\"\nenv:\"NETWORK_REUSED_SLOTS_POOL_SIZE\"\nenv:\"NBD_CONNECTIONS_PER_DEVICE\"\nNBD_CONNECTIONS_PER_DEVICE must be between 1 and 4\nenv:\"FIRECRACKER_SMT\"\nenv:\"FIRECRACKER_EXCLUSIVE_CPU_TOPOLOGY\"\nenv:\"FIRECRACKER_CPUSET_CPUS\"\nis required when FIRECRACKER_EXCLUSIVE_CPU_TOPOLOGY=true\nenv:\"BUILD_CACHE_TTL\"\nenv:\"BUILD_CACHE_MAX_BYTES\"\nenv:\"BUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT\"\nBUILD_CACHE_TTL must be at least 1h\nBUILD_CACHE_MAX_BYTES must be zero or at least 1 GiB\nBUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT must be between 1 and 100\nenv:\"BUILD_EXT4_DIR_INDEX_TEMPLATE_IDS\"\nBuildExt4DirIndexForTemplate\n",
		"packages/orchestrator/pkg/template/build/builder.go":                 "builders = append(builders, optimizeBuilder)\nfeatureflags.BuildExt4DirIndex\n",
		"packages/orchestrator/pkg/template/build/buildcontext/context.go":    "Ext4DirIndex bool\n",
		"packages/orchestrator/pkg/template/build/core/rootfs/rootfs.go":      "DirIndex: r.buildContext.Rootfs.Ext4DirIndex\n",
		"packages/orchestrator/pkg/template/build/phases/base/hash.go":        "ext4-dir-index:v1\n",
		"packages/orchestrator/pkg/template/build/phases/optimize/builder.go": "WithPrefetch(&metadata.Prefetch\ncontinuing without prefetch\n",
		"packages/orchestrator/pkg/sandbox/sandbox.go":                        "prefetch.New(sbxLogger, memfile, fcUffd, initMapping\nrootfs.NewNBDProvider\nexclusive CPU placement requires sandbox cgroup creation\n",
		"packages/orchestrator/pkg/sandbox/cgroup/manager.go":                 "cpuset.cpus.partition\ncpuset.cpus.exclusive.effective\n",
		"packages/shared/pkg/featureflags/flags.go":                           "NewStringFlag(\"resume-prefetch-source\", \"init\")\n",
		"packages/shared/pkg/storage/sandbox.go":                              "fmt.Sprintf(\"rootfs-%s-%s.cow\"\nenvDefault:\"${ORCHESTRATOR_BASE_PATH}/sandbox\"\nenvDefault:\"${ORCHESTRATOR_BASE_PATH}/template\"\n",
		"packages/orchestrator/pkg/sandbox/block/local.go":                    "d.f.ReadAt(p[:length], off)\n",
		"packages/orchestrator/pkg/sandbox/block/overlay.go":                  "if cacheRangeValid && length > o.blockSize && length%o.blockSize == 0 {\n",
		"packages/orchestrator/pkg/sandbox/block/overlay_read_test.go":        "TestOverlayReadAtMixedWritableAndBaseBlocks\n",
		"packages/orchestrator/pkg/sandbox/network/pool.go":                   "NewSlotsPoolSize    = 32\nReusedSlotsPoolSize = 100\n",
		"packages/orchestrator/pkg/factories/run.go":                          "network.NewPool(config.NetworkNewSlotsPoolSize, config.NetworkReusedSlotsPoolSize\nnetworkv2.WithPoolSizes(config.NetworkNewSlotsPoolSize, config.NetworkReusedSlotsPoolSize)\nexclusive CPU placement requires complete startup resource reclamation\n",
		"packages/orchestrator/pkg/server/sandboxes.go":                       "if err := sbx.Stop(ctx); err != nil\nSandboxes.WaitLifecycle(ctx\n",
		"packages/orchestrator/pkg/sandbox/map.go":                            "func (m *Map) WaitLifecycle(ctx context.Context\n",
		"packages/orchestrator/pkg/sandbox/nbd/path_direct.go":                "WithConnectionsPerDevice\n",
		"packages/orchestrator/pkg/sandbox/nbd/path_direct_lifecycle_test.go": "TestDirectPathMountConnectRetryFullyCleansPreviousAttempt\nTestDirectPathMountCloseIsSerializedAndIdempotent\n",
		"packages/orchestrator/pkg/sandbox/fc/cpu_affinity.go":                "no NUMA node has %d distinct physical cores\nanother Firecracker process holds the exclusive CPU lease\nFirecracker vCPU thread %d was not present after VM start\nstabilizeExclusiveCPUPlacement\nFirecracker is outside the qualified exclusive CPU cgroup\nvalidateEffectiveSandboxCPUSet(cgroupPath, placement.reservedCPUs, expectedMems)\nreturn fmt.Errorf(\"Firecracker sandbox %s drifted\", check.name)\n",
		"packages/orchestrator/pkg/sandbox/fc/process.go":                     "monitorExclusiveCPUPlacement\nreconcile exclusive Firecracker CPU topology\n",
		"packages/orchestrator/pkg/server/main.go":                            "resolveStartingSandboxesLimit\n",
		"packages/api/internal/orchestrator/placement/placement.go":           "resourceExhaustedRetryDelay\n",
		"packages/api/internal/orchestrator/placement/config.go":              "resourceExhaustedBackoffMax\n",
		"packages/orchestrator/pkg/sandbox/build/cache.go":                    "func allocatedBytes(path string)\ncachePressureObservationFailed\ncachePressureAllocatedBytes\norchestrator.build.cache.pressure_evictions\n",
		"packages/orchestrator/pkg/sandbox/template/cache.go":                 "config.BuildCacheTTL\n",
		"packages/orchestrator/pkg/nfsproxy/chroot/file.go":                   "syncing NFS write\nsyncing NFS truncate\n",
		"packages/orchestrator/pkg/nfsproxy/chroot/fs.go":                     "syncDirectoryTree\nerrors.Join(syncPath(f.chroot, newParent), syncPath(f.chroot, oldParent))\n",
		"embed/compose/compose.yaml":                                          "TEMPLATE_STORAGE_URL: file:///var/lib/e2b/storage/templates\nNBD_POOL_SIZE: \"64\"\nNETWORK_VERSION: \"1\"\n",
		"embed/compose/scripts/node/build-base-template.mjs":                  "BASE_TEMPLATE_MIN_FREE_DISK_MB\nminFreeDiskMb\n",
		"packages/envd/internal/services/process/service.go":                  "if value.Tag == nil || *value.Tag != tag {\n",
		"packages/envd/internal/services/process/service_test.go":             "TestGetProcessByTagScansPastNonMatches\nrequire.Same(t, target, got)\n",
		"packages/envd/internal/services/process/replay.go":                   "replayVersionHeader = \"E2b-Process-Replay-Version\"\nreturn replayRequest{}, fmt.Errorf(\"%s is required with %s\", journalIDHeader, afterSequenceHeader)\n",
		"packages/envd/internal/services/process/handler/journal.go":          "defaultJournalBytes       = 8 << 20\ndefaultJournalStoreBytes  = 32 << 20\n",
		"packages/envd/internal/services/process/connect_test.go":             "TestConnect_ReplayFailsExplicitlyAfterEviction\n",
		"packages/envd/internal/services/process/handler/journal_test.go":     "TestEventJournalAtomicReplayToWaitHandoff\n",
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
		`/bin/sh -s -- live "$engine_sandbox_id" "$min_network_slots" "$max_starting_sandboxes"`,
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
		"NETWORK_NEW_SLOTS_POOL_SIZE",
		"NETWORK_REUSED_SLOTS_POOL_SIZE",
		"NBD_POOL_SIZE",
		"NBD_CONNECTIONS_PER_DEVICE",
		"WithConnectionsPerDevice",
		"TestDirectPathMountConnectRetryFullyCleansPreviousAttempt",
		"TestDirectPathMountCloseIsSerializedAndIdempotent",
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
		"syncing NFS write",
		"syncing NFS truncate",
		"syncDirectoryTree",
		"fsync_before_success",
		"MAX_STARTING_INSTANCES_PER_NODE",
		"resourceExhaustedRetryDelay",
		"resourceExhaustedBackoffMax",
		"max_starting_sandboxes",
		"TestGetProcessByTagScansPastNonMatches",
		"complete-map-scan",
	} {
		if !strings.Contains(probe, required) {
			t.Fatalf("engine capability probe is missing %q", required)
		}
	}
}

func TestInstallerPinsAndAttestsNFSDurabilityPatch(t *testing.T) {
	installerData, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerData)
	for _, required := range []string{
		"0005-make-nfs-writes-crash-durable.patch",
		"orchestrator_nfs_durability_patch_sha256",
		`patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_NFS_DURABILITY_PATCH"`,
		`"$ENGINE_NFS_DURABILITY_PATCH_SHA256"`,
		"engine orchestrator NFS durability patch verification failed",
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("installer is missing NFS durability invariant %q", required)
		}
	}

	supplyChainData, err := os.ReadFile("artifact-supply-chain.sh")
	if err != nil {
		t.Fatal(err)
	}
	supplyChain := string(supplyChainData)
	for _, required := range []string{
		"artifact.orchestrator.nfs_durability_patch_sha256",
		"artifact.orchestrator.nfs_write_stability=fsync-before-file-sync-acknowledgement",
		"artifact.orchestrator.nfs_namespace_stability=parent-directory-fsync-before-acknowledgement",
		"the NFS durability patch requires the cache patch identity",
	} {
		if !strings.Contains(supplyChain, required) {
			t.Fatalf("distribution manifest writer is missing NFS durability identity %q", required)
		}
	}
}

func TestInstallerPinsAndValidatesStartAdmissionPatch(t *testing.T) {
	installerData, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerData)
	for _, required := range []string{
		"0006-bound-start-admission-retries.patch",
		"engine_start_admission_patch_sha256",
		"BREZEL_ENGINE_MAX_STARTING_SANDBOXES:-3",
		`patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_START_ADMISSION_PATCH"`,
		`"$ENGINE_NFS_DURABILITY_PATCH_SHA256" "$ENGINE_START_ADMISSION_PATCH_SHA256"`,
		"engine start-admission patch verification failed",
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("installer is missing start-admission invariant %q", required)
		}
	}

	overrideData, err := os.ReadFile("engine.override.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if override := string(overrideData); !strings.Contains(override, "MAX_STARTING_INSTANCES_PER_NODE: ${BREZEL_ENGINE_MAX_STARTING_SANDBOXES:") {
		t.Fatal("engine override does not pass the operator-owned starting-sandbox limit")
	}

	supplyChainData, err := os.ReadFile("artifact-supply-chain.sh")
	if err != nil {
		t.Fatal(err)
	}
	supplyChain := string(supplyChainData)
	for _, required := range []string{
		"artifact.engine.start_admission_patch_sha256",
		"artifact.orchestrator.start_admission=operator-pinned-local-limit",
		"artifact.api.capacity_retry=capped-exponential-backoff-with-jitter",
		"the start-admission patch requires the NFS durability patch identity",
	} {
		if !strings.Contains(supplyChain, required) {
			t.Fatalf("distribution manifest writer is missing start-admission identity %q", required)
		}
	}

	probeData, err := os.ReadFile("engine-capabilities.sh")
	if err != nil {
		t.Fatal(err)
	}
	probe := string(probeData)
	for _, required := range []string{
		`env:"MAX_STARTING_INSTANCES_PER_NODE"`,
		"resolveStartingSandboxesLimit",
		"resourceExhaustedRetryDelay",
		"resourceExhaustedBackoffMax",
		"actual_starting_limit",
	} {
		if !strings.Contains(probe, required) {
			t.Fatalf("engine capability probe is missing start-admission contract %q", required)
		}
	}
}

func TestInstallerPinsAndValidatesLocalCapacityPatch(t *testing.T) {
	installerData, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerData)
	for _, required := range []string{
		"0007-scale-local-resource-pools-and-template-shape.patch",
		"engine_local_capacity_patch_sha256",
		`patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_LOCAL_CAPACITY_PATCH"`,
		`BREZEL_ENGINE_BASE_TEMPLATE_SCRIPT="$INSTALL_DIR/artifacts/build-base-template.mjs"`,
		`BREZEL_ENGINE_BASE_TEMPLATE_SCRIPT_SHA256=$(sha256sum "$BREZEL_ENGINE_BASE_TEMPLATE_SCRIPT"`,
		"BREZEL_ENGINE_BASE_TEMPLATE_SOURCE_IMAGE must name an immutable OCI image manifest by SHA-256 digest",
		"export BREZEL_ENGINE_BASE_TEMPLATE_SOURCE_IMAGE",
		`"$ENGINE_START_ADMISSION_PATCH_SHA256" "$ENGINE_LOCAL_CAPACITY_PATCH_SHA256"`,
		"engine local-capacity patch verification failed",
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("installer is missing local-capacity invariant %q", required)
		}
	}

	overrideData, err := os.ReadFile("engine.override.yaml")
	if err != nil {
		t.Fatal(err)
	}
	override := string(overrideData)
	for _, required := range []string{
		"NETWORK_NEW_SLOTS_POOL_SIZE: ${BREZEL_ENGINE_NETWORK_NEW_SLOTS:",
		"NETWORK_REUSED_SLOTS_POOL_SIZE: ${BREZEL_ENGINE_NETWORK_REUSED_SLOTS:",
		"NBD_POOL_SIZE: ${BREZEL_ENGINE_NBD_POOL_SIZE:",
		"BASE_TEMPLATE_CPU_COUNT: ${BREZEL_GUEST_VCPUS:-2}",
		"BASE_TEMPLATE_MIN_FREE_DISK_MB: ${BREZEL_GUEST_MIN_FREE_DISK_MIB:-512}",
		"BASE_TEMPLATE_SOURCE_IMAGE: ${BREZEL_ENGINE_BASE_TEMPLATE_SOURCE_IMAGE:",
		`FORCE_REBUILD: "1"`,
		"BREZEL_ENGINE_BASE_TEMPLATE_SCRIPT_SHA256:",
		"/opt/brezel/build-base-template.mjs:ro",
		"mounted builder digest mismatch",
	} {
		if !strings.Contains(override, required) {
			t.Fatalf("engine override is missing local-capacity setting %q", required)
		}
	}

	contractData, err := os.ReadFile("engine-capacity-contract.sh")
	if err != nil {
		t.Fatal(err)
	}
	contract := string(contractData)
	for _, required := range []string{
		"active base template does not match the operator guest shape",
		"public.env_build_assignments",
		"public.env_builds",
		"active_base_template_verified",
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("engine capacity contract is missing template-shape gate %q", required)
		}
	}

	supplyChainData, err := os.ReadFile("artifact-supply-chain.sh")
	if err != nil {
		t.Fatal(err)
	}
	supplyChain := string(supplyChainData)
	for _, required := range []string{
		"artifact.engine.local_capacity_patch_sha256",
		"artifact.orchestrator.local_resource_pools=operator-sized",
		"artifact.template.resource_shape=operator-sized",
		"the local-capacity patch requires the start-admission patch identity",
	} {
		if !strings.Contains(supplyChain, required) {
			t.Fatalf("distribution manifest writer is missing local-capacity identity %q", required)
		}
	}
}

func TestInstallerPinsAndValidatesFirecrackerCPUTopologyPatch(t *testing.T) {
	installerData, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerData)
	for _, required := range []string{
		"0010-disable-smt-and-pin-exclusive-cpu-topology.patch",
		"orchestrator_cpu_topology_patch_sha256",
		`patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_CPU_TOPOLOGY_PATCH"`,
		"engine Firecracker CPU-topology patch verification failed",
		"0013-qualify-cpuset-exclusive-cpu-topology.patch",
		"orchestrator_cpuset_qualification_patch_sha256",
		`patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_CPUSET_QUALIFICATION_PATCH"`,
		"engine cpuset-qualification patch verification failed",
		`"$ENGINE_CPU_TOPOLOGY_PATCH_SHA256" "$ENGINE_ROOTFS_READ_PATCH_SHA256" "$ENGINE_NBD_MULTIQUEUE_PATCH_SHA256" "$ENGINE_CPUSET_QUALIFICATION_PATCH_SHA256"`,
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("installer is missing CPU-topology invariant %q", required)
		}
	}

	overrideData, err := os.ReadFile("engine.override.yaml")
	if err != nil {
		t.Fatal(err)
	}
	override := string(overrideData)
	for _, required := range []string{
		"FIRECRACKER_SMT: ${BREZEL_ENGINE_FIRECRACKER_SMT:-false}",
		"FIRECRACKER_EXCLUSIVE_CPU_TOPOLOGY: ${BREZEL_ENGINE_FIRECRACKER_EXCLUSIVE_CPU_TOPOLOGY:-false}",
		"FIRECRACKER_CPUSET_CPUS: ${BREZEL_ENGINE_FIRECRACKER_CPUSET_CPUS:-}",
		"FIRECRACKER_CPUSET_MEMS: ${BREZEL_ENGINE_FIRECRACKER_CPUSET_MEMS:-}",
		"FIRECRACKER_VCPU_CPUS: ${BREZEL_ENGINE_FIRECRACKER_VCPU_CPUS:-}",
		"FIRECRACKER_VMM_CPUS: ${BREZEL_ENGINE_FIRECRACKER_VMM_CPUS:-}",
	} {
		if !strings.Contains(override, required) {
			t.Fatalf("engine override is missing CPU-topology setting %q", required)
		}
	}

	probeData, err := os.ReadFile("engine-capabilities.sh")
	if err != nil {
		t.Fatal(err)
	}
	probe := string(probeData)
	for _, required := range []string{
		`env:"FIRECRACKER_SMT"`,
		`env:"FIRECRACKER_EXCLUSIVE_CPU_TOPOLOGY"`,
		`env:"FIRECRACKER_CPUSET_CPUS"`,
		"is required when FIRECRACKER_EXCLUSIVE_CPU_TOPOLOGY=true",
		"cpuset.cpus.partition",
		"cpuset.cpus.exclusive.effective",
		"no NUMA node has %d distinct physical cores",
		"Firecracker vCPU thread %d was not present after VM start",
		"stabilizeExclusiveCPUPlacement",
		"Firecracker is outside the qualified exclusive CPU cgroup",
		"validateEffectiveSandboxCPUSet(cgroupPath, placement.reservedCPUs, expectedMems)",
		`return fmt.Errorf("Firecracker sandbox %s drifted", check.name)`,
		"exclusive CPU placement requires sandbox cgroup creation",
		"exclusive CPU placement requires complete startup resource reclamation",
		"monitorExclusiveCPUPlacement",
		"reconcile exclusive Firecracker CPU topology",
		`"exclusive_placement":"qualified-opt-in-disabled-by-default"`,
		`"isolation":"cgroup-v2-isolated-partition-plus-thread-affinity"`,
	} {
		if !strings.Contains(probe, required) {
			t.Fatalf("engine capability probe is missing CPU-topology contract %q", required)
		}
	}

	supplyChainData, err := os.ReadFile("artifact-supply-chain.sh")
	if err != nil {
		t.Fatal(err)
	}
	supplyChain := string(supplyChainData)
	for _, required := range []string{
		"artifact.orchestrator.cpu_topology_patch_sha256",
		"artifact.orchestrator.guest_smt=operator-configured-default-disabled",
		"artifact.orchestrator.exclusive_cpu_topology=disabled-by-default",
		"artifact.orchestrator.cpuset_qualification_patch_sha256",
		"artifact.orchestrator.exclusive_cpu_isolation=qualified-only-when-explicitly-configured",
		"the CPU-topology patch requires the local-capacity patch identity",
		"the cpuset-qualification patch requires the NBD-multiqueue patch identity",
	} {
		if !strings.Contains(supplyChain, required) {
			t.Fatalf("distribution manifest writer is missing CPU-topology identity %q", required)
		}
	}
}

func TestInstallerPinsAndValidatesRootfsReadPatch(t *testing.T) {
	installerData, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerData)
	for _, required := range []string{
		"0011-reduce-rootfs-read-amplification.patch",
		"orchestrator_rootfs_read_patch_sha256",
		`patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_ROOTFS_READ_PATCH"`,
		"engine rootfs-read patch verification failed",
		`"$ENGINE_CPU_TOPOLOGY_PATCH_SHA256" "$ENGINE_ROOTFS_READ_PATCH_SHA256" "$ENGINE_NBD_MULTIQUEUE_PATCH_SHA256"`,
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("installer is missing rootfs-read invariant %q", required)
		}
	}

	probeData, err := os.ReadFile("engine-capabilities.sh")
	if err != nil {
		t.Fatal(err)
	}
	probe := string(probeData)
	for _, required := range []string{
		"the allocation-free local rootfs read path",
		"the whole writable-range rootfs read path",
		"TestOverlayReadAtMixedWritableAndBaseBlocks",
		`"whole_writable_range":"single-cache-read"`,
	} {
		if !strings.Contains(probe, required) {
			t.Fatalf("engine capability probe is missing rootfs-read contract %q", required)
		}
	}

	supplyChainData, err := os.ReadFile("artifact-supply-chain.sh")
	if err != nil {
		t.Fatal(err)
	}
	supplyChain := string(supplyChainData)
	for _, required := range []string{
		"artifact.orchestrator.rootfs_read_patch_sha256",
		"artifact.orchestrator.rootfs_read_path=allocation-free-local-and-whole-writable-range",
		"the rootfs-read patch requires the CPU-topology patch identity",
	} {
		if !strings.Contains(supplyChain, required) {
			t.Fatalf("distribution manifest writer is missing rootfs-read identity %q", required)
		}
	}
}

func TestInstallerPinsAndValidatesNBDMultiqueueLifecyclePatch(t *testing.T) {
	installerData, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerData)
	for _, required := range []string{
		"0012-harden-nbd-multiqueue-lifecycle.patch",
		"orchestrator_nbd_multiqueue_patch_sha256",
		`patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_NBD_MULTIQUEUE_PATCH"`,
		"engine NBD multiqueue patch verification failed",
		`"$ENGINE_ROOTFS_READ_PATCH_SHA256" "$ENGINE_NBD_MULTIQUEUE_PATCH_SHA256"`,
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("installer is missing NBD multiqueue invariant %q", required)
		}
	}

	overrideData, err := os.ReadFile("engine.override.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(overrideData), "NBD_CONNECTIONS_PER_DEVICE: ${BREZEL_ENGINE_NBD_CONNECTIONS_PER_DEVICE:-1}") {
		t.Fatal("engine override does not pass the bounded NBD queue count")
	}

	qualificationData, err := os.ReadFile("qualify.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(qualificationData), `"$ENGINE_NBD_CONNECTIONS_PER_DEVICE"`) {
		t.Fatal("qualification does not pass the expected NBD queue count to the live probe")
	}

	probeData, err := os.ReadFile("engine-capabilities.sh")
	if err != nil {
		t.Fatal(err)
	}
	probe := string(probeData)
	for _, required := range []string{
		`env:"NBD_CONNECTIONS_PER_DEVICE"`,
		"NBD_CONNECTIONS_PER_DEVICE must be between 1 and 4",
		"TestDirectPathMountConnectRetryFullyCleansPreviousAttempt",
		"TestDirectPathMountCloseIsSerializedAndIdempotent",
		`"values_above_one":"pending-kvm-ab-qualification"`,
	} {
		if !strings.Contains(probe, required) {
			t.Fatalf("engine capability probe is missing NBD lifecycle contract %q", required)
		}
	}

	supplyChainData, err := os.ReadFile("artifact-supply-chain.sh")
	if err != nil {
		t.Fatal(err)
	}
	supplyChain := string(supplyChainData)
	for _, required := range []string{
		"artifact.orchestrator.nbd_multiqueue_patch_sha256",
		"artifact.orchestrator.nbd_connections_per_device=operator-configured-default-one-range-one-to-four",
		"artifact.orchestrator.nbd_lifecycle=attempt-owned-idempotent-cleanup",
		"the NBD-multiqueue patch requires the rootfs-read patch identity",
	} {
		if !strings.Contains(supplyChain, required) {
			t.Fatalf("distribution manifest writer is missing NBD lifecycle identity %q", required)
		}
	}
}

func TestRejectedRootfsCoalescingPatchIsNotShipped(t *testing.T) {
	for _, name := range []string{"install.sh", "engine-capabilities.sh", "artifact-supply-chain.sh", "engine.lock"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, rejected := range []string{
			"0013-coalesce-rootfs-lower-layer-reads.patch",
			"orchestrator_rootfs_coalescing_patch_sha256",
			"homogeneous_lower_range",
		} {
			if strings.Contains(string(data), rejected) {
				t.Fatalf("%s still contains rejected rootfs coalescing identity %q", name, rejected)
			}
		}
	}
	patchPath := filepath.Join("..", "..", "third_party", "e2b-runtime", "patches", "0013-coalesce-rootfs-lower-layer-reads.patch")
	if _, err := os.Stat(patchPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected rootfs coalescing patch remains shipped: %v", err)
	}
}

func TestInstallerPinsBuildsAndAttestsEnvdProcessTagPatch(t *testing.T) {
	installerData, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerData)
	for _, required := range []string{
		"0008-fix-envd-process-tag-resolution.patch",
		"0009-add-bounded-process-output-replay.patch",
		"envd_process_tag_patch_sha256",
		"envd_process_replay_patch_sha256",
		`patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_ENVD_PROCESS_TAG_PATCH"`,
		`patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_ENVD_PROCESS_REPLAY_PATCH"`,
		`BREZEL_ENGINE_ENVD_IMAGE="brezel/engine-envd:`,
		`-f "$SCRIPT_DIR/envd.Dockerfile"`,
		`BREZEL_ENGINE_ENVD_BINARY="$INSTALL_DIR/artifacts/envd"`,
		`BREZEL_ENGINE_ENVD_SHA256=$(sha256sum "$BREZEL_ENGINE_ENVD_BINARY"`,
		`run --rm --no-deps brezel-envd-install`,
		`"$ENGINE_CPU_TOPOLOGY_PATCH_SHA256" "$ENGINE_ROOTFS_READ_PATCH_SHA256" "$ENGINE_NBD_MULTIQUEUE_PATCH_SHA256"`,
		`"$BREZEL_ENGINE_ENVD_SHA256" "$ENGINE_ENVD_PROCESS_TAG_PATCH_SHA256"`,
		`"$ENGINE_ENVD_PROCESS_REPLAY_PATCH_SHA256"`,
		"engine envd process-tag patch verification failed",
		"engine envd process-replay patch verification failed",
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("installer is missing envd process-tag invariant %q", required)
		}
	}

	dockerfileData, err := os.ReadFile("envd.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(dockerfileData)
	for _, required := range []string{
		"ARG GOLANG_VERSION=1.26.8",
		"make build BUILD_ARCH=${TARGETARCH} BUILD=${COMMIT_SHA} LINK_VERSION=${VERSION}",
		"COPY --from=builder /build/envd/bin/envd /envd",
	} {
		if !strings.Contains(dockerfile, required) {
			t.Fatalf("envd build is missing pinned-source invariant %q", required)
		}
	}

	overrideData, err := os.ReadFile("engine.override.yaml")
	if err != nil {
		t.Fatal(err)
	}
	override := string(overrideData)
	for _, required := range []string{
		"brezel-envd-install:",
		"BREZEL_ENGINE_ENVD_SHA256:",
		"BREZEL_ENGINE_ENVD_BINARY:",
		"target_dir=/host/fc-envd",
		`mv -f -- "$$temporary" "$$target_dir/envd"`,
	} {
		if !strings.Contains(override, required) {
			t.Fatalf("engine override is missing envd installation invariant %q", required)
		}
	}

	supplyChainData, err := os.ReadFile("artifact-supply-chain.sh")
	if err != nil {
		t.Fatal(err)
	}
	supplyChain := string(supplyChainData)
	for _, required := range []string{
		"artifact.envd.upstream_sha256",
		"artifact.envd.process_tag_patch_sha256",
		"artifact.envd.live_tag_resolution=complete-map-scan",
		"artifact.envd.process_replay_patch_sha256",
		"artifact.envd.process_output_recovery=generation-bound-cursor-journal",
		"the envd override requires the process-tag patch identity",
		"the envd override requires the process-replay patch identity",
		"the process-tag patch requires the envd override identity",
		"the process-replay patch requires the process-tag patch identity",
	} {
		if !strings.Contains(supplyChain, required) {
			t.Fatalf("distribution manifest writer is missing envd identity %q", required)
		}
	}

	probeData, err := os.ReadFile("engine-capabilities.sh")
	if err != nil {
		t.Fatal(err)
	}
	probe := string(probeData)
	for _, required := range []string{
		"if value.Tag == nil || *value.Tag != tag {",
		"TestGetProcessByTagScansPastNonMatches",
		"require.Same(t, target, got)",
		`"live_tag_resolution":"complete-map-scan"`,
		`replayVersionHeader = "E2b-Process-Replay-Version"`,
		"TestConnect_ReplayFailsExplicitlyAfterEviction",
		"TestEventJournalAtomicReplayToWaitHandoff",
		`"protocol":"generation-bound-cursor-journal"`,
	} {
		if !strings.Contains(probe, required) {
			t.Fatalf("engine capability probe is missing envd process-tag contract %q", required)
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
		`patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_CACHE_PATCH"`,
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
		"/proc/sys/kernel/random/boot_id", "host boot identity did not change", "host_boot_identity_changed",
		"--reset-method", "BREZEL_HOST_REBOOT_RESET_METHOD", "fault_injection", "reset_method",
		"runtime-attestation.manifest", "runtime_identity_unchanged", "runtime_identity_changed",
		"host_reboot_workspace_recovery_not_conformant", "durable_marker_changed",
		"sandbox_cleanup_failed", "workspace_cleanup_failed", "schema_version:3",
		"crash_consistency_manifest", "crash_consistency_corpus", "crash_consistency_corpus_missing",
		"crash_consistency_corpus_changed", "atomic-rename.pending", "atomic-rename.txt", "nested/path.txt",
		"overwrite.txt", "payload-1m.bin", "small.txt", "truncate.txt", "bs=1048576", "truncate -s 17",
		`--env "BREZEL_CORPUS_MARKER=$corpus_marker"`, "/bin/sh -c", "$BREZEL_CORPUS_MARKER",
		"jq -Rsce", `sync -f "$PENDING_FILE"`, `sync -f "$REPORT_PATH"`, `sync -f "$INSTALL_DIR/qualification"`,
		"$before == $after",
	} {
		if !strings.Contains(drill, required) {
			t.Fatalf("host reboot drill is missing %q", required)
		}
	}
	if strings.Contains(drill, "*) cli sandbox delete") {
		t.Fatal("host reboot drill must not delete a sandbox in an unknown lifecycle state")
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
