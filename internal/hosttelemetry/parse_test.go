package hosttelemetry

import (
	"math"
	"testing"
)

func TestParsersKeepOnlyFixedResourceCounters(t *testing.T) {
	previous, err := parseProcStat([]byte("cpu 100 0 50 800 20 1 2 3\ncpu0 1 0 1 8 0 0 0 0\ncpu1 1 0 1 8 0 0 0 0\nctxt 400\nprocesses 20\nprocs_running 2\nprocs_blocked 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	current, err := parseProcStat([]byte("cpu 140 0 70 830 30 1 2 8\ncpu0 1 0 1 8 0 0 0 0\ncpu1 1 0 1 8 0 0 0 0\nctxt 500\nprocesses 25\nprocs_running 3\nprocs_blocked 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	cpu := cpuDelta(previous, current)
	if cpu.LogicalCPUs != 2 || cpu.DeltaTotal != 105 || cpu.DeltaBusy != 65 || cpu.DeltaIOWait != 10 || cpu.DeltaSteal != 5 || cpu.ContextSwitch != 500 {
		t.Fatalf("cpu=%#v", cpu)
	}
	if math.Abs(cpu.IOWaitPercent-9.5238095238) > 0.0001 || math.Abs(cpu.StealPercent-4.7619047619) > 0.0001 {
		t.Fatalf("cpu percentages=%#v", cpu)
	}

	psi, err := parsePSI([]byte("some avg10=0.12 avg60=0.34 avg300=0.56 total=789\nfull avg10=0.01 avg60=0.02 avg300=0.03 total=45\n"))
	if err != nil || !psi.Available || !psi.Some.Present || psi.Some.TotalUS != 789 || psi.Full.Avg60 != 0.02 {
		t.Fatalf("psi=%#v err=%v", psi, err)
	}

	memory := parseMeminfo([]byte("MemTotal: 1000 kB\nMemAvailable: 600 kB\nSwapTotal: 50 kB\nSwapFree: 40 kB\nDirty: 3 kB\nWriteback: 2 kB\nAnonPages: 100 kB\nSlab: 80 kB\nSReclaimable: 30 kB\nSecretLabel: 999 kB\n"))
	if memory.MemTotalKB != 1000 || memory.MemAvailableKB != 600 || memory.DirtyKB != 3 || memory.SReclaimableKB != 30 {
		t.Fatalf("memory=%#v", memory)
	}
	vm := parseVMStat([]byte("pgfault 100\npgmajfault 4\npswpin 2\npswpout 3\noom_kill 1\nallocstall 11\nallocstall_dma 2\nallocstall_normal 3\ncompact_stall 4\npgscan_direct 5\npgscan_kswapd 6\npgsteal_direct 7\npgsteal_kswapd 8\ncustomer_secret 999\n"))
	if vm.OOMKills != 1 || vm.AllocStalls != 11 || vm.DirectPageScans != 5 || vm.KswapdPageSteals != 8 {
		t.Fatalf("vm=%#v", vm)
	}
	vm = parseVMStat([]byte("allocstall_dma 2\nallocstall_normal 3\n"))
	if vm.AllocStalls != 5 {
		t.Fatalf("zoned alloc stalls=%#v", vm)
	}
}

func TestDiskNetworkAndCgroupParsers(t *testing.T) {
	disks, err := parseDiskstats([]byte(" 259 0 nvme0n1 10 2 100 3 20 4 200 5 1 6 7 8 9 10 11 12 13\n 43 0 nbd0 30 0 300 9 40 0 400 10 2 11 12\n"))
	if err != nil || len(disks) != 2 || disks[0].Name != "nvme0n1" || disks[0].DiscardsCompleted != 8 || disks[0].FlushesCompleted != 12 || disks[1].IOInProgress != 2 {
		t.Fatalf("disks=%#v err=%v", disks, err)
	}
	interfaces, err := parseNetDev([]byte("Inter-| Receive | Transmit\n eth0: 100 2 3 4 0 0 0 0 200 5 6 7 0 0 0 0\n lo: 300 8 0 0 0 0 0 0 300 8 0 0 0 0 0 0\n"))
	if err != nil || len(interfaces) != 2 || interfaces[0].RXDropped != 4 || interfaces[0].TXBytes != 200 || interfaces[0].TXDropped != 7 {
		t.Fatalf("interfaces=%#v err=%v", interfaces, err)
	}
	tcp, err := parseSNMPTCP([]byte("Tcp: ActiveOpens PassiveOpens AttemptFails EstabResets CurrEstab InSegs OutSegs RetransSegs InErrs OutRsts\nTcp: 1 2 3 4 5 6 7 8 9 10\n"))
	if err != nil || tcp.CurrentEstab != 5 || tcp.Retransmits != 8 || tcp.OutResets != 10 {
		t.Fatalf("tcp=%#v", tcp)
	}
	sockets, err := parseSockstat([]byte("sockets: used 10\nTCP: inuse 2 orphan 3 tw 4 alloc 5 mem 6\n"))
	if err != nil || sockets.TCPInUse != 2 || sockets.TCPMemoryPages != 6 {
		t.Fatalf("sockets=%#v", sockets)
	}

	cgroup := parseCgroup(map[string][]byte{
		"cpu.stat":            []byte("usage_usec 100\nuser_usec 60\nsystem_usec 40\nnr_throttled 3\nthrottled_usec 20\n"),
		"memory.current":      []byte("1000\n"),
		"memory.peak":         []byte("1200\n"),
		"memory.swap.current": []byte("30\n"),
		"memory.events":       []byte("low 1\nhigh 2\nmax 3\noom 4\noom_kill 5\n"),
		"pids.current":        []byte("8\n"),
		"io.stat":             []byte("259:0 rbytes=10 wbytes=20 rios=2 wios=3\n43:0 rbytes=4 wbytes=5 rios=1 wios=1\n"),
		"cpu.pressure":        []byte("some avg10=0 avg60=0 avg300=0 total=10\n"),
	})
	if !cgroup.Available || cgroup.CPUUsageUS != 100 || cgroup.CPUThrottledPeriods != 3 || cgroup.MemoryOOMKills != 5 || cgroup.IOReadBytes != 14 || cgroup.IOWriteOps != 4 || cgroup.Pressure.CPU.Some.TotalUS != 10 {
		t.Fatalf("cgroup=%#v", cgroup)
	}
}

func TestProcessParserHandlesParenthesesWithoutReadingContent(t *testing.T) {
	process, err := parseProcessStat([]byte("123 (brezel-node) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22\n"))
	if err != nil {
		t.Fatal(err)
	}
	if process.PID != 123 || process.Name != "brezel-node" || process.MinorFaults != 7 || process.MajorFaults != 9 || process.UserTicks != 11 || process.SystemTicks != 12 || process.Threads != 17 || process.StartTicks != 19 || process.VirtualBytes != 20 || process.ResidentPages != 21 {
		t.Fatalf("process=%#v", process)
	}
	applyProcessIO(&process, []byte("rchar: 999\nwchar: 998\nsyscr: 10\nsyscw: 20\nread_bytes: 30\nwrite_bytes: 40\ncancelled_write_bytes: 5\n"))
	if process.ReadSyscalls != 10 || process.WriteSyscalls != 20 || process.ReadBytes != 30 || process.WriteBytes != 40 || process.CancelledWriteBytes != 5 {
		t.Fatalf("process io=%#v", process)
	}
	if _, err := parseProcessStat([]byte("123 (customer command) S 1 2 3")); err == nil {
		t.Fatal("unsafe process name was accepted")
	}
}

func TestCgroupPathRejectsTraversal(t *testing.T) {
	if path, ok := parseCgroupPath([]byte("0::/system.slice/brezel.service\n")); !ok || path != "/system.slice/brezel.service" {
		t.Fatalf("path=%q ok=%v", path, ok)
	}
	if _, ok := parseCgroupPath([]byte("0::/../../private\n")); ok {
		t.Fatal("traversal cgroup path was accepted")
	}
}

func TestParsersRejectMalformedKernelCounters(t *testing.T) {
	if _, err := parsePSI([]byte("some avg10=NaN avg60=0 avg300=0 total=1\n")); err == nil {
		t.Fatal("non-finite pressure value was accepted")
	}
	if _, err := parseSNMPTCP([]byte("Tcp: ActiveOpens RetransSegs\nTcp: 1 invalid\n")); err == nil {
		t.Fatal("invalid TCP counter was accepted")
	}
	if _, err := parseSockstat([]byte("TCP: inuse invalid\n")); err == nil {
		t.Fatal("invalid socket counter was accepted")
	}
}
