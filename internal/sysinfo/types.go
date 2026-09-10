// Package sysinfo discovers current Linux hardware without adding inventory to
// metric samples or the historical storage format. All discovery is best effort.
package sysinfo

import (
	"time"

	"kula/internal/collector"
)

// Details contains named hardware attributes. Missing attributes are omitted;
// an unreadable sensor or counter is never represented as a measured zero.
type Details map[string]string

type Snapshot struct {
	Timestamp   time.Time    `json:"ts"`
	MetricsTime *time.Time   `json:"metrics_ts,omitempty"`
	System      Details      `json:"system"`
	Board       Details      `json:"board"`
	BIOS        Details      `json:"bios"`
	CPU         CPU          `json:"cpu"`
	Memory      Details      `json:"memory"`
	DIMMs       []Details    `json:"dimms"`
	Disks       []Disk       `json:"disks"`
	Filesystems []Filesystem `json:"filesystems"`
	Network     []Interface  `json:"network"`
	PCI         []Details    `json:"pci"`
	USB         []Details    `json:"usb"`
	Sensors     []Sensor     `json:"sensors"`
	Power       []Details    `json:"power"`
	Live        *Live        `json:"live,omitempty"`
}

// Live holds only the relevant parts of the latest in-memory collection. It is
// never filled from storage, including when the dashboard is viewing history.
type Live struct {
	CPU     collector.CPUStats     `json:"cpu"`
	Memory  collector.MemoryStats  `json:"mem"`
	Swap    collector.SwapStats    `json:"swap"`
	System  collector.SystemStats  `json:"sys"`
	Load    collector.LoadAvg      `json:"lavg"`
	Process collector.ProcessStats `json:"proc"`
	GPU     []collector.GPUStats   `json:"gpu"`
}

type CPU struct {
	Details         Details   `json:"details"`
	LogicalCPUs     int       `json:"logical_cpus"`
	Cores           int       `json:"cores,omitempty"`
	Sockets         int       `json:"sockets,omitempty"`
	Caches          []Details `json:"caches"`
	Frequency       []Details `json:"frequency"`
	Vulnerabilities Details   `json:"vulnerabilities"`
}

type Disk struct {
	Name     string   `json:"name"`
	Details  Details  `json:"details"`
	Size     *uint64  `json:"size_bytes,omitempty"`
	Parent   string   `json:"parent,omitempty"`
	Slaves   []string `json:"slaves"`
	Mounts   []string `json:"mounts"`
	ReadBPS  *float64 `json:"read_bps,omitempty"`
	WriteBPS *float64 `json:"write_bps,omitempty"`
	ReadsPS  *float64 `json:"reads_ps,omitempty"`
	WritesPS *float64 `json:"writes_ps,omitempty"`
	BusyPct  *float64 `json:"busy_pct,omitempty"`
	InFlight *uint64  `json:"in_flight,omitempty"`
}

type Filesystem struct {
	Device   string                    `json:"device"`
	Mount    string                    `json:"mount"`
	Type     string                    `json:"type"`
	DeviceID string                    `json:"device_id"`
	Options  string                    `json:"options"`
	Usage    *collector.FileSystemInfo `json:"usage,omitempty"`
}

type Interface struct {
	Name      string   `json:"name"`
	Details   Details  `json:"details"`
	Addresses []string `json:"addresses"`
	SpeedMbps *uint64  `json:"speed_mbps,omitempty"`
	RxBytes   *uint64  `json:"rx_bytes,omitempty"`
	TxBytes   *uint64  `json:"tx_bytes,omitempty"`
	RxErrors  *uint64  `json:"rx_errors,omitempty"`
	TxErrors  *uint64  `json:"tx_errors,omitempty"`
	RxDropped *uint64  `json:"rx_dropped,omitempty"`
	TxDropped *uint64  `json:"tx_dropped,omitempty"`
	RxMbps    *float64 `json:"rx_mbps,omitempty"`
	TxMbps    *float64 `json:"tx_mbps,omitempty"`
	RxPct     *float64 `json:"rx_pct,omitempty"`
	TxPct     *float64 `json:"tx_pct,omitempty"`
}

type Sensor struct {
	Device string  `json:"device"`
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	Unit   string  `json:"unit"`
}
