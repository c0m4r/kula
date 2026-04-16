package collector

import (
	"fmt"
	"strings"
)

// aiMaxItems is the maximum number of entries shown per category in FormatForAI.
const aiMaxItems = 10

// FormatForAI formats the current sample into a concise text block
// for the LLM system prompt.
func (s *Sample) FormatForAI() string {
	if s == nil {
		return "No metric data available yet. Ask the user to wait for the first sample.\n"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Current server metrics snapshot:\n")
	fmt.Fprintf(&sb, "  CPU usage:    %.1f%%\n", s.CPU.Total.Usage)
	fmt.Fprintf(&sb, "  Memory:       %.1f%% used (%s / %s)\n",
		s.Memory.UsedPercent, fmtBytesAI(s.Memory.Used), fmtBytesAI(s.Memory.Total))
	if s.Swap.Total > 0 {
		fmt.Fprintf(&sb, "  Swap:         %.1f%% used\n", s.Swap.UsedPercent)
	}
	fmt.Fprintf(&sb, "  Load avg:     %.2f / %.2f / %.2f (1m/5m/15m)\n",
		s.LoadAvg.Load1, s.LoadAvg.Load5, s.LoadAvg.Load15)
	fmt.Fprintf(&sb, "  Processes:    %d total, %d running, %d zombie\n",
		s.Process.Total, s.Process.Running, s.Process.Zombie)

	if n := len(s.Network.Interfaces); n > 0 {
		fmt.Fprintf(&sb, "  Network:\n")
		shown := n
		if shown > aiMaxItems {
			shown = aiMaxItems
		}
		for _, iface := range s.Network.Interfaces[:shown] {
			fmt.Fprintf(&sb, "    %s: ↓%.2f Mbps ↑%.2f Mbps\n",
				iface.Name, iface.RxMbps, iface.TxMbps)
		}
		if n > aiMaxItems {
			fmt.Fprintf(&sb, "    ... (%d more)\n", n-aiMaxItems)
		}
	}
	if n := len(s.Disks.Devices); n > 0 {
		fmt.Fprintf(&sb, "  Disk I/O:\n")
		shown := n
		if shown > aiMaxItems {
			shown = aiMaxItems
		}
		for _, d := range s.Disks.Devices[:shown] {
			fmt.Fprintf(&sb, "    %s: %.1f%% util, r=%.1f MB/s w=%.1f MB/s\n",
				d.Name, d.Utilization, d.ReadBytesPS/1e6, d.WriteBytesPS/1e6)
		}
		if n > aiMaxItems {
			fmt.Fprintf(&sb, "    ... (%d more)\n", n-aiMaxItems)
		}
	}
	if n := len(s.Disks.FileSystems); n > 0 {
		fmt.Fprintf(&sb, "  Disk space:\n")
		shown := n
		if shown > aiMaxItems {
			shown = aiMaxItems
		}
		for _, fs := range s.Disks.FileSystems[:shown] {
			fmt.Fprintf(&sb, "    %s: %.1f%% used (%s / %s)\n",
				fs.MountPoint, fs.UsedPct, fmtBytesAI(fs.Used), fmtBytesAI(fs.Total))
		}
		if n > aiMaxItems {
			fmt.Fprintf(&sb, "    ... (%d more)\n", n-aiMaxItems)
		}
	}
	if n := len(s.GPU); n > 0 {
		fmt.Fprintf(&sb, "  GPU:\n")
		shown := n
		if shown > aiMaxItems {
			shown = aiMaxItems
		}
		for _, gpu := range s.GPU[:shown] {
			fmt.Fprintf(&sb, "    %s: load=%.1f%%, vram=%.1f%%\n",
				gpu.Name, gpu.LoadPct, gpu.VRAMUsedPct)
		}
		if n > aiMaxItems {
			fmt.Fprintf(&sb, "    ... (%d more)\n", n-aiMaxItems)
		}
	}
	return sb.String()
}

// fmtBytesAI is a local bytes formatter for AI strings.
func fmtBytesAI(b uint64) string {
	const k = 1024
	switch {
	case b >= k*k*k*k:
		return fmt.Sprintf("%.1f TiB", float64(b)/(k*k*k*k))
	case b >= k*k*k:
		return fmt.Sprintf("%.1f GiB", float64(b)/(k*k*k))
	case b >= k*k:
		return fmt.Sprintf("%.1f MiB", float64(b)/(k*k))
	case b >= k:
		return fmt.Sprintf("%.1f KiB", float64(b)/k)
	default:
		return fmt.Sprintf("%d B", b)
	}
}
