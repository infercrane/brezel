package hosttelemetry

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

type cpuCounters struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
	logicalCPUs                                           int
	contextSwitches, processForks, running, blocked       uint64
}

func parseProcStat(data []byte) (cpuCounters, error) {
	var result cpuCounters
	found := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "cpu":
			if len(fields) < 9 {
				return result, errors.New("aggregate CPU row is incomplete")
			}
			values, err := parseUintFields(fields[1:9])
			if err != nil {
				return result, err
			}
			result.user, result.nice, result.system, result.idle = values[0], values[1], values[2], values[3]
			result.iowait, result.irq, result.softirq, result.steal = values[4], values[5], values[6], values[7]
			found = true
		case "ctxt":
			result.contextSwitches = parseOptionalUint(fields)
		case "processes":
			result.processForks = parseOptionalUint(fields)
		case "procs_running":
			result.running = parseOptionalUint(fields)
		case "procs_blocked":
			result.blocked = parseOptionalUint(fields)
		default:
			if strings.HasPrefix(fields[0], "cpu") && len(fields[0]) > 3 {
				if _, err := strconv.ParseUint(fields[0][3:], 10, 32); err == nil {
					result.logicalCPUs++
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return result, err
	}
	if !found {
		return result, errors.New("aggregate CPU row is missing")
	}
	return result, nil
}

func cpuDelta(previous, current cpuCounters) CPU {
	totalBefore := previous.user + previous.nice + previous.system + previous.idle + previous.iowait + previous.irq + previous.softirq + previous.steal
	totalAfter := current.user + current.nice + current.system + current.idle + current.iowait + current.irq + current.softirq + current.steal
	total := monotonicDelta(totalBefore, totalAfter)
	idle := monotonicDelta(previous.idle, current.idle)
	iowait := monotonicDelta(previous.iowait, current.iowait)
	steal := monotonicDelta(previous.steal, current.steal)
	busy := uint64(0)
	if total > idle+iowait {
		busy = total - idle - iowait
	}
	result := CPU{
		LogicalCPUs: current.logicalCPUs, DeltaTotal: total, DeltaBusy: busy,
		DeltaIOWait: iowait, DeltaSteal: steal, ContextSwitch: current.contextSwitches,
		ProcessesForks: current.processForks, ProcsRunning: current.running, ProcsBlocked: current.blocked,
	}
	if total > 0 {
		result.BusyPercent = float64(busy) * 100 / float64(total)
		result.IOWaitPercent = float64(iowait) * 100 / float64(total)
		result.StealPercent = float64(steal) * 100 / float64(total)
	}
	return result
}

func parsePSI(data []byte) (PSIResource, error) {
	var result PSIResource
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		line := PSILine{Present: true}
		for _, field := range fields[1:] {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			switch key {
			case "avg10":
				line.Avg10, ok = parseFiniteFloat(value)
			case "avg60":
				line.Avg60, ok = parseFiniteFloat(value)
			case "avg300":
				line.Avg300, ok = parseFiniteFloat(value)
			case "total":
				parsed, parseErr := strconv.ParseUint(value, 10, 64)
				line.TotalUS, ok = parsed, parseErr == nil
			}
			if !ok {
				return result, errors.New("pressure value is invalid")
			}
		}
		switch fields[0] {
		case "some":
			result.Some = line
		case "full":
			result.Full = line
		}
	}
	if err := scanner.Err(); err != nil {
		return result, err
	}
	if !result.Some.Present && !result.Full.Present {
		return result, errors.New("pressure rows are missing")
	}
	result.Available = true
	return result, nil
}

func parseMeminfo(data []byte) Memory {
	values := parseColonUintMap(data)
	return Memory{
		MemTotalKB: values["MemTotal"], MemAvailableKB: values["MemAvailable"],
		SwapTotalKB: values["SwapTotal"], SwapFreeKB: values["SwapFree"],
		DirtyKB: values["Dirty"], WritebackKB: values["Writeback"],
		AnonPagesKB: values["AnonPages"], SlabKB: values["Slab"], SReclaimableKB: values["SReclaimable"],
	}
}

func parseVMStat(data []byte) VM {
	values := parseSpaceUintMap(data)
	allocStalls, hasAggregate := values["allocstall"]
	if !hasAggregate {
		for key, value := range values {
			if strings.HasPrefix(key, "allocstall_") {
				allocStalls += value
			}
		}
	}
	return VM{
		PageFaults: values["pgfault"], MajorPageFaults: values["pgmajfault"],
		SwapInPages: values["pswpin"], SwapOutPages: values["pswpout"], OOMKills: values["oom_kill"],
		AllocStalls: allocStalls, CompactStalls: values["compact_stall"],
		DirectPageScans: values["pgscan_direct"], KswapdPageScans: values["pgscan_kswapd"],
		DirectPageSteals: values["pgsteal_direct"], KswapdPageSteals: values["pgsteal_kswapd"],
	}
}

func parseDiskstats(data []byte) ([]BlockDevice, error) {
	var result []BlockDevice
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 14 || !safeKernelName(fields[2]) {
			continue
		}
		values, err := parseUintFields(fields[3:14])
		if err != nil {
			continue
		}
		device := BlockDevice{
			Name: fields[2], ReadsCompleted: values[0], ReadsMerged: values[1], SectorsRead: values[2], ReadTimeMS: values[3],
			WritesCompleted: values[4], WritesMerged: values[5], SectorsWritten: values[6], WriteTimeMS: values[7],
			IOInProgress: values[8], IOTimeMS: values[9], WeightedIOTimeMS: values[10],
		}
		if len(fields) >= 18 {
			device.DiscardsCompleted, _ = strconv.ParseUint(fields[14], 10, 64)
			device.DiscardSectors, _ = strconv.ParseUint(fields[16], 10, 64)
		}
		if len(fields) >= 20 {
			device.FlushesCompleted, _ = strconv.ParseUint(fields[18], 10, 64)
		}
		result = append(result, device)
	}
	return result, scanner.Err()
}

func parseNetDev(data []byte) ([]NetworkInterface, error) {
	var result []NetworkInterface
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		namePart, countersPart, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		name := strings.TrimSpace(namePart)
		fields := strings.Fields(countersPart)
		if !safeKernelName(name) || len(fields) < 16 {
			continue
		}
		values, err := parseUintFields(fields[:16])
		if err != nil {
			continue
		}
		result = append(result, NetworkInterface{
			Name: name, RXBytes: values[0], RXPackets: values[1], RXErrors: values[2], RXDropped: values[3],
			TXBytes: values[8], TXPackets: values[9], TXErrors: values[10], TXDropped: values[11],
		})
	}
	return result, scanner.Err()
}

func parseSNMPTCP(data []byte) (TCP, error) {
	lines := strings.Split(string(data), "\n")
	for index := 0; index+1 < len(lines); index++ {
		head := strings.Fields(lines[index])
		values := strings.Fields(lines[index+1])
		if len(head) < 2 || len(head) != len(values) || head[0] != "Tcp:" || values[0] != "Tcp:" {
			continue
		}
		mapped := make(map[string]uint64, len(head)-1)
		for fieldIndex := 1; fieldIndex < len(head); fieldIndex++ {
			value, err := strconv.ParseUint(values[fieldIndex], 10, 64)
			if err != nil {
				return TCP{}, errors.New("TCP counter is invalid")
			}
			mapped[head[fieldIndex]] = value
		}
		return TCP{
			ActiveOpens: mapped["ActiveOpens"], PassiveOpens: mapped["PassiveOpens"], AttemptFails: mapped["AttemptFails"],
			EstabResets: mapped["EstabResets"], CurrentEstab: mapped["CurrEstab"], InSegments: mapped["InSegs"],
			OutSegments: mapped["OutSegs"], Retransmits: mapped["RetransSegs"], InErrors: mapped["InErrs"], OutResets: mapped["OutRsts"],
		}, nil
	}
	return TCP{}, errors.New("TCP counters are missing")
}

func parseSockstat(data []byte) (SocketStats, error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "TCP:" {
			continue
		}
		values := make(map[string]uint64)
		for index := 1; index+1 < len(fields); index += 2 {
			value, err := strconv.ParseUint(fields[index+1], 10, 64)
			if err != nil {
				return SocketStats{}, errors.New("socket counter is invalid")
			}
			values[fields[index]] = value
		}
		return SocketStats{TCPInUse: values["inuse"], TCPOrphan: values["orphan"], TCPTimeWait: values["tw"], TCPAllocated: values["alloc"], TCPMemoryPages: values["mem"]}, nil
	}
	return SocketStats{}, errors.New("TCP socket counters are missing")
}

func parseProcessStat(data []byte) (Process, error) {
	text := strings.TrimSpace(string(data))
	closeIndex := strings.LastIndex(text, ")")
	openIndex := strings.Index(text, "(")
	if openIndex < 1 || closeIndex <= openIndex || closeIndex+1 >= len(text) {
		return Process{}, errors.New("process stat is malformed")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(text[:openIndex]))
	if err != nil || pid <= 0 {
		return Process{}, errors.New("process pid is invalid")
	}
	name := text[openIndex+1 : closeIndex]
	if !safeProcessName(name) {
		return Process{}, errors.New("process name is unsafe")
	}
	fields := strings.Fields(text[closeIndex+1:])
	if len(fields) < 22 {
		return Process{}, errors.New("process stat is incomplete")
	}
	unsignedIndexes := []int{7, 9, 11, 12, 17, 19, 20}
	values := make([]uint64, len(unsignedIndexes))
	for index, fieldIndex := range unsignedIndexes {
		values[index], err = strconv.ParseUint(fields[fieldIndex], 10, 64)
		if err != nil {
			return Process{}, fmt.Errorf("process stat field %d is invalid", fieldIndex)
		}
	}
	rss, err := strconv.ParseInt(fields[21], 10, 64)
	if err != nil {
		return Process{}, errors.New("process resident pages are invalid")
	}
	return Process{
		PID: pid, Name: name, MinorFaults: values[0], MajorFaults: values[1], UserTicks: values[2], SystemTicks: values[3],
		Threads: values[4], StartTicks: values[5], VirtualBytes: values[6], ResidentPages: rss,
	}, nil
}

func applyProcessIO(process *Process, data []byte) {
	values := parseColonUintMap(data)
	process.ReadSyscalls = values["syscr"]
	process.WriteSyscalls = values["syscw"]
	process.ReadBytes = values["read_bytes"]
	process.WriteBytes = values["write_bytes"]
	process.CancelledWriteBytes = values["cancelled_write_bytes"]
}

func parseCgroup(data map[string][]byte) Cgroup {
	cpu := parseSpaceUintMap(data["cpu.stat"])
	memoryEvents := parseSpaceUintMap(data["memory.events"])
	ioValues := make(map[string]uint64)
	for _, line := range strings.Split(string(data["io.stat"]), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		for _, field := range fields[1:] {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			parsed, _ := strconv.ParseUint(value, 10, 64)
			ioValues[key] += parsed
		}
	}
	result := Cgroup{
		Available: len(data) > 0, CPUUsageUS: cpu["usage_usec"], CPUUserUS: cpu["user_usec"], CPUSystemUS: cpu["system_usec"],
		CPUThrottledUS: cpu["throttled_usec"], CPUThrottledPeriods: cpu["nr_throttled"],
		MemoryCurrent: parseScalar(data["memory.current"]), MemoryPeak: parseScalar(data["memory.peak"]), SwapCurrent: parseScalar(data["memory.swap.current"]),
		MemoryLow: memoryEvents["low"], MemoryHigh: memoryEvents["high"], MemoryMax: memoryEvents["max"], MemoryOOM: memoryEvents["oom"], MemoryOOMKills: memoryEvents["oom_kill"],
		PIDsCurrent: parseScalar(data["pids.current"]), IOReadBytes: ioValues["rbytes"], IOWriteBytes: ioValues["wbytes"], IOReadOps: ioValues["rios"], IOWriteOps: ioValues["wios"],
	}
	result.Pressure.CPU, _ = parsePSI(data["cpu.pressure"])
	result.Pressure.Memory, _ = parsePSI(data["memory.pressure"])
	result.Pressure.IO, _ = parsePSI(data["io.pressure"])
	return result
}

func parseCgroupPath(data []byte) (string, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::/") || line == "0::/" {
			path := strings.TrimPrefix(line, "0::")
			if strings.ContainsRune(path, '\x00') || strings.Contains(path, "..") {
				return "", false
			}
			return path, true
		}
	}
	return "", false
}

func parseColonUintMap(data []byte) map[string]uint64 {
	result := make(map[string]uint64)
	for _, line := range strings.Split(string(data), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		result[strings.TrimSpace(key)], _ = strconv.ParseUint(fields[0], 10, 64)
	}
	return result
}

func parseSpaceUintMap(data []byte) map[string]uint64 {
	result := make(map[string]uint64)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		result[fields[0]], _ = strconv.ParseUint(fields[1], 10, 64)
	}
	return result
}

func parseScalar(data []byte) uint64 {
	value, _ := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	return value
}

func parseUintFields(fields []string) ([]uint64, error) {
	values := make([]uint64, len(fields))
	for index, field := range fields {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return nil, err
		}
		values[index] = value
	}
	return values, nil
}

func parseOptionalUint(fields []string) uint64 {
	if len(fields) < 2 {
		return 0
	}
	value, _ := strconv.ParseUint(fields[1], 10, 64)
	return value
}

func monotonicDelta(before, after uint64) uint64 {
	if after < before {
		return 0
	}
	return after - before
}

func parseFiniteFloat(value string) (float64, bool) {
	parsed, err := strconv.ParseFloat(value, 64)
	return parsed, err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0)
}

func safeKernelName(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, current := range value {
		if (current >= 'a' && current <= 'z') || (current >= 'A' && current <= 'Z') || (current >= '0' && current <= '9') || current == '_' || current == '-' || current == '.' || current == '@' {
			continue
		}
		return false
	}
	return true
}

func safeProcessName(value string) bool {
	return safeKernelName(value) && len(value) <= 32
}
