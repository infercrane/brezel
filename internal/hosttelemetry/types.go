// Package hosttelemetry collects bounded, content-free Linux host evidence for
// benchmark diagnosis. It never enters a guest or reads command lines,
// environment variables, network addresses, file contents, or credentials.
package hosttelemetry

import "time"

const (
	SchemaVersion  = 1
	SampleInterval = time.Second
)

type Sample struct {
	SchemaVersion      int           `json:"schema_version"`
	Sequence           uint64        `json:"sequence"`
	SampledAt          string        `json:"sampled_at"`
	ElapsedNS          int64         `json:"elapsed_ns"`
	CollectionNS       int64         `json:"collection_ns"`
	CPU                CPU           `json:"cpu"`
	Pressure           Pressure      `json:"pressure"`
	Memory             Memory        `json:"memory"`
	VM                 VM            `json:"vm"`
	BlockDevices       []BlockDevice `json:"block_devices"`
	Network            Network       `json:"network"`
	HostCgroup         Cgroup        `json:"host_cgroup"`
	Processes          []Process     `json:"processes"`
	ReadErrors         ReadErrors    `json:"read_errors"`
	BlockDevicesCapped bool          `json:"block_devices_capped"`
	InterfacesCapped   bool          `json:"interfaces_capped"`
	ProcessesCapped    bool          `json:"processes_capped"`
}

type CPU struct {
	LogicalCPUs    int     `json:"logical_cpus"`
	DeltaTotal     uint64  `json:"delta_total_ticks"`
	DeltaBusy      uint64  `json:"delta_busy_ticks"`
	DeltaIOWait    uint64  `json:"delta_iowait_ticks"`
	DeltaSteal     uint64  `json:"delta_steal_ticks"`
	BusyPercent    float64 `json:"busy_percent"`
	IOWaitPercent  float64 `json:"iowait_percent"`
	StealPercent   float64 `json:"steal_percent"`
	ContextSwitch  uint64  `json:"context_switches_total"`
	ProcessesForks uint64  `json:"processes_forked_total"`
	ProcsRunning   uint64  `json:"processes_running"`
	ProcsBlocked   uint64  `json:"processes_blocked"`
}

type Pressure struct {
	CPU    PSIResource `json:"cpu"`
	Memory PSIResource `json:"memory"`
	IO     PSIResource `json:"io"`
}

type PSIResource struct {
	Available bool    `json:"available"`
	Some      PSILine `json:"some"`
	Full      PSILine `json:"full"`
}

type PSILine struct {
	Present bool    `json:"present"`
	Avg10   float64 `json:"avg10"`
	Avg60   float64 `json:"avg60"`
	Avg300  float64 `json:"avg300"`
	TotalUS uint64  `json:"total_us"`
}

type Memory struct {
	MemTotalKB     uint64 `json:"mem_total_kb"`
	MemAvailableKB uint64 `json:"mem_available_kb"`
	SwapTotalKB    uint64 `json:"swap_total_kb"`
	SwapFreeKB     uint64 `json:"swap_free_kb"`
	DirtyKB        uint64 `json:"dirty_kb"`
	WritebackKB    uint64 `json:"writeback_kb"`
	AnonPagesKB    uint64 `json:"anon_pages_kb"`
	SlabKB         uint64 `json:"slab_kb"`
	SReclaimableKB uint64 `json:"s_reclaimable_kb"`
}

type VM struct {
	PageFaults       uint64 `json:"page_faults_total"`
	MajorPageFaults  uint64 `json:"major_page_faults_total"`
	SwapInPages      uint64 `json:"swap_in_pages_total"`
	SwapOutPages     uint64 `json:"swap_out_pages_total"`
	OOMKills         uint64 `json:"oom_kills_total"`
	AllocStalls      uint64 `json:"alloc_stalls_total"`
	CompactStalls    uint64 `json:"compact_stalls_total"`
	DirectPageScans  uint64 `json:"direct_page_scans_total"`
	KswapdPageScans  uint64 `json:"kswapd_page_scans_total"`
	DirectPageSteals uint64 `json:"direct_page_steals_total"`
	KswapdPageSteals uint64 `json:"kswapd_page_steals_total"`
}

type BlockDevice struct {
	Name              string `json:"name"`
	ReadsCompleted    uint64 `json:"reads_completed_total"`
	ReadsMerged       uint64 `json:"reads_merged_total"`
	SectorsRead       uint64 `json:"sectors_read_total"`
	ReadTimeMS        uint64 `json:"read_time_ms_total"`
	WritesCompleted   uint64 `json:"writes_completed_total"`
	WritesMerged      uint64 `json:"writes_merged_total"`
	SectorsWritten    uint64 `json:"sectors_written_total"`
	WriteTimeMS       uint64 `json:"write_time_ms_total"`
	IOInProgress      uint64 `json:"io_in_progress"`
	IOTimeMS          uint64 `json:"io_time_ms_total"`
	WeightedIOTimeMS  uint64 `json:"weighted_io_time_ms_total"`
	DiscardsCompleted uint64 `json:"discards_completed_total"`
	DiscardSectors    uint64 `json:"discard_sectors_total"`
	FlushesCompleted  uint64 `json:"flushes_completed_total"`
}

type Network struct {
	Interfaces []NetworkInterface `json:"interfaces"`
	TCP        TCP                `json:"tcp"`
	Sockets    SocketStats        `json:"sockets"`
}

type NetworkInterface struct {
	Name      string `json:"name"`
	RXBytes   uint64 `json:"rx_bytes_total"`
	RXPackets uint64 `json:"rx_packets_total"`
	RXErrors  uint64 `json:"rx_errors_total"`
	RXDropped uint64 `json:"rx_dropped_total"`
	TXBytes   uint64 `json:"tx_bytes_total"`
	TXPackets uint64 `json:"tx_packets_total"`
	TXErrors  uint64 `json:"tx_errors_total"`
	TXDropped uint64 `json:"tx_dropped_total"`
}

type TCP struct {
	ActiveOpens  uint64 `json:"active_opens_total"`
	PassiveOpens uint64 `json:"passive_opens_total"`
	AttemptFails uint64 `json:"attempt_fails_total"`
	EstabResets  uint64 `json:"established_resets_total"`
	CurrentEstab uint64 `json:"current_established"`
	InSegments   uint64 `json:"in_segments_total"`
	OutSegments  uint64 `json:"out_segments_total"`
	Retransmits  uint64 `json:"retransmitted_segments_total"`
	InErrors     uint64 `json:"in_errors_total"`
	OutResets    uint64 `json:"out_resets_total"`
}

type SocketStats struct {
	TCPInUse       uint64 `json:"tcp_in_use"`
	TCPOrphan      uint64 `json:"tcp_orphan"`
	TCPTimeWait    uint64 `json:"tcp_time_wait"`
	TCPAllocated   uint64 `json:"tcp_allocated"`
	TCPMemoryPages uint64 `json:"tcp_memory_pages"`
}

type Cgroup struct {
	Available           bool     `json:"available"`
	CPUUsageUS          uint64   `json:"cpu_usage_us_total"`
	CPUUserUS           uint64   `json:"cpu_user_us_total"`
	CPUSystemUS         uint64   `json:"cpu_system_us_total"`
	CPUThrottledUS      uint64   `json:"cpu_throttled_us_total"`
	CPUThrottledPeriods uint64   `json:"cpu_throttled_periods_total"`
	MemoryCurrent       uint64   `json:"memory_current_bytes"`
	MemoryPeak          uint64   `json:"memory_peak_bytes"`
	SwapCurrent         uint64   `json:"swap_current_bytes"`
	MemoryLow           uint64   `json:"memory_low_events_total"`
	MemoryHigh          uint64   `json:"memory_high_events_total"`
	MemoryMax           uint64   `json:"memory_max_events_total"`
	MemoryOOM           uint64   `json:"memory_oom_events_total"`
	MemoryOOMKills      uint64   `json:"memory_oom_kill_events_total"`
	PIDsCurrent         uint64   `json:"pids_current"`
	IOReadBytes         uint64   `json:"io_read_bytes_total"`
	IOWriteBytes        uint64   `json:"io_write_bytes_total"`
	IOReadOps           uint64   `json:"io_read_ops_total"`
	IOWriteOps          uint64   `json:"io_write_ops_total"`
	Pressure            Pressure `json:"pressure"`
}

type Process struct {
	PID                 int    `json:"pid"`
	Name                string `json:"name"`
	StartTicks          uint64 `json:"start_ticks"`
	UserTicks           uint64 `json:"user_ticks_total"`
	SystemTicks         uint64 `json:"system_ticks_total"`
	MinorFaults         uint64 `json:"minor_faults_total"`
	MajorFaults         uint64 `json:"major_faults_total"`
	Threads             uint64 `json:"threads"`
	VirtualBytes        uint64 `json:"virtual_bytes"`
	ResidentPages       int64  `json:"resident_pages"`
	ReadBytes           uint64 `json:"read_bytes_total"`
	WriteBytes          uint64 `json:"write_bytes_total"`
	CancelledWriteBytes uint64 `json:"cancelled_write_bytes_total"`
	ReadSyscalls        uint64 `json:"read_syscalls_total"`
	WriteSyscalls       uint64 `json:"write_syscalls_total"`
	Cgroup              Cgroup `json:"cgroup"`
}

type ReadErrors struct {
	CPU       uint64 `json:"cpu"`
	Pressure  uint64 `json:"pressure"`
	Memory    uint64 `json:"memory"`
	VM        uint64 `json:"vm"`
	Block     uint64 `json:"block"`
	Network   uint64 `json:"network"`
	Cgroup    uint64 `json:"cgroup"`
	Processes uint64 `json:"processes"`
}

type Manifest struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	StartedAt     string `json:"started_at"`
	FinishedAt    string `json:"finished_at"`
	DurationNS    int64  `json:"duration_ns"`
	IntervalNS    int64  `json:"interval_ns"`
	SampleCount   uint64 `json:"sample_count"`
	SampleBytes   int64  `json:"sample_bytes"`
	MaxBytes      int64  `json:"max_bytes"`
	SamplesFile   string `json:"samples_file"`
	SamplesSHA256 string `json:"samples_sha256"`
	StopReason    string `json:"stop_reason"`
	Truncated     bool   `json:"truncated"`
}
