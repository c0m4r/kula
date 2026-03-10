package collector

import "time"

// Sample holds all metrics collected at a single point in time.
type Sample struct {
	Timestamp time.Time `json:"ts"`

	CPU     CPUStats     `json:"cpu"`
	LoadAvg LoadAvg      `json:"lavg"`
	Memory  MemoryStats  `json:"mem"`
	Swap    SwapStats    `json:"swap"`
	Network NetworkStats `json:"net"`
	Disks   DiskStats    `json:"disk"`
	System  SystemStats  `json:"sys"`
	Process ProcessStats `json:"proc"`
	Self    SelfStats    `json:"self"`
}

// CPUStats holds per-core and total CPU usage percentages.
type CPUStats struct {
	Total       CPUCoreStats    `json:"total"`
	NumCores    int             `json:"num_cores"`
	Temperature float64         `json:"temp,omitempty"`
	Sensors     []CPUTempSensor `json:"sensors,omitempty"`
}

type CPUTempSensor struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

type CPUCoreStats struct {
	User    float64 `json:"user"`
	System  float64 `json:"system"`
	IOWait  float64 `json:"iowait"`
	IRQ     float64 `json:"irq"`
	SoftIRQ float64 `json:"softirq"`
	Steal   float64 `json:"steal"`
	Usage   float64 `json:"usage"` // 100 - idle
}

type LoadAvg struct {
	Load1   float64 `json:"load1"`
	Load5   float64 `json:"load5"`
	Load15  float64 `json:"load15"`
	Running int     `json:"running"`
	Total   int     `json:"total"`
}

type MemoryStats struct {
	Total       uint64  `json:"total"`
	Free        uint64  `json:"free"`
	Available   uint64  `json:"available"`
	Used        uint64  `json:"used"`
	Buffers     uint64  `json:"buffers"`
	Cached      uint64  `json:"cached"`
	Shmem       uint64  `json:"shmem"`
	UsedPercent float64 `json:"used_pct"`
}

type SwapStats struct {
	Total       uint64  `json:"total"`
	Free        uint64  `json:"free"`
	Used        uint64  `json:"used"`
	UsedPercent float64 `json:"used_pct"`
}

type NetworkStats struct {
	Interfaces []NetInterface `json:"ifaces"`
	TCP        TCPStats       `json:"tcp"`
	Sockets    SocketStats    `json:"sockets"`
}

type NetInterface struct {
	Name    string  `json:"name"`
	RxBytes uint64  `json:"rx_bytes"`
	TxBytes uint64  `json:"tx_bytes"`
	RxMbps  float64 `json:"rx_mbps"`
	TxMbps  float64 `json:"tx_mbps"`
	RxPkts  uint64  `json:"rx_pkts"`
	TxPkts  uint64  `json:"tx_pkts"`
	RxPPS   float64 `json:"rx_pps"`
	TxPPS   float64 `json:"tx_pps"`
	RxErrs  uint64  `json:"rx_errs"`
	TxErrs  uint64  `json:"tx_errs"`
	RxDrop  uint64  `json:"rx_drop"`
	TxDrop  uint64  `json:"tx_drop"`
}

// TCPStats holds key TCP protocol counters.
// CurrEstab is a gauge. InErrs and OutRsts are per-second rates (delta/elapsed).
type TCPStats struct {
	CurrEstab uint64  `json:"curr_estab"`
	InErrs    float64 `json:"in_errs_ps"`
	OutRsts   float64 `json:"out_rsts_ps"`
}

type SocketStats struct {
	TCPInUse int `json:"tcp_inuse"`
	TCPTw    int `json:"tcp_tw"`
	UDPInUse int `json:"udp_inuse"`
}

type DiskStats struct {
	Devices     []DiskDevice     `json:"devices"`
	FileSystems []FileSystemInfo `json:"filesystems"`
}

type DiskDevice struct {
	Name         string           `json:"name"`
	ReadsPerSec  float64          `json:"reads_ps"`
	WritesPerSec float64          `json:"writes_ps"`
	ReadBytesPS  float64          `json:"read_bps"`
	WriteBytesPS float64          `json:"write_bps"`
	Utilization  float64          `json:"util_pct"`
	Temperature  float64          `json:"temp,omitempty"`
	Sensors      []DiskTempSensor `json:"sensors,omitempty"`
}

type DiskTempSensor struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
}

type FileSystemInfo struct {
	Device     string  `json:"device"`
	MountPoint string  `json:"mount"`
	FSType     string  `json:"fstype"`
	Total      uint64  `json:"total"`
	Used       uint64  `json:"used"`
	Available  uint64  `json:"available"`
	UsedPct    float64 `json:"used_pct"`
}

type SystemStats struct {
	Hostname    string  `json:"hostname"`
	Uptime      float64 `json:"uptime_sec"`
	UptimeHuman string  `json:"uptime_human"`
	Entropy     int     `json:"entropy"`
	ClockSync   bool    `json:"clock_synced"`
	ClockSource string  `json:"clock_source"`
	UserCount   int     `json:"user_count"`
}

type ProcessStats struct {
	Total    int `json:"total"`
	Running  int `json:"running"`
	Sleeping int `json:"sleeping"`
	Zombie   int `json:"zombie"`
	Blocked  int `json:"blocked"`
	Threads  int `json:"threads"`
}

type SelfStats struct {
	CPUPercent float64 `json:"cpu_pct"`
	MemRSS     uint64  `json:"mem_rss"`
	FDs        int     `json:"fds"`
}
