package storage

// ---- Codec backward-compatibility rules ------------------------------------
//
// The binary codec must read records written by ALL previous versions of Kula
// that may still exist in tier files. Violating this silently drops historical
// data from graphs. Follow these rules when changing the format:
//
//  1. NEVER resize an existing block in-place. Old records on disk already
//     contain the old size and the decoder will read garbage or corrupt the
//     offset for subsequent sections.
//
//  2. To add fields to an application metrics section (nginx, postgres, etc.),
//     use the presence byte as a VERSION TAG:
//       - Existing version (e.g. 1) keeps the old block size.
//       - Bump the presence byte to N+1 and write the new, larger block.
//       - The decoder must branch: if version==1 → read old layout;
//         if version>=2 → read new layout. Zero-init fields absent in old.
//     See the PostgreSQL section for a worked example (v1=56B, v2=104B).
//
//  3. To add an entirely new application section, append it AFTER the custom
//     metrics section and gate it behind a new preamble flag bit
//     (like flagHasApps gates the whole apps section). Old records without
//     the flag will not attempt to read the new section.
//
//  4. NEVER remove or reorder fields inside an existing version block.
//     Deprecate by keeping the field, writing zero, and ignoring it on read.
//
//  5. After any format change, update ALL of:
//       - appendVariable()  (encoder)
//       - decodeVariable()  (decoder, with version branches)
//       - store.go           (aggregation of new rate fields)
//       - addons/inspect_tier.py (Python decoder, with version branches)
//       - codec_test.go      (add a test that decodes the OLD format)
//
//  6. Always write a regression test that constructs a binary payload in the
//     OLD format and verifies decodeSample handles it. See
//     TestDecodePostgresV1Block and TestDecodeOldAggregatedRecord.
// ----------------------------------------------------------------------------

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"kula/internal/collector"
	"math"
	"sort"
	"sync"
	"time"
)

// Flags packed into the 2-byte preamble flags field.
const (
	flagHasMin     uint16 = 1 << 0
	flagHasMax     uint16 = 1 << 1
	flagHasData    uint16 = 1 << 2
	flagHasApps    uint16 = 1 << 3 // variable block includes application metrics section
	flagHasApache2 uint16 = 1 << 8 // variable block includes Apache2 metrics
	flagHasMysql   uint16 = 1 << 9 // variable block includes MySQL metrics
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
// For tier 0 (raw), this is just a wrapper around the raw sample.
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
// the hot write path. Each pool entry is a *[]byte pointing to a slice
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
//	[16:18] flags        uint16 LE (flagHasData/Min/Max/Apps)
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
	flags |= flagHasApps    // always set: variable blocks include app metrics section
	flags |= flagHasApache2 // always set: Apache2 metrics byte follows MySQL
	flags |= flagHasMysql   // always set: MySQL metrics byte follows Postgres
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
	run := s.LoadAvg.Running
	if run < 0 { run = 0 } else if run > 65535 { run = 65535 }
	binary.LittleEndian.PutUint16(b[46:], uint16(run))
	tot := s.LoadAvg.Total
	if tot < 0 { tot = 0 } else if tot > 65535 { tot = 65535 }
	binary.LittleEndian.PutUint16(b[48:], uint16(tot))
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
	ent := s.System.Entropy
	if ent < 0 { ent = 0 } else if ent > math.MaxInt32 { ent = math.MaxInt32 }
	binary.LittleEndian.PutUint32(b[198:], uint32(int32(ent)))
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

	// ---- Application metrics ----

	// Nginx (1-byte presence + 52-byte fixed block when present)
	if s.Apps.Nginx != nil {
		buf = append(buf, 1)
		n := s.Apps.Nginx
		var nb [52]byte
		binary.LittleEndian.PutUint32(nb[0:], uint32(int32(n.ActiveConnections)))
		binary.LittleEndian.PutUint64(nb[4:], n.Accepts)
		binary.LittleEndian.PutUint64(nb[12:], n.Handled)
		binary.LittleEndian.PutUint64(nb[20:], n.Requests)
		putF32(nb[28:], n.AcceptsPS)
		putF32(nb[32:], n.HandledPS)
		putF32(nb[36:], n.RequestsPS)
		binary.LittleEndian.PutUint32(nb[40:], uint32(int32(n.Reading)))
		binary.LittleEndian.PutUint32(nb[44:], uint32(int32(n.Writing)))
		binary.LittleEndian.PutUint32(nb[48:], uint32(int32(n.Waiting)))
		buf = append(buf, nb[:]...)
	} else {
		buf = append(buf, 0)
	}

	// Containers (uint16 count + variable per container)
	ctCount := len(s.Apps.Containers)
	if ctCount > 65535 {
		ctCount = 65535
	}
	buf = appendUint16(buf, uint16(ctCount))
	for _, ct := range s.Apps.Containers[:ctCount] {
		if buf, err = appendStr(buf, ct.ID); err != nil {
			return buf, fmt.Errorf("container id: %w", err)
		}
		if buf, err = appendStr(buf, ct.Name); err != nil {
			return buf, fmt.Errorf("container name: %w", err)
		}
		buf = appendF32(buf, ct.CPUPct)
		buf = appendUint64(buf, ct.MemUsed)
		buf = appendUint64(buf, ct.MemLimit)
		buf = appendF32(buf, ct.MemPct)
		buf = appendF32(buf, ct.NetRxBPS)
		buf = appendF32(buf, ct.NetTxBPS)
		buf = appendF32(buf, ct.DiskRBPS)
		buf = appendF32(buf, ct.DiskWBPS)
	}

	// PostgreSQL (1-byte presence + 104-byte fixed block when present)
	//
	// Layout (104 bytes):
	//   [0:20]   ActiveConns, IdleConns, IdleInTxConns, WaitingConns, MaxConns  — 5×int32
	//   [20:28]  TxCommitPS, TxRollbackPS                                       — 2×float32
	//   [28:48]  TupFetchedPS, TupReturnedPS, TupInsertedPS, TupUpdatedPS,
	//            TupDeletedPS                                                    — 5×float32
	//   [48:60]  BlksReadPS, BlksHitPS, BlksHitPct                              — 3×float32
	//   [60:64]  DeadlocksPS                                                    — 1×float32
	//   [64:72]  BufCheckpointPS, BufBackendPS                                  — 2×float32
	//   [72:96]  DeadTuples, LiveTuples, AutovacuumCount                        — 3×int64
	//   [96:104] DBSizeBytes                                                    — 1×int64
	if s.Apps.Postgres != nil {
		buf = append(buf, 2) // version 2: 104-byte block (version 1 was 56-byte)
		pg := s.Apps.Postgres
		var pb [104]byte
		binary.LittleEndian.PutUint32(pb[0:], uint32(int32(pg.ActiveConns)))
		binary.LittleEndian.PutUint32(pb[4:], uint32(int32(pg.IdleConns)))
		binary.LittleEndian.PutUint32(pb[8:], uint32(int32(pg.IdleInTxConns)))
		binary.LittleEndian.PutUint32(pb[12:], uint32(int32(pg.WaitingConns)))
		binary.LittleEndian.PutUint32(pb[16:], uint32(int32(pg.MaxConns)))
		putF32(pb[20:], pg.TxCommitPS)
		putF32(pb[24:], pg.TxRollbackPS)
		putF32(pb[28:], pg.TupFetchedPS)
		putF32(pb[32:], pg.TupReturnedPS)
		putF32(pb[36:], pg.TupInsertedPS)
		putF32(pb[40:], pg.TupUpdatedPS)
		putF32(pb[44:], pg.TupDeletedPS)
		putF32(pb[48:], pg.BlksReadPS)
		putF32(pb[52:], pg.BlksHitPS)
		putF32(pb[56:], pg.BlksHitPct)
		putF32(pb[60:], pg.DeadlocksPS)
		putF32(pb[64:], pg.BufCheckpointPS)
		putF32(pb[68:], pg.BufBackendPS)
		binary.LittleEndian.PutUint64(pb[72:], uint64(pg.DeadTuples))
		binary.LittleEndian.PutUint64(pb[80:], uint64(pg.LiveTuples))
		binary.LittleEndian.PutUint64(pb[88:], uint64(pg.AutovacuumCount))
		binary.LittleEndian.PutUint64(pb[96:], uint64(pg.DBSizeBytes))
		buf = append(buf, pb[:]...)
	} else {
		buf = append(buf, 0)
	}

	// MySQL — presence byte doubles as version tag:
	//   0 = not present
	//   1 = v1 format (56-byte block: 4×int32 + 10×float32)
	if s.Apps.Mysql != nil {
		buf = append(buf, 1)
		my := s.Apps.Mysql
		var mb [56]byte
		binary.LittleEndian.PutUint32(mb[0:], uint32(int32(my.ThreadsConnected)))
		binary.LittleEndian.PutUint32(mb[4:], uint32(int32(my.ThreadsRunning)))
		binary.LittleEndian.PutUint32(mb[8:], uint32(int32(my.ThreadsCached)))
		binary.LittleEndian.PutUint32(mb[12:], uint32(int32(my.MaxConnections)))
		putF32(mb[16:], my.QueriesPS)
		putF32(mb[20:], my.ComSelectPS)
		putF32(mb[24:], my.ComInsertPS)
		putF32(mb[28:], my.ComUpdatePS)
		putF32(mb[32:], my.ComDeletePS)
		putF32(mb[36:], my.SlowQueriesPS)
		putF32(mb[40:], my.InnodbBufferPoolHitPct)
		putF32(mb[44:], my.InnodbBPReadsPS)
		putF32(mb[48:], my.TableLocksWaitedPS)
		putF32(mb[52:], my.RowLockWaitsPS)
		buf = append(buf, mb[:]...)
	} else {
		buf = append(buf, 0)
	}

	// Apache2 — presence byte doubles as version tag:
	//   0 = not present
	//   1 = v1 format (72-byte block, pre-0.17.0: BusyWorkers through Keepalive only)
	//   2 = v2 format (100-byte block: adds Starting/DNS/Closing/Logging/Graceful/IdleCleanup/OpenSlots)
	if s.Apps.Apache2 != nil {
		buf = append(buf, 2)
		a := s.Apps.Apache2
		var ab [100]byte
		binary.LittleEndian.PutUint32(ab[0:], uint32(int32(a.BusyWorkers)))
		binary.LittleEndian.PutUint32(ab[4:], uint32(int32(a.IdleWorkers)))
		binary.LittleEndian.PutUint64(ab[8:], a.TotalAccesses)
		binary.LittleEndian.PutUint64(ab[16:], a.TotalKBytes)
		putF32(ab[24:], a.AccessesPS)
		putF32(ab[28:], a.KBytesPS)
		putF32(ab[32:], a.ReqPerSec)
		putF32(ab[36:], a.BytesPerSec)
		putF32(ab[40:], a.BytesPerReq)
		putF32(ab[44:], a.CPULoad)
		binary.LittleEndian.PutUint64(ab[48:], uint64(a.Uptime))
		binary.LittleEndian.PutUint32(ab[56:], uint32(int32(a.Waiting)))
		binary.LittleEndian.PutUint32(ab[60:], uint32(int32(a.Reading)))
		binary.LittleEndian.PutUint32(ab[64:], uint32(int32(a.Sending)))
		binary.LittleEndian.PutUint32(ab[68:], uint32(int32(a.Keepalive)))
		binary.LittleEndian.PutUint32(ab[72:], uint32(int32(a.Starting)))
		binary.LittleEndian.PutUint32(ab[76:], uint32(int32(a.DNS)))
		binary.LittleEndian.PutUint32(ab[80:], uint32(int32(a.Closing)))
		binary.LittleEndian.PutUint32(ab[84:], uint32(int32(a.Logging)))
		binary.LittleEndian.PutUint32(ab[88:], uint32(int32(a.Graceful)))
		binary.LittleEndian.PutUint32(ab[92:], uint32(int32(a.IdleCleanup)))
		binary.LittleEndian.PutUint32(ab[96:], uint32(int32(a.OpenSlots)))
		buf = append(buf, ab[:]...)
	} else {
		buf = append(buf, 0)
	}

	// Custom metrics (uint16 group count, sorted keys for deterministic encoding)
	customKeys := make([]string, 0, len(s.Apps.Custom))
	for k := range s.Apps.Custom {
		customKeys = append(customKeys, k)
	}
	sort.Strings(customKeys)
	buf = appendUint16(buf, uint16(len(customKeys)))
	for _, group := range customKeys {
		metrics := s.Apps.Custom[group]
		if buf, err = appendStr(buf, group); err != nil {
			return buf, fmt.Errorf("custom group name: %w", err)
		}
		mCount := len(metrics)
		if mCount > 65535 {
			mCount = 65535
		}
		buf = appendUint16(buf, uint16(mCount))
		for _, m := range metrics[:mCount] {
			if buf, err = appendStr(buf, m.Name); err != nil {
				return buf, fmt.Errorf("custom metric name: %w", err)
			}
			buf = appendF32(buf, m.Value)
		}
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
	hasApps := flags&flagHasApps != 0
	hasApache2 := flags&flagHasApache2 != 0
	hasMysql := flags&flagHasMysql != 0
	off := 18

	if flags&flagHasData != 0 {
		s, n, err := decodeFixed(data[off:])
		if err != nil {
			return nil, fmt.Errorf("decode data fixed: %w", err)
		}
		off += n
		vn, err := decodeVariable(data[off:], s, hasApps, hasApache2, hasMysql)
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
		vn, err := decodeVariable(data[off:], s, hasApps, hasApache2, hasMysql)
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
		vn, err := decodeVariable(data[off:], s, hasApps, hasApache2, hasMysql)
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
// hasApps indicates the variable block includes the application metrics section
// (flagHasApps was set in the preamble). Old records without the flag must not
// attempt to decode app metrics because the remaining bytes belong to the next
// fixed+variable block (min/max) in multi-block aggregated records.
// hasApache2 gates the Apache2 presence byte (flagHasApache2).
// hasMysql gates the MySQL presence byte (flagHasMysql).
// Records written before each type existed omit the flag, so the decoder
// skips that section's bytes and subsequent offsets stay correct.
// Returns the number of bytes consumed and any error.
func decodeVariable(data []byte, s *collector.Sample, hasApps, hasApache2, hasMysql bool) (int, error) {
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

	// ---- Application metrics ----
	// Old records (pre-flagHasApps) do not contain this section.  In multi-block
	// aggregated records the remaining bytes after section 6 belong to the next
	// fixed+variable block (min/max), not to app metrics.
	if !hasApps {
		return off, nil
	}

	// Nginx
	nginxPresent := data[off]; off++
	if nginxPresent != 0 {
		if err := need(52, "nginx fields"); err != nil {
			return off, err
		}
		ng := &collector.NginxStats{}
		ng.ActiveConnections = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
		ng.Accepts = binary.LittleEndian.Uint64(data[off:]); off += 8
		ng.Handled = binary.LittleEndian.Uint64(data[off:]); off += 8
		ng.Requests = binary.LittleEndian.Uint64(data[off:]); off += 8
		ng.AcceptsPS = getF32(data[off:]); off += 4
		ng.HandledPS = getF32(data[off:]); off += 4
		ng.RequestsPS = getF32(data[off:]); off += 4
		ng.Reading = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
		ng.Writing = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
		ng.Waiting = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
		s.Apps.Nginx = ng
	}

	// Containers
	if err := need(2, "container count"); err != nil {
		return off, err
	}
	numContainers := int(binary.LittleEndian.Uint16(data[off:])); off += 2
	if numContainers > 0 {
		s.Apps.Containers = make([]collector.ContainerStats, 0, numContainers)
	}
	for i := 0; i < numContainers; i++ {
		var ct collector.ContainerStats
		id, n, err := getStr(data[off:])
		if err != nil {
			return off, fmt.Errorf("container id: %w", err)
		}
		off += n
		ct.ID = id
		ctName, n2, err := getStr(data[off:])
		if err != nil {
			return off, fmt.Errorf("container name: %w", err)
		}
		off += n2
		ct.Name = ctName
		if err := need(4+8+8+4*5, "container fields"); err != nil {
			return off, err
		}
		ct.CPUPct = getF32(data[off:]); off += 4
		ct.MemUsed = binary.LittleEndian.Uint64(data[off:]); off += 8
		ct.MemLimit = binary.LittleEndian.Uint64(data[off:]); off += 8
		ct.MemPct = getF32(data[off:]); off += 4
		ct.NetRxBPS = getF32(data[off:]); off += 4
		ct.NetTxBPS = getF32(data[off:]); off += 4
		ct.DiskRBPS = getF32(data[off:]); off += 4
		ct.DiskWBPS = getF32(data[off:]); off += 4
		s.Apps.Containers = append(s.Apps.Containers, ct)
	}

	// PostgreSQL — presence byte doubles as version tag:
	//   0 = not present
	//   1 = v1 format (56-byte block: 3×int32 + 7×float32 + 2×int64)
	//   2 = v2 format (104-byte block: 5×int32 + 13×float32 + 4×int64)
	if err := need(1, "postgres presence"); err != nil {
		return off, err
	}
	pgVersion := data[off]; off++
	if pgVersion == 1 {
		// v1: old 56-byte block (ActiveConns, IdleConns, MaxConns,
		//   TxCommitPS, TxRollbackPS, TupFetchedPS, TupInsertedPS,
		//   TupUpdatedPS, TupDeletedPS, BlksHitPct, DeadTuples, DBSizeBytes)
		if err := need(56, "postgres v1 fields"); err != nil {
			return off, err
		}
		pg := &collector.PostgresStats{}
		pg.ActiveConns  = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
		pg.IdleConns    = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
		pg.MaxConns     = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
		pg.TxCommitPS   = getF32(data[off:]); off += 4
		pg.TxRollbackPS = getF32(data[off:]); off += 4
		pg.TupFetchedPS = getF32(data[off:]); off += 4
		pg.TupInsertedPS = getF32(data[off:]); off += 4
		pg.TupUpdatedPS = getF32(data[off:]); off += 4
		pg.TupDeletedPS = getF32(data[off:]); off += 4
		pg.BlksHitPct   = getF32(data[off:]); off += 4
		pg.DeadTuples   = int64(binary.LittleEndian.Uint64(data[off:])); off += 8
		pg.DBSizeBytes  = int64(binary.LittleEndian.Uint64(data[off:])); off += 8
		s.Apps.Postgres = pg
	} else if pgVersion >= 2 {
		// v2: 104-byte block with full metrics
		if err := need(104, "postgres v2 fields"); err != nil {
			return off, err
		}
		pg := &collector.PostgresStats{}
		pg.ActiveConns    = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
		pg.IdleConns      = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
		pg.IdleInTxConns  = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
		pg.WaitingConns   = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
		pg.MaxConns       = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
		pg.TxCommitPS     = getF32(data[off:]); off += 4
		pg.TxRollbackPS   = getF32(data[off:]); off += 4
		pg.TupFetchedPS   = getF32(data[off:]); off += 4
		pg.TupReturnedPS  = getF32(data[off:]); off += 4
		pg.TupInsertedPS  = getF32(data[off:]); off += 4
		pg.TupUpdatedPS   = getF32(data[off:]); off += 4
		pg.TupDeletedPS   = getF32(data[off:]); off += 4
		pg.BlksReadPS     = getF32(data[off:]); off += 4
		pg.BlksHitPS      = getF32(data[off:]); off += 4
		pg.BlksHitPct     = getF32(data[off:]); off += 4
		pg.DeadlocksPS    = getF32(data[off:]); off += 4
		pg.BufCheckpointPS = getF32(data[off:]); off += 4
		pg.BufBackendPS   = getF32(data[off:]); off += 4
		pg.DeadTuples     = int64(binary.LittleEndian.Uint64(data[off:])); off += 8
		pg.LiveTuples     = int64(binary.LittleEndian.Uint64(data[off:])); off += 8
		pg.AutovacuumCount = int64(binary.LittleEndian.Uint64(data[off:])); off += 8
		pg.DBSizeBytes    = int64(binary.LittleEndian.Uint64(data[off:])); off += 8
		s.Apps.Postgres = pg
	}

	// MySQL — gated by flagHasMysql so old records (pre-0.17.0) that
	// lack this flag skip the byte and continue to Apache2 or Custom.
	if hasMysql {
		if err := need(1, "mysql presence"); err != nil {
			return off, err
		}
		myVersion := data[off]; off++
		if myVersion >= 1 {
			if err := need(56, "mysql v1 fields"); err != nil {
				return off, err
			}
			my := &collector.MysqlStats{}
			my.ThreadsConnected = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			my.ThreadsRunning   = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			my.ThreadsCached    = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			my.MaxConnections   = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			my.QueriesPS       = getF32(data[off:]); off += 4
			my.ComSelectPS     = getF32(data[off:]); off += 4
			my.ComInsertPS     = getF32(data[off:]); off += 4
			my.ComUpdatePS     = getF32(data[off:]); off += 4
			my.ComDeletePS     = getF32(data[off:]); off += 4
			my.SlowQueriesPS   = getF32(data[off:]); off += 4
			my.InnodbBufferPoolHitPct = getF32(data[off:]); off += 4
			my.InnodbBPReadsPS = getF32(data[off:]); off += 4
			my.TableLocksWaitedPS = getF32(data[off:]); off += 4
			my.RowLockWaitsPS  = getF32(data[off:]); off += 4
			s.Apps.Mysql = my
		}
	}

	// Apache2 — gated by flagHasApache2 so old records (pre-0.16.0) that
	// lack this flag skip the byte and continue to custom metrics.
	if hasApache2 {
		apache2Version := data[off]; off++
		switch apache2Version {
		case 1:
			if err := need(72, "apache2 v1 fields"); err != nil {
				return off, err
			}
			ap := &collector.Apache2Stats{}
			ap.BusyWorkers = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			ap.IdleWorkers = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			ap.TotalAccesses = binary.LittleEndian.Uint64(data[off:]); off += 8
			ap.TotalKBytes = binary.LittleEndian.Uint64(data[off:]); off += 8
			ap.AccessesPS = getF32(data[off:]); off += 4
			ap.KBytesPS = getF32(data[off:]); off += 4
			ap.ReqPerSec = getF32(data[off:]); off += 4
			ap.BytesPerSec = getF32(data[off:]); off += 4
			ap.BytesPerReq = getF32(data[off:]); off += 4
			ap.CPULoad = getF32(data[off:]); off += 4
			ap.Uptime = int64(binary.LittleEndian.Uint64(data[off:])); off += 8
			ap.Waiting = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			ap.Reading = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			ap.Sending = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			ap.Keepalive = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			s.Apps.Apache2 = ap
		case 2:
			if err := need(100, "apache2 v2 fields"); err != nil {
				return off, err
			}
			ap := &collector.Apache2Stats{}
			ap.BusyWorkers = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			ap.IdleWorkers = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			ap.TotalAccesses = binary.LittleEndian.Uint64(data[off:]); off += 8
			ap.TotalKBytes = binary.LittleEndian.Uint64(data[off:]); off += 8
			ap.AccessesPS = getF32(data[off:]); off += 4
			ap.KBytesPS = getF32(data[off:]); off += 4
			ap.ReqPerSec = getF32(data[off:]); off += 4
			ap.BytesPerSec = getF32(data[off:]); off += 4
			ap.BytesPerReq = getF32(data[off:]); off += 4
			ap.CPULoad = getF32(data[off:]); off += 4
			ap.Uptime = int64(binary.LittleEndian.Uint64(data[off:])); off += 8
			ap.Waiting = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			ap.Reading = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			ap.Sending = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			ap.Keepalive = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			ap.Starting = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			ap.DNS = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			ap.Closing = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			ap.Logging = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			ap.Graceful = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			ap.IdleCleanup = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			ap.OpenSlots = int(int32(binary.LittleEndian.Uint32(data[off:]))); off += 4
			s.Apps.Apache2 = ap
		}
	}

	// Custom metrics
	if err := need(2, "custom group count"); err != nil {
		return off, err
	}
	numGroups := int(binary.LittleEndian.Uint16(data[off:])); off += 2
	if numGroups > 0 {
		s.Apps.Custom = make(map[string][]collector.CustomMetricValue, numGroups)
	}
	for i := 0; i < numGroups; i++ {
		groupName, gn, err := getStr(data[off:])
		if err != nil {
			return off, fmt.Errorf("custom group name: %w", err)
		}
		off += gn
		if err := need(2, "custom metric count"); err != nil {
			return off, err
		}
		mCount := int(binary.LittleEndian.Uint16(data[off:])); off += 2
		metrics := make([]collector.CustomMetricValue, 0, mCount)
		for j := 0; j < mCount; j++ {
			mName, mn, err := getStr(data[off:])
			if err != nil {
				return off, fmt.Errorf("custom metric name: %w", err)
			}
			off += mn
			if err := need(4, "custom metric value"); err != nil {
				return off, err
			}
			val := getF32(data[off:]); off += 4
			metrics = append(metrics, collector.CustomMetricValue{Name: mName, Value: val})
		}
		s.Apps.Custom[groupName] = metrics
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

