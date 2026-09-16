package hosttelemetry

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxKernelFileBytes = 4 << 20

var defaultProcessNames = []string{"brezeld", "brezel-node", "firecracker", "orchestrator"}

type Config struct {
	OutputDir       string
	Duration        time.Duration
	MaxBytes        int64
	ProcRoot        string
	SysRoot         string
	CgroupRoot      string
	ProcessNames    []string
	MaxProcesses    int
	MaxBlockDevices int
	MaxInterfaces   int
}

func DefaultConfig() Config {
	return Config{
		Duration: 30 * time.Minute, MaxBytes: 128 << 20,
		ProcRoot: "/proc", SysRoot: "/sys", CgroupRoot: "/sys/fs/cgroup",
		ProcessNames: append([]string(nil), defaultProcessNames...),
		MaxProcesses: 128, MaxBlockDevices: 64, MaxInterfaces: 64,
	}
}

func (c Config) validate() error {
	if strings.TrimSpace(c.OutputDir) == "" {
		return errors.New("output directory is required")
	}
	if c.Duration < SampleInterval || c.Duration > 24*time.Hour {
		return errors.New("duration must be between 1 second and 24 hours")
	}
	if c.MaxBytes < 1<<20 || c.MaxBytes > 4<<30 {
		return errors.New("max bytes must be between 1 MiB and 4 GiB")
	}
	if c.MaxProcesses < 1 || c.MaxProcesses > 1024 || c.MaxBlockDevices < 1 || c.MaxBlockDevices > 256 || c.MaxInterfaces < 1 || c.MaxInterfaces > 256 {
		return errors.New("collector cardinality bounds are invalid")
	}
	if len(c.ProcessNames) == 0 || len(c.ProcessNames) > 32 {
		return errors.New("between 1 and 32 process names are required")
	}
	for _, name := range c.ProcessNames {
		if !safeProcessName(name) {
			return errors.New("process names must be fixed safe kernel names")
		}
	}
	for _, root := range []string{c.ProcRoot, c.SysRoot, c.CgroupRoot} {
		if !filepath.IsAbs(root) {
			return errors.New("collector roots must be absolute")
		}
	}
	return nil
}

type collector struct {
	config       Config
	processNames map[string]struct{}
	previousCPU  cpuCounters
	haveCPU      bool
}

func newCollector(config Config) (*collector, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	names := make(map[string]struct{}, len(config.ProcessNames))
	for _, name := range config.ProcessNames {
		names[name] = struct{}{}
	}
	return &collector{config: config, processNames: names}, nil
}

func (c *collector) baselineCPU() error {
	data, err := readKernelFile(filepath.Join(c.config.ProcRoot, "stat"))
	if err != nil {
		return err
	}
	c.previousCPU, err = parseProcStat(data)
	c.haveCPU = err == nil
	return err
}

func (c *collector) collect(sequence uint64, started time.Time) Sample {
	collectionStarted := time.Now()
	now := time.Now()
	sample := Sample{SchemaVersion: SchemaVersion, Sequence: sequence, SampledAt: now.UTC().Format(time.RFC3339Nano), ElapsedNS: time.Since(started).Nanoseconds()}

	c.collectCPU(&sample)
	sample.Pressure.CPU = c.readPSI("cpu", &sample.ReadErrors.Pressure)
	sample.Pressure.Memory = c.readPSI("memory", &sample.ReadErrors.Pressure)
	sample.Pressure.IO = c.readPSI("io", &sample.ReadErrors.Pressure)
	if data, err := readKernelFile(filepath.Join(c.config.ProcRoot, "meminfo")); err == nil {
		sample.Memory = parseMeminfo(data)
	} else {
		sample.ReadErrors.Memory++
	}
	if data, err := readKernelFile(filepath.Join(c.config.ProcRoot, "vmstat")); err == nil {
		sample.VM = parseVMStat(data)
	} else {
		sample.ReadErrors.VM++
	}
	c.collectBlock(&sample)
	c.collectNetwork(&sample)
	var cgroupErrors uint64
	sample.HostCgroup, cgroupErrors = c.readCgroup(c.config.CgroupRoot)
	sample.ReadErrors.Cgroup += cgroupErrors
	c.collectProcesses(&sample)
	sample.CollectionNS = time.Since(collectionStarted).Nanoseconds()
	return sample
}

func (c *collector) collectCPU(sample *Sample) {
	data, err := readKernelFile(filepath.Join(c.config.ProcRoot, "stat"))
	if err != nil {
		sample.ReadErrors.CPU++
		return
	}
	current, err := parseProcStat(data)
	if err != nil {
		sample.ReadErrors.CPU++
		return
	}
	if c.haveCPU {
		sample.CPU = cpuDelta(c.previousCPU, current)
	} else {
		sample.CPU.LogicalCPUs = current.logicalCPUs
	}
	c.previousCPU, c.haveCPU = current, true
}

func (c *collector) readPSI(resource string, errorsCounter *uint64) PSIResource {
	data, err := readKernelFile(filepath.Join(c.config.ProcRoot, "pressure", resource))
	if err != nil {
		(*errorsCounter)++
		return PSIResource{}
	}
	parsed, err := parsePSI(data)
	if err != nil {
		(*errorsCounter)++
		return PSIResource{}
	}
	return parsed
}

func (c *collector) collectBlock(sample *Sample) {
	data, err := readKernelFile(filepath.Join(c.config.ProcRoot, "diskstats"))
	if err != nil {
		sample.ReadErrors.Block++
		return
	}
	devices, err := parseDiskstats(data)
	if err != nil {
		sample.ReadErrors.Block++
		return
	}
	filtered := devices[:0]
	for _, device := range devices {
		if c.includeBlockDevice(device.Name) {
			filtered = append(filtered, device)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Name < filtered[j].Name })
	if len(filtered) > c.config.MaxBlockDevices {
		filtered = filtered[:c.config.MaxBlockDevices]
		sample.BlockDevicesCapped = true
	}
	sample.BlockDevices = filtered
}

func (c *collector) includeBlockDevice(name string) bool {
	for _, excluded := range []string{"loop", "ram", "zram", "fd", "sr"} {
		if strings.HasPrefix(name, excluded) {
			return false
		}
	}
	_, err := os.Stat(filepath.Join(c.config.SysRoot, "class", "block", name, "partition"))
	return errors.Is(err, os.ErrNotExist)
}

func (c *collector) collectNetwork(sample *Sample) {
	if data, err := readKernelFile(filepath.Join(c.config.ProcRoot, "net", "dev")); err == nil {
		interfaces, parseErr := parseNetDev(data)
		if parseErr != nil {
			sample.ReadErrors.Network++
		} else {
			sort.Slice(interfaces, func(i, j int) bool { return interfaces[i].Name < interfaces[j].Name })
			if len(interfaces) > c.config.MaxInterfaces {
				interfaces = interfaces[:c.config.MaxInterfaces]
				sample.InterfacesCapped = true
			}
			sample.Network.Interfaces = interfaces
		}
	} else {
		sample.ReadErrors.Network++
	}
	if data, err := readKernelFile(filepath.Join(c.config.ProcRoot, "net", "snmp")); err == nil {
		if sample.Network.TCP, err = parseSNMPTCP(data); err != nil {
			sample.ReadErrors.Network++
		}
	} else {
		sample.ReadErrors.Network++
	}
	if data, err := readKernelFile(filepath.Join(c.config.ProcRoot, "net", "sockstat")); err == nil {
		if sample.Network.Sockets, err = parseSockstat(data); err != nil {
			sample.ReadErrors.Network++
		}
	} else {
		sample.ReadErrors.Network++
	}
}

func (c *collector) collectProcesses(sample *Sample) {
	entries, err := os.ReadDir(c.config.ProcRoot)
	if err != nil {
		sample.ReadErrors.Processes++
		return
	}
	var pids []int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	for _, pid := range pids {
		process, relevant, cgroupErrors, readErr := c.readProcess(pid)
		sample.ReadErrors.Cgroup += cgroupErrors
		if readErr != nil {
			if !errors.Is(readErr, os.ErrNotExist) {
				sample.ReadErrors.Processes++
			}
			continue
		}
		if !relevant {
			continue
		}
		if len(sample.Processes) == c.config.MaxProcesses {
			sample.ProcessesCapped = true
			break
		}
		sample.Processes = append(sample.Processes, process)
	}
}

func (c *collector) readProcess(pid int) (Process, bool, uint64, error) {
	directory := filepath.Join(c.config.ProcRoot, strconv.Itoa(pid))
	comm, err := readKernelFile(filepath.Join(directory, "comm"))
	if err != nil {
		return Process{}, false, 0, err
	}
	name := strings.TrimSpace(string(comm))
	if _, ok := c.processNames[name]; !ok {
		return Process{}, false, 0, nil
	}
	stat, err := readKernelFile(filepath.Join(directory, "stat"))
	if err != nil {
		return Process{}, false, 0, err
	}
	process, err := parseProcessStat(stat)
	if err != nil || process.PID != pid || process.Name != name {
		return Process{}, false, 0, errors.New("process identity changed while sampling")
	}
	if data, ioErr := readKernelFile(filepath.Join(directory, "io")); ioErr == nil {
		applyProcessIO(&process, data)
	} else if !errors.Is(ioErr, os.ErrNotExist) && !errors.Is(ioErr, os.ErrPermission) {
		return Process{}, false, 0, ioErr
	}
	var cgroupErrors uint64
	if data, cgroupErr := readKernelFile(filepath.Join(directory, "cgroup")); cgroupErr == nil {
		if relative, ok := parseCgroupPath(data); ok {
			root := filepath.Clean(c.config.CgroupRoot)
			path := filepath.Join(root, strings.TrimPrefix(relative, "/"))
			if withinRoot(root, path) {
				process.Cgroup, cgroupErrors = c.readCgroup(path)
			} else {
				cgroupErrors++
			}
		} else {
			cgroupErrors++
		}
	} else if !errors.Is(cgroupErr, os.ErrNotExist) {
		cgroupErrors++
	}
	return process, true, cgroupErrors, nil
}

func (c *collector) readCgroup(path string) (Cgroup, uint64) {
	if _, err := os.Stat(filepath.Join(path, "cgroup.controllers")); err != nil {
		return Cgroup{}, 1
	}
	names := []string{"cpu.stat", "memory.current", "memory.peak", "memory.swap.current", "memory.events", "pids.current", "io.stat", "cpu.pressure", "memory.pressure", "io.pressure"}
	data := make(map[string][]byte, len(names))
	var readErrors uint64
	for _, name := range names {
		value, err := readKernelFile(filepath.Join(path, name))
		if err == nil {
			data[name] = value
			continue
		}
		if name == "cpu.stat" || name == "memory.current" || name == "memory.events" || name == "pids.current" || name == "io.stat" {
			readErrors++
		}
	}
	return parseCgroup(data), readErrors
}

func readKernelFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxKernelFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxKernelFileBytes {
		return nil, fmt.Errorf("kernel observation exceeded %d bytes", maxKernelFileBytes)
	}
	return data, nil
}

func withinRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
