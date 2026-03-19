package storage

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"kula/internal/collector"
	"math"
	"sync"
	"time"
)

// Flags packed into the 2-byte preamble flags field.
const (
	flagHasMin  uint16 = 1 << 0
	flagHasMax  uint16 = 1 << 1
	flagHasData uint16 = 1 << 2
)

// fixedBlockSize is the size in bytes of the encoded fixed scalar block.
const fixedBlockSize = 218

// recordKindBinary is the one-byte tag written as the first byte of every new
// binary payload (after the 4-byte length prefix). Legacy JSON records start
// with '{' (0x7B) and legacy binary records written before this tag was
// introduced have no kind byte. This makes new binary records fully
// deterministic to identify without peeking at timestamp bytes.
const recordKindBinary = byte(0x02)

// AggregatedSample holds a time-aggregated metric sample.
// For tier 0 (1s), this is just a wrapper around the raw sample.
// For higher tiers, Data holds averaged values and Min/Max fields hold
// the minimum and maximum observed values over the aggregation window.
type AggregatedSample struct {
	Timestamp time.Time         `json:"ts"`
	Duration  time.Duration     `json:"dur"`
	Data      *collector.Sample `json:"data"`
	Min       *collector.Sample `json:"min,omitempty"`
	Max       *collector.Sample `json:"max,omitempty"`
}

// encPool holds reusable byte slices to make encodeSample allocation-free on
// the hot 1 Hz write path. Each pool entry is a *[]byte pointing to a slice
// with cap ~3000. The single heap allocation per call is the output record.
var encPool = sync.Pool{New: func() any { b := make([]byte, 0, 3000); return &b }}

// ---- Float32 helpers --------------------------------------------------------

// putF32 encodes a float64 as a float32 in little-endian byte order.
// Rate/percentage fields need at most 7 significant digits — float32 is sufficient.
func putF32(b []byte, v float64) {
	binary.LittleEndian.PutUint32(b, math.Float32bits(float32(v)))
}

// getF32 decodes a little-endian float32 to float64.
func getF32(b []byte) float64 {
	return float64(math.Float32frombits(binary.LittleEndian.Uint32(b)))
}

// ---- Length-prefixed string helpers -----------------------------------------

// appendStr appends a uint8-length-prefixed UTF-8 string. Returns an error if
// the string exceeds the 255-byte on-disk limit; callers must not truncate
// silently, as that would misrepresent stored data.
func appendStr(buf []byte, s string) ([]byte, error) {
	n := len(s)
	if n > 255 {
		return buf, fmt.Errorf("string exceeds 255-byte storage limit (%d bytes)", n)
	}
	buf = append(buf, byte(n))
	return append(buf, s[:n]...), nil
}

// getStr reads a uint8-length-prefixed string. Returns the string, bytes consumed, and an error
// if the buffer is too short to hold the declared string length.
func getStr(buf []byte) (string, int, error) {
	if len(buf) == 0 {
		return "", 0, fmt.Errorf("empty buffer for string")
	}
	n := int(buf[0])
	if n > len(buf)-1 {
		return "", 0, fmt.Errorf("string truncated: claims %d bytes, have %d", n, len(buf)-1)
	}
	return string(buf[1 : 1+n]), 1 + n, nil
}

// ---- Append helpers ---------------------------------------------------------

func appendUint16(b []byte, v uint16) []byte {
	var x [2]byte
	binary.LittleEndian.PutUint16(x[:], v)
	return append(b, x[:]...)
}

func appendUint64(b []byte, v uint64) []byte {
	var x [8]byte
	binary.LittleEndian.PutUint64(x[:], v)
	return append(b, x[:]...)
}


func appendF32(b []byte, v float64) []byte {
	var x [4]byte
	putF32(x[:], v)
	return append(b, x[:]...)
}

// ---- Encoder ----------------------------------------------------------------

// encodeSample encodes an AggregatedSample to a binary payload (no length prefix).
// Uses a sync.Pool to reuse the build buffer — only one heap allocation (the
// output slice) occurs per call after the pool has warmed up.
func encodeSample(a *AggregatedSample) ([]byte, error) {
	ptr := encPool.Get().(*[]byte)
	buf := (*ptr)[:0]

	buf = appendPreamble(buf, a)
	var err error
	if a.Data != nil {
		buf = appendFixed(buf, a.Data)
		if buf, err = appendVariable(buf, a.Data); err != nil {
			*ptr = buf
			encPool.Put(ptr)
			return nil, fmt.Errorf("encode data: %w", err)
		}
	}
	if a.Min != nil {
		buf = appendFixed(buf, a.Min)
		if buf, err = appendVariable(buf, a.Min); err != nil {
			*ptr = buf
			encPool.Put(ptr)
			return nil, fmt.Errorf("encode min: %w", err)
		}
	}
	if a.Max != nil {
		buf = appendFixed(buf, a.Max)
		if buf, err = appendVariable(buf, a.Max); err != nil {
			*ptr = buf
			encPool.Put(ptr)
			return nil, fmt.Errorf("encode max: %w", err)
		}
	}

	out := make([]byte, len(buf))
	copy(out, buf)

	*ptr = buf
	encPool.Put(ptr)
	return out, nil
}

// appendPreamble writes the 18-byte record preamble:
//
//	[0:8]   timestamp_ns int64 LE  ← fixed offset; extractTimestamp reads here
//	[8:16]  duration_ns  int64 LE
//	[16:18] flags        uint16 LE (flagHasData/Min/Max)
func appendPreamble(buf []byte, a *AggregatedSample) []byte {
	var b [18]byte
	binary.LittleEndian.PutUint64(b[0:], uint64(a.Timestamp.UnixNano()))
	binary.LittleEndian.PutUint64(b[8:], uint64(a.Duration))
	var flags uint16
	if a.Data != nil {
		flags |= flagHasData
	}
	if a.Min != nil {
		flags |= flagHasMin
	}
	if a.Max != nil {
		flags |= flagHasMax
	}
	binary.LittleEndian.PutUint16(b[16:], flags)
	return append(buf, b[:]...)
}

// appendFixed encodes a *collector.Sample into a 218-byte stack-allocated array.
// The array never escapes to the heap; it is copied into buf in one append call.
//
// Fixed block offsets (local to the 218-byte array):
//
//	[0:28]   cpu total × 7 float32 (usage, user, sys, iowait, irq, softirq, steal)
//	[28:30]  num_cores uint16
//	[30:34]  cpu_temp  float32
//	[34:46]  load[3]   float32
//	[46:48]  load_running uint16
//	[48:50]  load_total   uint16
//	[50:106] mem[7]    uint64
//	[106:110] mem_used_pct float32
//	[110:134] swap[3]  uint64
//	[134:138] swap_used_pct float32
//	[138:146] tcp_curr_estab uint64
//	[146:150] tcp_in_errs float32
//	[150:154] tcp_out_rsts float32
//	[154:158] sock_tcp_inuse int32
//	[158:162] sock_tcp_tw   int32
//	[162:166] sock_udp_inuse int32
//	[166:190] proc[6] int32
//	[190:198] uptime_sec float64
//	[198:202] entropy    int32
//	[202:203] user_count uint8
//	[203:204] clock_sync uint8
//	[204:212] self_rss   uint64
//	[212:216] self_cpu_pct float32
//	[216:218] self_fds  uint16
func appendFixed(buf []byte, s *collector.Sample) []byte {
	var b [fixedBlockSize]byte
	if s == nil {
		return append(buf, b[:]...)
	}
	// CPU total (28 bytes)
	putF32(b[0:], s.CPU.Total.Usage)
	putF32(b[4:], s.CPU.Total.User)
	putF32(b[8:], s.CPU.Total.System)
	putF32(b[12:], s.CPU.Total.IOWait)
	putF32(b[16:], s.CPU.Total.IRQ)
	putF32(b[20:], s.CPU.Total.SoftIRQ)
	putF32(b[24:], s.CPU.Total.Steal)
	// CPU meta (6 bytes)
	binary.LittleEndian.PutUint16(b[28:], uint16(s.CPU.NumCores))
	putF32(b[30:], s.CPU.Temperature)
	// Load average (16 bytes)
	putF32(b[34:], s.LoadAvg.Load1)
	putF32(b[38:], s.LoadAvg.Load5)
	putF32(b[42:], s.LoadAvg.Load15)
	binary.LittleEndian.PutUint16(b[46:], uint16(s.LoadAvg.Running))
	binary.LittleEndian.PutUint16(b[48:], uint16(s.LoadAvg.Total))
	// Memory (60 bytes)
	binary.LittleEndian.PutUint64(b[50:], s.Memory.Total)
	binary.LittleEndian.PutUint64(b[58:], s.Memory.Free)
	binary.LittleEndian.PutUint64(b[66:], s.Memory.Available)
	binary.LittleEndian.PutUint64(b[74:], s.Memory.Used)
	binary.LittleEndian.PutUint64(b[82:], s.Memory.Buffers)
	binary.LittleEndian.PutUint64(b[90:], s.Memory.Cached)
	binary.LittleEndian.PutUint64(b[98:], s.Memory.Shmem)
	putF32(b[106:], s.Memory.UsedPercent)
	// Swap (28 bytes)
	binary.LittleEndian.PutUint64(b[110:], s.Swap.Total)
	binary.LittleEndian.PutUint64(b[118:], s.Swap.Free)
	binary.LittleEndian.PutUint64(b[126:], s.Swap.Used)
	putF32(b[134:], s.Swap.UsedPercent)
	// TCP + sockets (28 bytes)
	binary.LittleEndian.PutUint64(b[138:], s.Network.TCP.CurrEstab)
	putF32(b[146:], s.Network.TCP.InErrs)
	putF32(b[150:], s.Network.TCP.OutRsts)
	binary.LittleEndian.PutUint32(b[154:], uint32(int32(s.Network.Sockets.TCPInUse)))
	binary.LittleEndian.PutUint32(b[158:], uint32(int32(s.Network.Sockets.TCPTw)))
	binary.LittleEndian.PutUint32(b[162:], uint32(int32(s.Network.Sockets.UDPInUse)))
	// Process (24 bytes)
	binary.LittleEndian.PutUint32(b[166:], uint32(int32(s.Process.Total)))
	binary.LittleEndian.PutUint32(b[170:], uint32(int32(s.Process.Running)))
	binary.LittleEndian.PutUint32(b[174:], uint32(int32(s.Process.Sleeping)))
	binary.LittleEndian.PutUint32(b[178:], uint32(int32(s.Process.Zombie)))
	binary.LittleEndian.PutUint32(b[182:], uint32(int32(s.Process.Blocked)))
	binary.LittleEndian.PutUint32(b[186:], uint32(int32(s.Process.Threads)))
	// System (14 bytes)
	binary.LittleEndian.PutUint64(b[190:], math.Float64bits(s.System.Uptime))
	binary.LittleEndian.PutUint32(b[198:], uint32(int32(s.System.Entropy)))
	b[202] = uint8(s.System.UserCount)
	if s.System.ClockSync {
		b[203] = 1
	}
	// Self (14 bytes)
	binary.LittleEndian.PutUint64(b[204:], s.Self.MemRSS)
	putF32(b[212:], s.Self.CPUPercent)
	binary.LittleEndian.PutUint16(b[216:], uint16(s.Self.FDs))
	return append(buf, b[:]...)
}

// appendVariable encodes the variable-length sections of a sample:
// network interfaces, CPU sensors, disk devices, filesystems, system strings, GPU entries.
func appendVariable(buf []byte, s *collector.Sample) ([]byte, error) {
	if s == nil {
		// Write zero counts for all 5 array sections.
		for i := 0; i < 5; i++ {
			buf = appendUint16(buf, 0)
		}
		// Empty system strings (hostname, clock_source).
		buf = append(buf, 0, 0)
		return buf, nil
	}

	var err error

	// Network interfaces
	buf = appendUint16(buf, uint16(len(s.Network.Interfaces)))
	for _, iface := range s.Network.Interfaces {
		if buf, err = appendStr(buf, iface.Name); err != nil {
			return buf, fmt.Errorf("iface name: %w", err)
		}
		buf = appendF32(buf, iface.RxMbps)
		buf = appendF32(buf, iface.TxMbps)
		buf = appendF32(buf, iface.RxPPS)
		buf = appendF32(buf, iface.TxPPS)
		buf = appendUint64(buf, iface.RxBytes)
		buf = appendUint64(buf, iface.TxBytes)
		buf = appendUint64(buf, iface.RxPkts)
		buf = appendUint64(buf, iface.TxPkts)
		buf = appendUint64(buf, iface.RxErrs)
		buf = appendUint64(buf, iface.TxErrs)
		buf = appendUint64(buf, iface.RxDrop)
		buf = appendUint64(buf, iface.TxDrop)
	}

	// CPU temperature sensors
	buf = appendUint16(buf, uint16(len(s.CPU.Sensors)))
	for _, sensor := range s.CPU.Sensors {
		if buf, err = appendStr(buf, sensor.Name); err != nil {
			return buf, fmt.Errorf("cpu sensor name: %w", err)
		}
		buf = appendF32(buf, sensor.Value)
	}

	// Disk devices
	buf = appendUint16(buf, uint16(len(s.Disks.Devices)))
	for _, dev := range s.Disks.Devices {
		if buf, err = appendStr(buf, dev.Name); err != nil {
			return buf, fmt.Errorf("disk name: %w", err)
		}
		buf = appendF32(buf, dev.ReadsPerSec)
		buf = appendF32(buf, dev.WritesPerSec)
		buf = appendF32(buf, dev.ReadBytesPS)
		buf = appendF32(buf, dev.WriteBytesPS)
		buf = appendF32(buf, dev.Utilization)
		buf = appendF32(buf, dev.Temperature)
		buf = appendUint16(buf, uint16(len(dev.Sensors)))
		for _, ts := range dev.Sensors {
			if buf, err = appendStr(buf, ts.Name); err != nil {
				return buf, fmt.Errorf("disk sensor name: %w", err)
			}
			buf = appendF32(buf, ts.Value)
		}
	}

	// Filesystems
	buf = appendUint16(buf, uint16(len(s.Disks.FileSystems)))
	for _, fs := range s.Disks.FileSystems {
		if buf, err = appendStr(buf, fs.Device); err != nil {
			return buf, fmt.Errorf("fs device: %w", err)
		}
		if buf, err = appendStr(buf, fs.MountPoint); err != nil {
			return buf, fmt.Errorf("fs mountpoint: %w", err)
		}
		if buf, err = appendStr(buf, fs.FSType); err != nil {
			return buf, fmt.Errorf("fs type: %w", err)
		}
		buf = appendUint64(buf, fs.Total)
		buf = appendUint64(buf, fs.Used)
		buf = appendUint64(buf, fs.Available)
		buf = appendF32(buf, fs.UsedPct)
	}

	// System strings
	if buf, err = appendStr(buf, s.System.Hostname); err != nil {
		return buf, fmt.Errorf("hostname: %w", err)
	}
	if buf, err = appendStr(buf, s.System.ClockSource); err != nil {
		return buf, fmt.Errorf("clock source: %w", err)
	}

	// GPU entries — count is uint16-encoded; cap to avoid silent truncation.
	gpuCount := len(s.GPU)
	if gpuCount > 65535 {
		gpuCount = 65535
	}
	buf = appendUint16(buf, uint16(gpuCount))
	for _, g := range s.GPU[:gpuCount] {
		buf = appendUint16(buf, uint16(g.Index))
		if buf, err = appendStr(buf, g.Name); err != nil {
			return buf, fmt.Errorf("gpu name: %w", err)
		}
		if buf, err = appendStr(buf, g.Driver); err != nil {
			return buf, fmt.Errorf("gpu driver: %w", err)
		}
		buf = appendF32(buf, g.Temperature)
		buf = appendUint64(buf, g.VRAMUsed)
		buf = appendUint64(buf, g.VRAMTotal)
		buf = appendF32(buf, g.VRAMUsedPct)
		buf = appendF32(buf, g.LoadPct)
		buf = appendF32(buf, g.PowerW)
	}

	return buf, nil
}

// ---- Decoder ----------------------------------------------------------------

// decodeSample decodes a binary v2 payload produced by encodeSample.
// data must NOT include the 4-byte length prefix (stripped by the caller).
func decodeSample(data []byte) (*AggregatedSample, error) {
	if len(data) < 18 {
		return nil, fmt.Errorf("record too short: %d bytes", len(data))
	}

	a := &AggregatedSample{}
	a.Timestamp = time.Unix(0, int64(binary.LittleEndian.Uint64(data[0:])))
	a.Duration = time.Duration(binary.LittleEndian.Uint64(data[8:]))
	flags := binary.LittleEndian.Uint16(data[16:])
	off := 18

	if flags&flagHasData != 0 {
		s, n, err := decodeFixed(data[off:])
		if err != nil {
			return nil, fmt.Errorf("decode data fixed: %w", err)
		}
		off += n
		vn, err := decodeVariable(data[off:], s)
		if err != nil {
			return nil, fmt.Errorf("decode data variable: %w", err)
		}
		off += vn
		s.Timestamp = a.Timestamp // propagate outer ts so JS item.data.ts is correct
		a.Data = s
	}

	if flags&flagHasMin != 0 {
		s, n, err := decodeFixed(data[off:])
		if err != nil {
			return nil, fmt.Errorf("decode min fixed: %w", err)
		}
		off += n
		vn, err := decodeVariable(data[off:], s)
		if err != nil {
			return nil, fmt.Errorf("decode min variable: %w", err)
		}
		off += vn
		s.Timestamp = a.Timestamp
		a.Min = s
	}

	if flags&flagHasMax != 0 {
		s, n, err := decodeFixed(data[off:])
		if err != nil {
			return nil, fmt.Errorf("decode max fixed: %w", err)
		}
		off += n
		vn, err := decodeVariable(data[off:], s)
		if err != nil {
			return nil, fmt.Errorf("decode max variable: %w", err)
		}
		_ = vn
		s.Timestamp = a.Timestamp
		a.Max = s
	}

	return a, nil
}

// decodeFixed decodes the 218-byte fixed scalar block from data.
// Returns the decoded sample, the number of bytes consumed, and any error.
func decodeFixed(data []byte) (*collector.Sample, int, error) {
	if len(data) < fixedBlockSize {
		return nil, 0, fmt.Errorf("fixed block too short: %d bytes", len(data))
	}
	s := &collector.Sample{}
	// CPU total
	s.CPU.Total.Usage = getF32(data[0:])
	s.CPU.Total.User = getF32(data[4:])
	s.CPU.Total.System = getF32(data[8:])
	s.CPU.Total.IOWait = getF32(data[12:])
	s.CPU.Total.IRQ = getF32(data[16:])
	s.CPU.Total.SoftIRQ = getF32(data[20:])
	s.CPU.Total.Steal = getF32(data[24:])
	// CPU meta
	s.CPU.NumCores = int(binary.LittleEndian.Uint16(data[28:]))
	s.CPU.Temperature = getF32(data[30:])
	// Load average
	s.LoadAvg.Load1 = getF32(data[34:])
	s.LoadAvg.Load5 = getF32(data[38:])
	s.LoadAvg.Load15 = getF32(data[42:])
	s.LoadAvg.Running = int(binary.LittleEndian.Uint16(data[46:]))
	s.LoadAvg.Total = int(binary.LittleEndian.Uint16(data[48:]))
	// Memory
	s.Memory.Total = binary.LittleEndian.Uint64(data[50:])
	s.Memory.Free = binary.LittleEndian.Uint64(data[58:])
	s.Memory.Available = binary.LittleEndian.Uint64(data[66:])
	s.Memory.Used = binary.LittleEndian.Uint64(data[74:])
	s.Memory.Buffers = binary.LittleEndian.Uint64(data[82:])
	s.Memory.Cached = binary.LittleEndian.Uint64(data[90:])
	s.Memory.Shmem = binary.LittleEndian.Uint64(data[98:])
	s.Memory.UsedPercent = getF32(data[106:])
	// Swap
	s.Swap.Total = binary.LittleEndian.Uint64(data[110:])
	s.Swap.Free = binary.LittleEndian.Uint64(data[118:])
	s.Swap.Used = binary.LittleEndian.Uint64(data[126:])
	s.Swap.UsedPercent = getF32(data[134:])
	// TCP + sockets
	s.Network.TCP.CurrEstab = binary.LittleEndian.Uint64(data[138:])
	s.Network.TCP.InErrs = getF32(data[146:])
	s.Network.TCP.OutRsts = getF32(data[150:])
	s.Network.Sockets.TCPInUse = int(int32(binary.LittleEndian.Uint32(data[154:])))
	s.Network.Sockets.TCPTw = int(int32(binary.LittleEndian.Uint32(data[158:])))
	s.Network.Sockets.UDPInUse = int(int32(binary.LittleEndian.Uint32(data[162:])))
	// Process
	s.Process.Total = int(int32(binary.LittleEndian.Uint32(data[166:])))
	s.Process.Running = int(int32(binary.LittleEndian.Uint32(data[170:])))
	s.Process.Sleeping = int(int32(binary.LittleEndian.Uint32(data[174:])))
	s.Process.Zombie = int(int32(binary.LittleEndian.Uint32(data[178:])))
	s.Process.Blocked = int(int32(binary.LittleEndian.Uint32(data[182:])))
	s.Process.Threads = int(int32(binary.LittleEndian.Uint32(data[186:])))
	// System
	s.System.Uptime = math.Float64frombits(binary.LittleEndian.Uint64(data[190:]))
	s.System.Entropy = int(int32(binary.LittleEndian.Uint32(data[198:])))
	s.System.UserCount = int(data[202])
	s.System.ClockSync = data[203] != 0
	s.System.UptimeHuman = collector.FormatUptime(s.System.Uptime)
	// Self
	s.Self.MemRSS = binary.LittleEndian.Uint64(data[204:])
	s.Self.CPUPercent = getF32(data[212:])
	s.Self.FDs = int(binary.LittleEndian.Uint16(data[216:]))
	return s, fixedBlockSize, nil
}

// decodeVariable decodes the variable-length sections into s.
// Returns the number of bytes consumed and any error.
func decodeVariable(data []byte, s *collector.Sample) (int, error) {
	off := 0

	need := func(n int, ctx string) error {
		if len(data)-off < n {
			return fmt.Errorf("variable block truncated at %s (need %d, have %d)", ctx, n, len(data)-off)
		}
		return nil
	}

	// Network interfaces
	if err := need(2, "iface count"); err != nil {
		return off, err
	}
	numIfaces := int(binary.LittleEndian.Uint16(data[off:]))
	off += 2
	s.Network.Interfaces = make([]collector.NetInterface, 0, numIfaces)
	for i := 0; i < numIfaces; i++ {
		var iface collector.NetInterface
		name, n, err := getStr(data[off:])
		if err != nil {
			return off, fmt.Errorf("iface name: %w", err)
		}
		off += n
		iface.Name = name
		if err := need(4*4+8*8, "iface fields"); err != nil {
			return off, err
		}
		iface.RxMbps = getF32(data[off:]); off += 4
		iface.TxMbps = getF32(data[off:]); off += 4
		iface.RxPPS = getF32(data[off:]); off += 4
		iface.TxPPS = getF32(data[off:]); off += 4
		iface.RxBytes = binary.LittleEndian.Uint64(data[off:]); off += 8
		iface.TxBytes = binary.LittleEndian.Uint64(data[off:]); off += 8
		iface.RxPkts = binary.LittleEndian.Uint64(data[off:]); off += 8
		iface.TxPkts = binary.LittleEndian.Uint64(data[off:]); off += 8
		iface.RxErrs = binary.LittleEndian.Uint64(data[off:]); off += 8
		iface.TxErrs = binary.LittleEndian.Uint64(data[off:]); off += 8
		iface.RxDrop = binary.LittleEndian.Uint64(data[off:]); off += 8
		iface.TxDrop = binary.LittleEndian.Uint64(data[off:]); off += 8
		s.Network.Interfaces = append(s.Network.Interfaces, iface)
	}

	// CPU temperature sensors
	if err := need(2, "cpu sensor count"); err != nil {
		return off, err
	}
	numSensors := int(binary.LittleEndian.Uint16(data[off:]))
	off += 2
	s.CPU.Sensors = make([]collector.CPUTempSensor, 0, numSensors)
	for i := 0; i < numSensors; i++ {
		name, n, err := getStr(data[off:])
		if err != nil {
			return off, fmt.Errorf("cpu sensor name: %w", err)
		}
		off += n
		if err := need(4, "cpu sensor value"); err != nil {
			return off, err
		}
		val := getF32(data[off:]); off += 4
		s.CPU.Sensors = append(s.CPU.Sensors, collector.CPUTempSensor{Name: name, Value: val})
	}

	// Disk devices
	if err := need(2, "disk count"); err != nil {
		return off, err
	}
	numDisks := int(binary.LittleEndian.Uint16(data[off:]))
	off += 2
	s.Disks.Devices = make([]collector.DiskDevice, 0, numDisks)
	for i := 0; i < numDisks; i++ {
		var dev collector.DiskDevice
		name, n, err := getStr(data[off:])
		if err != nil {
			return off, fmt.Errorf("disk name: %w", err)
		}
		off += n
		dev.Name = name
		if err := need(6*4, "disk scalar fields"); err != nil {
			return off, err
		}
		dev.ReadsPerSec = getF32(data[off:]); off += 4
		dev.WritesPerSec = getF32(data[off:]); off += 4
		dev.ReadBytesPS = getF32(data[off:]); off += 4
		dev.WriteBytesPS = getF32(data[off:]); off += 4
		dev.Utilization = getF32(data[off:]); off += 4
		dev.Temperature = getF32(data[off:]); off += 4
		if err := need(2, "disk sensor count"); err != nil {
			return off, err
		}
		numTS := int(binary.LittleEndian.Uint16(data[off:]))
		off += 2
		dev.Sensors = make([]collector.DiskTempSensor, 0, numTS)
		for j := 0; j < numTS; j++ {
			tsName, tn, err := getStr(data[off:])
			if err != nil {
				return off, fmt.Errorf("disk sensor name: %w", err)
			}
			off += tn
			if err := need(4, "disk sensor value"); err != nil {
				return off, err
			}
			tsVal := getF32(data[off:]); off += 4
			dev.Sensors = append(dev.Sensors, collector.DiskTempSensor{Name: tsName, Value: tsVal})
		}
		s.Disks.Devices = append(s.Disks.Devices, dev)
	}

	// Filesystems
	if err := need(2, "fs count"); err != nil {
		return off, err
	}
	numFS := int(binary.LittleEndian.Uint16(data[off:]))
	off += 2
	s.Disks.FileSystems = make([]collector.FileSystemInfo, 0, numFS)
	for i := 0; i < numFS; i++ {
		var fs collector.FileSystemInfo
		dev, n, err := getStr(data[off:])
		if err != nil {
			return off, fmt.Errorf("fs device: %w", err)
		}
		off += n
		fs.Device = dev
		mp, n2, err := getStr(data[off:])
		if err != nil {
			return off, fmt.Errorf("fs mountpoint: %w", err)
		}
		off += n2
		fs.MountPoint = mp
		ft, n3, err := getStr(data[off:])
		if err != nil {
			return off, fmt.Errorf("fs type: %w", err)
		}
		off += n3
		fs.FSType = ft
		if err := need(3*8+4, "fs numeric fields"); err != nil {
			return off, err
		}
		fs.Total = binary.LittleEndian.Uint64(data[off:]); off += 8
		fs.Used = binary.LittleEndian.Uint64(data[off:]); off += 8
		fs.Available = binary.LittleEndian.Uint64(data[off:]); off += 8
		fs.UsedPct = getF32(data[off:]); off += 4
		s.Disks.FileSystems = append(s.Disks.FileSystems, fs)
	}

	// System strings
	hostname, n, err := getStr(data[off:])
	if err != nil {
		return off, fmt.Errorf("hostname: %w", err)
	}
	off += n
	s.System.Hostname = hostname
	clockSrc, n2, err := getStr(data[off:])
	if err != nil {
		return off, fmt.Errorf("clock source: %w", err)
	}
	off += n2
	s.System.ClockSource = clockSrc

	// GPU entries
	if len(data)-off < 2 {
		// GPU section absent in older v2 records (graceful forward compat)
		return off, nil
	}
	numGPU := int(binary.LittleEndian.Uint16(data[off:]))
	off += 2
	s.GPU = make([]collector.GPUStats, 0, numGPU)
	for i := 0; i < numGPU; i++ {
		var g collector.GPUStats
		if err := need(2, "gpu index"); err != nil {
			return off, err
		}
		g.Index = int(binary.LittleEndian.Uint16(data[off:])); off += 2
		name, gn, err := getStr(data[off:])
		if err != nil {
			return off, fmt.Errorf("gpu name: %w", err)
		}
		off += gn
		g.Name = name
		drv, dn, err := getStr(data[off:])
		if err != nil {
			return off, fmt.Errorf("gpu driver: %w", err)
		}
		off += dn
		g.Driver = drv
		if err := need(4+8+8+4+4+4, "gpu scalar fields"); err != nil {
			return off, err
		}
		g.Temperature = getF32(data[off:]); off += 4
		g.VRAMUsed = binary.LittleEndian.Uint64(data[off:]); off += 8
		g.VRAMTotal = binary.LittleEndian.Uint64(data[off:]); off += 8
		g.VRAMUsedPct = getF32(data[off:]); off += 4
		g.LoadPct = getF32(data[off:]); off += 4
		g.PowerW = getF32(data[off:]); off += 4
		s.GPU = append(s.GPU, g)
	}

	return off, nil
}

// ---- Timestamp extraction ---------------------------------------------------

// extractTimestamp reads the timestamp from a binary payload (no length prefix).
// Three record formats are handled:
//
//   - recordKindBinary (0x02): kind-tagged binary — timestamp at data[1:9]
//   - '{' (0x7B):              legacy JSON — returns error to trigger full decode
//   - anything else:           legacy binary (no kind byte) — timestamp at data[0:8]
func extractTimestamp(data []byte) (time.Time, error) {
	if len(data) < 1 {
		return time.Time{}, fmt.Errorf("empty record")
	}
	switch data[0] {
	case recordKindBinary:
		if len(data) < 9 {
			return time.Time{}, fmt.Errorf("binary record too short for timestamp: %d bytes", len(data))
		}
		return time.Unix(0, int64(binary.LittleEndian.Uint64(data[1:9]))), nil
	case '{':
		// FAST-PATH: Extract JSON timestamp manually instead of triggering a full json.Unmarshal
		idx := bytes.Index(data, []byte(`"ts":"`))
		if idx == -1 {
			return time.Time{}, fmt.Errorf("JSON record: missing ts field")
		}
		start := idx + 6
		end := bytes.IndexByte(data[start:], '"')
		if end == -1 {
			return time.Time{}, fmt.Errorf("JSON record: malformed ts field")
		}
		// Parse the extracted string natively
		return time.Parse(time.RFC3339Nano, string(data[start:start+end]))
	default:
		// Legacy binary record written before the kind-byte format.
		if len(data) < 8 {
			return time.Time{}, fmt.Errorf("legacy record too short for timestamp: %d bytes", len(data))
		}
		return time.Unix(0, int64(binary.LittleEndian.Uint64(data[0:8]))), nil
	}
}

// ---- Version dispatch -------------------------------------------------------

// encodeSampleV encodes a using the binary v2 codec and prepends the
// recordKindBinary byte so the returned slice matches the on-disk payload
// format exactly: [kind][preamble][fixed][variable...].
// This keeps the allocation count in Write at one (encodeSample's output copy)
// rather than two (output copy + kind-byte prefix copy).
func encodeSampleV(a *AggregatedSample) ([]byte, error) {
	payload, err := encodeSample(a)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 1+len(payload))
	out[0] = recordKindBinary
	copy(out[1:], payload)
	return out, nil
}

// decodeSampleJSON is the legacy JSON decoder retained for transparent
// migration of existing tier files (codec version 1).
func decodeSampleJSON(data []byte) (*AggregatedSample, error) {
	s := &AggregatedSample{}
	err := json.Unmarshal(data, s)
	return s, err
}

