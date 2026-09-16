package hosttelemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCollectorCapturesBoundedHostCountersAndToleratesProcessChurn(t *testing.T) {
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	sysRoot := filepath.Join(root, "sys")
	cgroupRoot := filepath.Join(root, "cgroup")
	writeHostFixture(t, procRoot, sysRoot, cgroupRoot)

	config := DefaultConfig()
	config.OutputDir = filepath.Join(root, "unused-output")
	config.ProcRoot, config.SysRoot, config.CgroupRoot = procRoot, sysRoot, cgroupRoot
	config.MaxBlockDevices, config.MaxInterfaces, config.MaxProcesses = 1, 1, 1
	configured, err := newCollector(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := configured.baselineCPU(); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(procRoot, "stat"), "cpu 140 0 70 830 30 1 2 8\ncpu0 1 0 1 8 0 0 0 0\ncpu1 1 0 1 8 0 0 0 0\nctxt 500\nprocesses 25\nprocs_running 3\nprocs_blocked 2\n")
	sample := configured.collect(7, time.Now().Add(-time.Second))

	if sample.Sequence != 7 || sample.ElapsedNS <= 0 || sample.CollectionNS <= 0 || sample.CPU.DeltaTotal != 105 || sample.CPU.DeltaSteal != 5 {
		t.Fatalf("sample timing/cpu=%#v", sample)
	}
	if !sample.Pressure.CPU.Available || sample.Memory.MemAvailableKB != 600 || sample.VM.OOMKills != 1 {
		t.Fatalf("sample pressure/memory=%#v", sample)
	}
	if len(sample.BlockDevices) != 1 || sample.BlockDevices[0].Name != "nbd0" || !sample.BlockDevicesCapped {
		t.Fatalf("block devices=%#v capped=%v", sample.BlockDevices, sample.BlockDevicesCapped)
	}
	if len(sample.Network.Interfaces) != 1 || sample.Network.Interfaces[0].Name != "eth0" || !sample.InterfacesCapped || sample.Network.TCP.Retransmits != 8 {
		t.Fatalf("network=%#v capped=%v", sample.Network, sample.InterfacesCapped)
	}
	if !sample.HostCgroup.Available || sample.HostCgroup.MemoryOOMKills != 5 || len(sample.Processes) != 1 || sample.Processes[0].Name != "brezeld" || !sample.Processes[0].Cgroup.Available || !sample.ProcessesCapped {
		t.Fatalf("cgroup/processes host=%#v processes=%#v", sample.HostCgroup, sample.Processes)
	}
	if sample.ReadErrors.Processes != 0 {
		t.Fatalf("process churn was reported as a hard read error: %#v", sample.ReadErrors)
	}
	encoded, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"customer-secret", "private-agent", "argv", "command_line", "environment", procRoot, cgroupRoot} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("forbidden content %q reached sample: %s", forbidden, encoded)
		}
	}
}

func writeHostFixture(t *testing.T, procRoot, sysRoot, cgroupRoot string) {
	t.Helper()
	writeFixture(t, filepath.Join(procRoot, "stat"), "cpu 100 0 50 800 20 1 2 3\ncpu0 1 0 1 8 0 0 0 0\ncpu1 1 0 1 8 0 0 0 0\nctxt 400\nprocesses 20\nprocs_running 2\nprocs_blocked 1\n")
	for _, resource := range []string{"cpu", "memory", "io"} {
		writeFixture(t, filepath.Join(procRoot, "pressure", resource), "some avg10=0.1 avg60=0.2 avg300=0.3 total=10\nfull avg10=0 avg60=0 avg300=0 total=0\n")
	}
	writeFixture(t, filepath.Join(procRoot, "meminfo"), "MemTotal: 1000 kB\nMemAvailable: 600 kB\nSwapTotal: 50 kB\nSwapFree: 40 kB\nDirty: 3 kB\nWriteback: 2 kB\nAnonPages: 100 kB\nSlab: 80 kB\nSReclaimable: 30 kB\n")
	writeFixture(t, filepath.Join(procRoot, "vmstat"), "pgfault 100\npgmajfault 4\npswpin 2\npswpout 3\noom_kill 1\nallocstall_normal 3\ncompact_stall 4\npgscan_direct 5\npgscan_kswapd 6\npgsteal_direct 7\npgsteal_kswapd 8\n")
	writeFixture(t, filepath.Join(procRoot, "diskstats"), "259 0 nvme0n1 10 2 100 3 20 4 200 5 1 6 7\n43 0 nbd0 30 0 300 9 40 0 400 10 2 11 12\n259 1 nvme0n1p1 5 0 50 1 6 0 60 2 0 3 4\n")
	if err := os.MkdirAll(filepath.Join(sysRoot, "class", "block", "nvme0n1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sysRoot, "class", "block", "nbd0"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(sysRoot, "class", "block", "nvme0n1p1", "partition"), "1\n")
	writeFixture(t, filepath.Join(procRoot, "net", "dev"), "Inter-| Receive | Transmit\n eth0: 100 2 3 4 0 0 0 0 200 5 6 7 0 0 0 0\n lo: 300 8 0 0 0 0 0 0 300 8 0 0 0 0 0 0\n")
	writeFixture(t, filepath.Join(procRoot, "net", "snmp"), "Tcp: ActiveOpens PassiveOpens AttemptFails EstabResets CurrEstab InSegs OutSegs RetransSegs InErrs OutRsts\nTcp: 1 2 3 4 5 6 7 8 9 10\n")
	writeFixture(t, filepath.Join(procRoot, "net", "sockstat"), "TCP: inuse 2 orphan 3 tw 4 alloc 5 mem 6\n")

	writeCgroupFixture(t, cgroupRoot)
	writeCgroupFixture(t, filepath.Join(cgroupRoot, "brezel.slice"))
	writeFixture(t, filepath.Join(procRoot, "101", "comm"), "brezeld\n")
	writeFixture(t, filepath.Join(procRoot, "101", "stat"), "101 (brezeld) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22\n")
	writeFixture(t, filepath.Join(procRoot, "101", "io"), "syscr: 10\nsyscw: 20\nread_bytes: 30\nwrite_bytes: 40\ncancelled_write_bytes: 5\n")
	writeFixture(t, filepath.Join(procRoot, "101", "cgroup"), "0::/brezel.slice\n")
	writeFixture(t, filepath.Join(procRoot, "102", "comm"), "private-agent\n")
	writeFixture(t, filepath.Join(procRoot, "103", "comm"), "brezeld\n")
	writeFixture(t, filepath.Join(procRoot, "104", "comm"), "brezel-node\n")
	writeFixture(t, filepath.Join(procRoot, "104", "stat"), "104 (brezel-node) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22\n")
}

func writeCgroupFixture(t *testing.T, root string) {
	t.Helper()
	writeFixture(t, filepath.Join(root, "cgroup.controllers"), "cpu io memory pids\n")
	writeFixture(t, filepath.Join(root, "cpu.stat"), "usage_usec 100\nuser_usec 60\nsystem_usec 40\nnr_throttled 3\nthrottled_usec 20\n")
	writeFixture(t, filepath.Join(root, "memory.current"), "1000\n")
	writeFixture(t, filepath.Join(root, "memory.peak"), "1200\n")
	writeFixture(t, filepath.Join(root, "memory.swap.current"), "30\n")
	writeFixture(t, filepath.Join(root, "memory.events"), "low 1\nhigh 2\nmax 3\noom 4\noom_kill 5\n")
	writeFixture(t, filepath.Join(root, "pids.current"), "8\n")
	writeFixture(t, filepath.Join(root, "io.stat"), "259:0 rbytes=10 wbytes=20 rios=2 wios=3\n")
	for _, resource := range []string{"cpu", "memory", "io"} {
		writeFixture(t, filepath.Join(root, resource+".pressure"), "some avg10=0 avg60=0 avg300=0 total=10\n")
	}
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
