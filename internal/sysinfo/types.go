// Package sysinfo discovers current Linux hardware without adding inventory to
// metric samples or the historical storage format. All discovery is best effort.
package sysinfo

import "time"

// Details contains named hardware attributes. Missing attributes are omitted;
// an unreadable sensor or counter is never represented as a measured zero.
type Details map[string]string

type Snapshot struct {
	Timestamp   time.Time    `json:"ts"`
	MetricsTime *time.Time   `json:"metrics_ts,omitempty"`
	System      Details      `json:"system"`
	CPU         CPU          `json:"cpu"`
	Disks       []Disk       `json:"disks"`
	Filesystems []Filesystem `json:"filesystems"`
	Network     []Interface  `json:"network"`
	PCI         []Details    `json:"pci"`
	USB         []Details    `json:"usb"`
	Sensors     []Sensor     `json:"sensors"`
	Power       []Details    `json:"power"`
	Live        *Live        `json:"live,omitempty"`
}

// Live contains only the values from the latest in-memory sample presented by
// the inventory page. It is never filled from historical storage.
type Live struct {
	Memory LiveMemory `json:"mem"`
	System LiveSystem `json:"sys"`
	GPU    []LiveGPU  `json:"gpu,omitempty"`
}

type LiveMemory struct {
	Total uint64 `json:"total"`
}

type LiveSystem struct {
	UptimeHuman string `json:"uptime_human"`
}

type LiveGPU struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
}

type CPU struct {
	ModelName   string `json:"model_name,omitempty"`
	LogicalCPUs int    `json:"logical_cpus"`
	Cores       int    `json:"cores,omitempty"`
}

type Disk struct {
	Name    string   `json:"name"`
	Details Details  `json:"details"`
	Size    *uint64  `json:"size_bytes,omitempty"`
	Parent  string   `json:"-"`
	Slaves  []string `json:"-"`
	Mounts  []string `json:"mounts"`
}

type Filesystem struct {
	Device   string           `json:"device"`
	Mount    string           `json:"mount"`
	Type     string           `json:"type"`
	DeviceID string           `json:"-"`
	Usage    *FilesystemUsage `json:"usage,omitempty"`
}

type FilesystemUsage struct {
	Total uint64 `json:"total"`
}

type Interface struct {
	Name      string   `json:"name"`
	Details   Details  `json:"details"`
	Addresses []string `json:"addresses"`
	SpeedMbps *uint64  `json:"speed_mbps,omitempty"`
}

type Sensor struct {
	Device string  `json:"device"`
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	Unit   string  `json:"unit"`
}
