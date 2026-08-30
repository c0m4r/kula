package storage

import (
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"

	"kula/internal/collector"
)

// ---- helpers ----------------------------------------------------------------

func makeSampleFull(ts time.Time) *AggregatedSample {
	return &AggregatedSample{
		Timestamp: ts,
		Duration:  time.Second,
		Data: &collector.Sample{
			Timestamp: ts,
			CPU: collector.CPUStats{
				Total: collector.CPUCoreStats{
					User:   25.5,
					System: 10.2,
					Usage:  35.7,
				},
				NumCores: 8,
			},
			LoadAvg: collector.LoadAvg{
				Load1:  1.5,
				Load5:  1.2,
				Load15: 0.8,
			},
			Memory: collector.MemoryStats{
				Total:       16 * 1024 * 1024 * 1024,
				Used:        8 * 1024 * 1024 * 1024,
				Free:        4 * 1024 * 1024 * 1024,
				Shmem:       512 * 1024 * 1024,
				UsedPercent: 50.0,
			},
			Network: collector.NetworkStats{
				Interfaces: []collector.NetInterface{
					{Name: "eth0", RxMbps: 1.5, TxMbps: 0.3},
				},
				TCP:     collector.TCPStats{CurrEstab: 42, InErrs: 0.1, OutRsts: 0.5},
				Sockets: collector.SocketStats{TCPInUse: 42, UDPInUse: 5, TCPTw: 3},
			},
			System: collector.SystemStats{
				Hostname:    "test-host",
				Entropy:     256,
				ClockSource: "tsc",
				ClockSync:   true,
				UserCount:   2,
			},
		},
	}
}

// ---- TestEncodeDecode -------------------------------------------------------

func TestEncodeDecode(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	original := makeSampleFull(now)

	encoded, err := encodeSample(original)
	if err != nil {
		t.Fatalf("encodeSample() error: %v", err)
	}
	if len(encoded) == 0 {
		t.Fatal("encodeSample() returned empty data")
	}

	decoded, err := decodeSample(encoded)
	if err != nil {
		t.Fatalf("decodeSample() error: %v", err)
	}

	if !decoded.Timestamp.Equal(original.Timestamp) {
		t.Errorf("Timestamp = %v, want %v", decoded.Timestamp, original.Timestamp)
	}
	if decoded.Duration != original.Duration {
		t.Errorf("Duration = %v, want %v", decoded.Duration, original.Duration)
	}
	if decoded.Data == nil {
		t.Fatal("Decoded Data is nil")
	}
	// float32 round-trip: allow 0.01 epsilon due to float64→float32 narrowing.
	if diff := decoded.Data.CPU.Total.Usage - original.Data.CPU.Total.Usage; diff > 0.01 || diff < -0.01 {
		t.Errorf("CPU Usage = %f, want ~%f", decoded.Data.CPU.Total.Usage, original.Data.CPU.Total.Usage)
	}
	if decoded.Data.CPU.NumCores != original.Data.CPU.NumCores {
		t.Errorf("NumCores = %d, want %d", decoded.Data.CPU.NumCores, original.Data.CPU.NumCores)
	}
	if decoded.Data.System.Hostname != "test-host" {
		t.Errorf("Hostname = %q, want \"test-host\"", decoded.Data.System.Hostname)
	}
	if decoded.Data.Memory.Total != original.Data.Memory.Total {
		t.Errorf("Memory Total = %d, want %d", decoded.Data.Memory.Total, original.Data.Memory.Total)
	}
	if decoded.Data.Memory.Shmem != original.Data.Memory.Shmem {
		t.Errorf("Memory Shmem = %d, want %d", decoded.Data.Memory.Shmem, original.Data.Memory.Shmem)
	}
	// Network TCP stats survive round-trip
	if decoded.Data.Network.TCP.CurrEstab != original.Data.Network.TCP.CurrEstab {
		t.Errorf("TCP.CurrEstab = %d, want %d",
			decoded.Data.Network.TCP.CurrEstab,
			original.Data.Network.TCP.CurrEstab)
	}
}

func TestEncodeDecodeRoundTripTimestamp(t *testing.T) {
	// Binary codec stores raw UnixNano — nanosecond precision is exact.
	ts := time.Date(2026, 3, 4, 12, 30, 0, 123456789, time.UTC)
	s := &AggregatedSample{
		Timestamp: ts,
		Duration:  time.Second,
		Data:      &collector.Sample{Timestamp: ts},
	}
	enc, err := encodeSample(s)
	if err != nil {
		t.Fatalf("encodeSample: %v", err)
	}
	dec, err := decodeSample(enc)
	if err != nil {
		t.Fatalf("decodeSample: %v", err)
	}
	if !dec.Timestamp.Equal(ts) {
		t.Errorf("Timestamp mismatch: got %v, want %v", dec.Timestamp, ts)
	}
}

// ---- TestDecodeInvalid ------------------------------------------------------

func TestDecodeInvalid(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
	}{
		{"empty", []byte{}},
		{"truncated-preamble", []byte{0x01, 0x02, 0x03}}, // < 18 bytes
		// Preamble OK with flagHasData set, but no fixed block follows.
		{"flagged-no-fixed-block", func() []byte {
			b := make([]byte, 18)
			binary.LittleEndian.PutUint16(b[16:], flagHasData)
			return b
		}()},
		// Preamble OK, flagHasData set, but fixed block is truncated.
		{"truncated-fixed-block", func() []byte {
			b := make([]byte, 18+10) // need 218 bytes for fixed block
			binary.LittleEndian.PutUint16(b[16:], flagHasData)
			return b
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeSample(tc.input)
			if err == nil {
				t.Errorf("decodeSample(%q) expected error, got nil", tc.name)
			}
		})
	}
}

// ---- TestEncodeNilData ------------------------------------------------------

func TestEncodeNilData(t *testing.T) {
	s := &AggregatedSample{
		Timestamp: time.Now(),
		Duration:  time.Second,
		Data:      nil,
	}
	encoded, err := encodeSample(s)
	if err != nil {
		t.Fatalf("encodeSample() with nil Data: %v", err)
	}

	decoded, err := decodeSample(encoded)
	if err != nil {
		t.Fatalf("decodeSample() error: %v", err)
	}
	if decoded.Data != nil {
		t.Error("Decoded Data should be nil")
	}
}

// ---- TestDecodeOldAggregatedRecord ------------------------------------------
// Regression test: records written before flagHasApps was introduced have
// Data+Min+Max blocks without the app metrics section. The decoder must NOT
// attempt to read app metrics from the remaining bytes (which belong to the
// next fixed+variable block), avoiding corrupt reads and silent record skipping.

func TestDecodeOldAggregatedRecord(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	sample := makeSampleFull(now)

	// Encode a single block's variable section (includes 8 bytes of app metrics).
	varBuf, err := appendVariable(nil, sample.Data)
	if err != nil {
		t.Fatalf("appendVariable: %v", err)
	}

	// An empty variable tail has exactly 11 trailing bytes:
	// 1 (nginx=0) + 2 (containers=0) + 1 (postgres=0) + 1 (mysql=0) + 1 (apache2=0) +
	// 2 (custom=0) + 3 (psu version + count=0).
	const emptyTailSize = 11
	oldVarBuf := varBuf[:len(varBuf)-emptyTailSize]

	// Append 218 bytes of "next fixed block" — simulates a min/max block
	// that immediately follows the variable section in aggregated records.
	padded := make([]byte, len(oldVarBuf)+fixedBlockSize)
	copy(padded, oldVarBuf)

	// With hasApps=false, decodeVariable must consume only sections 1-6
	// and NOT touch the trailing 218 bytes.
	target := &collector.Sample{}
	n, err := decodeVariable(padded, target, false, false, false, false)
	if err != nil {
		t.Fatalf("decodeVariable(hasApps=false) error: %v", err)
	}
	if n != len(oldVarBuf) {
		t.Errorf("consumed %d bytes, want %d", n, len(oldVarBuf))
	}
	if target.System.Hostname != "test-host" {
		t.Errorf("Hostname = %q, want %q", target.System.Hostname, "test-host")
	}

	// Full round-trip: build an old-format multi-block record manually.
	fixedBuf := appendFixed(nil, sample.Data)
	var record []byte
	// Preamble: 18 bytes with flagHasData|flagHasMin|flagHasMax (no flagHasApps).
	var preamble [18]byte
	binary.LittleEndian.PutUint64(preamble[0:], uint64(now.UnixNano()))
	binary.LittleEndian.PutUint64(preamble[8:], uint64(time.Second))
	binary.LittleEndian.PutUint16(preamble[16:], flagHasData|flagHasMin|flagHasMax)
	record = append(record, preamble[:]...)
	// Three identical blocks, each without app metrics.
	for range 3 {
		record = append(record, fixedBuf...)
		record = append(record, oldVarBuf...)
	}

	decoded, err := decodeSample(record)
	if err != nil {
		t.Fatalf("decodeSample of old aggregated record: %v", err)
	}
	if decoded.Data == nil || decoded.Min == nil || decoded.Max == nil {
		t.Fatalf("expected Data/Min/Max non-nil, got Data=%v Min=%v Max=%v",
			decoded.Data != nil, decoded.Min != nil, decoded.Max != nil)
	}
	if decoded.Data.System.Hostname != "test-host" {
		t.Errorf("Data.Hostname = %q, want %q", decoded.Data.System.Hostname, "test-host")
	}
	if decoded.Min.System.Hostname != "test-host" {
		t.Errorf("Min.Hostname = %q, want %q", decoded.Min.System.Hostname, "test-host")
	}
}

// ---- TestDecodePostgresV1Block -----------------------------------------------
// Regression test: records written with postgres v1 (56-byte block, presence=1)
// must decode correctly when the current decoder expects v2 (104-byte block).
// The presence byte doubles as a version tag: 1=old, 2=new.

func TestDecodePostgresV1Block(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	sample := makeSampleFull(now)

	// Add postgres data in the v1 layout and encode normally to get variable bytes.
	sample.Data.Apps.Postgres = &collector.PostgresStats{
		ActiveConns:   5,
		IdleConns:     10,
		MaxConns:      100,
		TxCommitPS:    42.5,
		TxRollbackPS:  0.3,
		TupFetchedPS:  100.0,
		TupInsertedPS: 10.0,
		TupUpdatedPS:  5.0,
		TupDeletedPS:  1.0,
		BlksHitPct:    99.5,
		DeadTuples:    500,
		DBSizeBytes:   1024 * 1024 * 1024,
	}
	varBuf, err := appendVariable(nil, sample.Data)
	if err != nil {
		t.Fatalf("appendVariable: %v", err)
	}

	// The encoder writes presence=2 (v2, 104 bytes). Patch it to presence=1
	// and replace with the old 56-byte layout to simulate an old record.
	// Find the postgres presence byte: it's after nginx(1) + containers(2).
	// We need to locate it by building a no-postgres variable section for comparison.
	noPostgres := *sample.Data
	noPostgres.Apps.Postgres = nil
	noPgBuf, err := appendVariable(nil, &noPostgres)
	if err != nil {
		t.Fatalf("appendVariable (no pg): %v", err)
	}
	// In noPgBuf the postgres byte is 0; in varBuf it's 2 followed by 104 bytes.
	// Find the divergence point — that's the postgres presence offset.
	pgOff := -1
	for i := 0; i < len(noPgBuf) && i < len(varBuf); i++ {
		if noPgBuf[i] != varBuf[i] {
			pgOff = i
			break
		}
	}
	if pgOff < 0 {
		t.Fatal("could not find postgres presence byte offset")
	}

	// Build a v1-format variable buffer:
	// everything before postgres + presence=1 + 56 bytes of v1 data + custom section.
	// Note: varBuf now includes the Apache2 byte (between nginx and containers)
	// because appendVariable always writes it. We decode with hasApache2=true
	// so the decoder matches the encoder layout.
	var v1Var []byte
	v1Var = append(v1Var, varBuf[:pgOff]...) // up to postgres presence (includes Apache2 byte)
	v1Var = append(v1Var, 1)                 // v1 presence tag
	// 56-byte v1 block: 3×int32 + 7×float32 + 2×int64
	var pb [56]byte
	binary.LittleEndian.PutUint32(pb[0:], uint32(int32(5)))        // ActiveConns
	binary.LittleEndian.PutUint32(pb[4:], uint32(int32(10)))       // IdleConns
	binary.LittleEndian.PutUint32(pb[8:], uint32(int32(100)))      // MaxConns
	putF32(pb[12:], 42.5)                                          // TxCommitPS
	putF32(pb[16:], 0.3)                                           // TxRollbackPS
	putF32(pb[20:], 100.0)                                         // TupFetchedPS
	putF32(pb[24:], 10.0)                                          // TupInsertedPS
	putF32(pb[28:], 5.0)                                           // TupUpdatedPS
	putF32(pb[32:], 1.0)                                           // TupDeletedPS
	putF32(pb[36:], 99.5)                                          // BlksHitPct
	binary.LittleEndian.PutUint64(pb[40:], uint64(500))            // DeadTuples
	binary.LittleEndian.PutUint64(pb[48:], uint64(1024*1024*1024)) // DBSizeBytes
	v1Var = append(v1Var, pb[:]...)
	// Custom metrics section (empty): from after v2 postgres block to end
	v1Var = append(v1Var, varBuf[pgOff+1+104:]...) // skip v2 presence+block, keep rest

	// Decode the v1-format variable section
	target := &collector.Sample{}
	_, err = decodeVariable(v1Var, target, true, true, false, false)
	if err != nil {
		t.Fatalf("decodeVariable(v1 postgres) error: %v", err)
	}
	pg := target.Apps.Postgres
	if pg == nil {
		t.Fatal("expected Postgres to be non-nil")
	}
	if pg.ActiveConns != 5 || pg.IdleConns != 10 || pg.MaxConns != 100 {
		t.Errorf("connection fields: active=%d idle=%d max=%d", pg.ActiveConns, pg.IdleConns, pg.MaxConns)
	}
	if pg.BlksHitPct < 99.0 {
		t.Errorf("BlksHitPct = %v, want ~99.5", pg.BlksHitPct)
	}
	if pg.DBSizeBytes != 1024*1024*1024 {
		t.Errorf("DBSizeBytes = %d, want %d", pg.DBSizeBytes, 1024*1024*1024)
	}
	// v1 fields that didn't exist should be zero-valued
	if pg.IdleInTxConns != 0 || pg.WaitingConns != 0 || pg.DeadlocksPS != 0 {
		t.Errorf("v2-only fields should be zero: idleTx=%d wait=%d deadlocks=%v",
			pg.IdleInTxConns, pg.WaitingConns, pg.DeadlocksPS)
	}
}

// ---- TestDecodeMysqlV1Block --------------------------------------------------
// Regression test: MySQL v1 block is 56 bytes (4×int32 + 10×float32).
// Construct the binary payload by hand and verify the decoder reads all 10
// floats and leaves the offset at exactly 56 bytes past the presence byte.

func TestDecodeMysqlV1Block(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	sample := makeSampleFull(now)

	// Add MySQL data so appendVariable writes the MySQL section.
	sample.Data.Apps.Mysql = &collector.MysqlStats{
		ThreadsConnected:       5,
		ThreadsRunning:         2,
		ThreadsCached:          3,
		MaxConnections:         151,
		QueriesPS:              42.5,
		ComSelectPS:            20.0,
		ComInsertPS:            5.0,
		ComUpdatePS:            3.0,
		ComDeletePS:            1.0,
		SlowQueriesPS:          0.5,
		InnodbBufferPoolHitPct: 98.7,
		InnodbBPReadsPS:        0.3,
		TableLocksWaitedPS:     0.1,
		RowLockWaitsPS:         0.2,
	}

	// Encode normally — this writes the corrected 56-byte v1 block.
	enc, err := encodeSample(sample)
	if err != nil {
		t.Fatalf("encodeSample: %v", err)
	}

	// Decode and verify all MySQL fields survive the round-trip.
	dec, err := decodeSample(enc)
	if err != nil {
		t.Fatalf("decodeSample: %v", err)
	}
	my := dec.Data.Apps.Mysql
	if my == nil {
		t.Fatal("expected Mysql to be non-nil after round-trip")
	}
	if my.ThreadsConnected != 5 || my.ThreadsRunning != 2 || my.ThreadsCached != 3 {
		t.Errorf("thread fields: connected=%d running=%d cached=%d",
			my.ThreadsConnected, my.ThreadsRunning, my.ThreadsCached)
	}
	if my.MaxConnections != 151 {
		t.Errorf("MaxConnections = %d, want 151", my.MaxConnections)
	}
	if my.QueriesPS < 42.0 || my.QueriesPS > 43.0 {
		t.Errorf("QueriesPS = %v, want ~42.5", my.QueriesPS)
	}
	if my.InnodbBufferPoolHitPct < 98.0 || my.InnodbBufferPoolHitPct > 99.0 {
		t.Errorf("InnodbBufferPoolHitPct = %v, want ~98.7", my.InnodbBufferPoolHitPct)
	}
	// Custom metrics section must still decode correctly after MySQL (alignment check)
	if dec.Data.System.Hostname != "test-host" {
		t.Errorf("Hostname = %q after MySQL decode (alignment check)", dec.Data.System.Hostname)
	}
}

// ---- TestDecodeApache2V1Block ------------------------------------------------
// Regression test: Apache2 v1 block is 72 bytes (the layout before scoreboard
// extended states were added). The current decoder must read v1 correctly and
// leave Starting/DNS/Closing/Logging/Graceful/IdleCleanup/OpenSlots as zero.

func TestDecodeApache2V1Block(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	sample := makeSampleFull(now)

	// Encode with Apache2 data to get a reference variable buffer.
	sample.Data.Apps.Apache2 = &collector.Apache2Stats{
		BusyWorkers:   3,
		IdleWorkers:   7,
		TotalAccesses: 1234,
		TotalKBytes:   5678,
		AccessesPS:    2.5,
		KBytesPS:      10.0,
		ReqPerSec:     2.5,
		BytesPerSec:   8192,
		BytesPerReq:   3276,
		CPULoad:       0.12,
		Uptime:        12345,
		Waiting:       5,
		Reading:       2,
		Sending:       1,
		Keepalive:     0,
		// v2-only fields intentionally left zero
	}
	varBuf, err := appendVariable(nil, sample.Data)
	if err != nil {
		t.Fatalf("appendVariable: %v", err)
	}

	// Find the Apache2 presence byte by encoding without Apache2 and locating divergence.
	noApache := *sample.Data
	noApache.Apps.Apache2 = nil
	noApBuf, err := appendVariable(nil, &noApache)
	if err != nil {
		t.Fatalf("appendVariable (no apache2): %v", err)
	}
	apOff := -1
	for i := 0; i < len(noApBuf) && i < len(varBuf); i++ {
		if noApBuf[i] != varBuf[i] {
			apOff = i
			break
		}
	}
	if apOff < 0 {
		t.Fatal("could not find apache2 presence byte offset")
	}

	// Build a v1-format variable buffer:
	// everything up to Apache2 presence + presence=1 + 72 bytes of v1 data + custom section.
	var v1Var []byte
	v1Var = append(v1Var, varBuf[:apOff]...) // up to apache2 presence
	v1Var = append(v1Var, 1)                 // v1 presence tag
	// 72-byte v1 block: 2×int32 + 2×uint64 + 6×float32 + 1×int64 + 4×int32
	var ab [72]byte
	binary.LittleEndian.PutUint32(ab[0:], uint32(int32(3)))  // BusyWorkers
	binary.LittleEndian.PutUint32(ab[4:], uint32(int32(7)))  // IdleWorkers
	binary.LittleEndian.PutUint64(ab[8:], 1234)              // TotalAccesses
	binary.LittleEndian.PutUint64(ab[16:], 5678)             // TotalKBytes
	putF32(ab[24:], 2.5)                                     // AccessesPS
	putF32(ab[28:], 10.0)                                    // KBytesPS
	putF32(ab[32:], 2.5)                                     // ReqPerSec
	putF32(ab[36:], 8192)                                    // BytesPerSec
	putF32(ab[40:], 3276)                                    // BytesPerReq
	putF32(ab[44:], 0.12)                                    // CPULoad
	binary.LittleEndian.PutUint64(ab[48:], uint64(12345))    // Uptime
	binary.LittleEndian.PutUint32(ab[56:], uint32(int32(5))) // Waiting
	binary.LittleEndian.PutUint32(ab[60:], uint32(int32(2))) // Reading
	binary.LittleEndian.PutUint32(ab[64:], uint32(int32(1))) // Sending
	binary.LittleEndian.PutUint32(ab[68:], uint32(int32(0))) // Keepalive
	v1Var = append(v1Var, ab[:]...)
	// Custom section: from after the v2 apache2 block (presence=2 + 100 bytes).
	v1Var = append(v1Var, varBuf[apOff+1+100:]...)

	// Decode v1 Apache2 variable section (hasMysql=true since encoder always sets it)
	target := &collector.Sample{}
	_, err = decodeVariable(v1Var, target, true, true, true, false)
	if err != nil {
		t.Fatalf("decodeVariable(v1 apache2) error: %v", err)
	}
	ap := target.Apps.Apache2
	if ap == nil {
		t.Fatal("expected Apache2 to be non-nil")
	}
	if ap.BusyWorkers != 3 || ap.IdleWorkers != 7 {
		t.Errorf("workers: busy=%d idle=%d", ap.BusyWorkers, ap.IdleWorkers)
	}
	if ap.TotalAccesses != 1234 || ap.TotalKBytes != 5678 {
		t.Errorf("counters: accesses=%d kbytes=%d", ap.TotalAccesses, ap.TotalKBytes)
	}
	if ap.Uptime != 12345 {
		t.Errorf("Uptime = %d, want 12345", ap.Uptime)
	}
	// v2-only fields must be zero
	if ap.Starting != 0 || ap.DNS != 0 || ap.Closing != 0 || ap.Logging != 0 ||
		ap.Graceful != 0 || ap.IdleCleanup != 0 || ap.OpenSlots != 0 {
		t.Errorf("v2-only scoreboard fields should be zero: S=%d D=%d C=%d L=%d G=%d I=%d .=%d",
			ap.Starting, ap.DNS, ap.Closing, ap.Logging, ap.Graceful, ap.IdleCleanup, ap.OpenSlots)
	}
}

// ---- extractTimestamp -------------------------------------------------------

func TestExtractTimestamp_HappyPath(t *testing.T) {
	ts := time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)
	s := &AggregatedSample{
		Timestamp: ts,
		Duration:  time.Second,
		Data:      &collector.Sample{Timestamp: ts},
	}
	data, err := encodeSample(s)
	if err != nil {
		t.Fatalf("encodeSample: %v", err)
	}

	got, err := extractTimestamp(data)
	if err != nil {
		t.Fatalf("extractTimestamp() error: %v", err)
	}
	if !got.Equal(ts) {
		t.Errorf("extractTimestamp() = %v, want %v", got, ts)
	}
}

func TestExtractTimestamp_TooShort(t *testing.T) {
	_, err := extractTimestamp([]byte{0x01, 0x02, 0x03})
	if err == nil {
		t.Error("extractTimestamp() with < 8 bytes should return error")
	}
}

// TestExtractTimestamp_KindByte exercises the recordKindBinary fast path:
// the on-disk format written by encodeSampleV has the kind byte at [0] and
// the timestamp at [1:9]. This is the path taken on every real disk read.
func TestExtractTimestamp_KindByte(t *testing.T) {
	ts := time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)
	s := &AggregatedSample{
		Timestamp: ts,
		Duration:  time.Second,
		Data:      &collector.Sample{Timestamp: ts},
	}

	disk, err := encodeSampleV(s)
	if err != nil {
		t.Fatalf("encodeSampleV: %v", err)
	}
	if disk[0] != recordKindBinary {
		t.Fatalf("disk[0] = %02x, want recordKindBinary", disk[0])
	}

	got, err := extractTimestamp(disk)
	if err != nil {
		t.Fatalf("extractTimestamp(kind-byte payload): %v", err)
	}
	if !got.Equal(ts) {
		t.Errorf("extractTimestamp = %v, want %v", got, ts)
	}

	// Ensure it agrees with a full decode after stripping the kind byte.
	full, err := decodeSample(disk[1:])
	if err != nil {
		t.Fatalf("decodeSample(disk[1:]): %v", err)
	}
	if !got.Equal(full.Timestamp) {
		t.Errorf("extractTimestamp %v != decodeSample %v", got, full.Timestamp)
	}
}

func TestExtractTimestamp_Zero(t *testing.T) {
	// 8 zero bytes is a valid payload — decodes to time.Unix(0, 0).
	got, err := extractTimestamp(make([]byte, 8))
	if err != nil {
		t.Errorf("extractTimestamp(zeroes) unexpected error: %v", err)
	}
	if !got.Equal(time.Unix(0, 0)) {
		t.Errorf("extractTimestamp(zeroes) = %v, want time.Unix(0,0)", got)
	}
}

func TestExtractTimestamp_MatchesFullDecode(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	s := &AggregatedSample{
		Timestamp: now,
		Duration:  time.Second,
		Data:      &collector.Sample{Timestamp: now},
	}
	data, _ := encodeSample(s)

	fast, err := extractTimestamp(data)
	if err != nil {
		t.Fatalf("extractTimestamp: %v", err)
	}
	full, err := decodeSample(data)
	if err != nil {
		t.Fatalf("decodeSample: %v", err)
	}
	if !fast.Equal(full.Timestamp) {
		t.Errorf("extractTimestamp %v != decodeSample %v", fast, full.Timestamp)
	}
}

// ---- TestTimestampOffset ----------------------------------------------------

// TestTimestampOffset verifies the timestamp layout in both payload formats:
//   - encodeSample()  (raw, no kind byte): timestamp at payload[0:8]
//   - encodeSampleV() (on-disk format):   kind byte at [0], timestamp at [1:9]
func TestTimestampOffset(t *testing.T) {
	ts := time.Date(2026, 3, 19, 12, 0, 0, 999999999, time.UTC)
	s := &AggregatedSample{
		Timestamp: ts,
		Duration:  time.Second,
		Data:      &collector.Sample{Timestamp: ts},
	}

	// Raw payload (encodeSample): timestamp at [0:8], no kind byte.
	raw, err := encodeSample(s)
	if err != nil {
		t.Fatalf("encodeSample: %v", err)
	}
	if len(raw) < 8 {
		t.Fatalf("raw payload too short: %d", len(raw))
	}
	ns := int64(binary.LittleEndian.Uint64(raw[0:8]))
	if ns != ts.UnixNano() {
		t.Errorf("raw payload[0:8] = %d, want %d (ts.UnixNano)", ns, ts.UnixNano())
	}

	// On-disk payload (encodeSampleV): kind byte at [0], timestamp at [1:9].
	disk, err := encodeSampleV(s)
	if err != nil {
		t.Fatalf("encodeSampleV: %v", err)
	}
	if len(disk) < 9 {
		t.Fatalf("disk payload too short: %d", len(disk))
	}
	if disk[0] != recordKindBinary {
		t.Errorf("disk[0] = %02x, want recordKindBinary (%02x)", disk[0], recordKindBinary)
	}
	nsOnDisk := int64(binary.LittleEndian.Uint64(disk[1:9]))
	if nsOnDisk != ts.UnixNano() {
		t.Errorf("disk payload[1:9] = %d, want %d (ts.UnixNano)", nsOnDisk, ts.UnixNano())
	}
}

// ---- TestRecordSizeReduction ------------------------------------------------

// TestRecordSizeReduction checks that a representative binary tier-0 record is
// well under the old JSON size (~3 KB). Target: < 1200 bytes.
func TestRecordSizeReduction(t *testing.T) {
	s := makeSampleFull(time.Now())
	s.Data.CPU.Sensors = []collector.CPUTempSensor{{Name: "Tctl", Value: 62.5}}
	s.Data.Disks.Devices = []collector.DiskDevice{{Name: "sda", Utilization: 15.3}}
	s.Data.Disks.FileSystems = []collector.FileSystemInfo{
		{Device: "/dev/sda1", MountPoint: "/", FSType: "ext4", Total: 500e9, Used: 200e9},
	}
	data, err := encodeSample(s)
	if err != nil {
		t.Fatalf("encodeSample: %v", err)
	}
	t.Logf("binary record size: %d bytes (JSON equivalent ~3 KB)", len(data))
	if len(data) > 1200 {
		t.Errorf("record too large: %d bytes, want < 1200", len(data))
	}
}

// ---- TestBinaryMigration ----------------------------------------------------

// TestBinaryMigration verifies that version-1 (JSON) records are decoded
// correctly through the decodeSampleV dispatch path.
func TestBinaryMigration(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	original := &AggregatedSample{
		Timestamp: ts,
		Duration:  time.Second,
		Data: &collector.Sample{
			Timestamp: ts,
			CPU:       collector.CPUStats{Total: collector.CPUCoreStats{Usage: 77.7}},
			System:    collector.SystemStats{Hostname: "legacy-host"},
		},
	}

	// Encode as JSON (simulates an existing v1 file record)
	jsonPayload, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	// ver=1 records use the JSON path; call the decoder directly.
	decoded, err := decodeSampleJSON(jsonPayload)
	if err != nil {
		t.Fatalf("decodeSampleJSON: %v", err)
	}
	if decoded.Data == nil {
		t.Fatal("decoded.Data is nil")
	}
	if decoded.Data.System.Hostname != "legacy-host" {
		t.Errorf("Hostname = %q, want \"legacy-host\"", decoded.Data.System.Hostname)
	}
	if decoded.Data.CPU.Total.Usage != 77.7 {
		t.Errorf("CPU Usage = %f, want 77.7", decoded.Data.CPU.Total.Usage)
	}
}

// ---- Benchmarks -------------------------------------------------------------

func BenchmarkEncodeSample(b *testing.B) {
	s := makeSampleFull(time.Now())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = encodeSample(s)
	}
}

func BenchmarkDecodeSample(b *testing.B) {
	s := makeSampleFull(time.Now())
	data, _ := encodeSample(s)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = decodeSample(data)
	}
}

func BenchmarkExtractTimestamp(b *testing.B) {
	s := makeSampleFull(time.Now())
	data, _ := encodeSample(s)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = extractTimestamp(data)
	}
}

// BenchmarkExtractVsFullDecode shows the speedup of the fixed-offset fast path.
func BenchmarkExtractVsFullDecode(b *testing.B) {
	s := makeSampleFull(time.Now())
	data, _ := encodeSample(s)

	b.Run("ExtractTimestamp", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = extractTimestamp(data)
		}
	})
	b.Run("FullDecode", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = decodeSample(data)
		}
	})
}

// ---- TestDecodePostgresV2Block -----------------------------------------------
// Regression test: records written with postgres v2 (104-byte block,
// presence=2) must still decode under the current v3 decoder, leaving the
// replication fields at zero (since v2 records pre-date them).

func TestDecodePostgresV2Block(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	sample := makeSampleFull(now)

	sample.Data.Apps.Postgres = &collector.PostgresStats{
		ActiveConns:     5,
		IdleConns:       10,
		IdleInTxConns:   1,
		WaitingConns:    2,
		MaxConns:        100,
		TxCommitPS:      42.5,
		TxRollbackPS:    0.3,
		TupFetchedPS:    100.0,
		TupReturnedPS:   200.0,
		TupInsertedPS:   10.0,
		TupUpdatedPS:    5.0,
		TupDeletedPS:    1.0,
		BlksReadPS:      0.4,
		BlksHitPS:       99.6,
		BlksHitPct:      99.5,
		DeadlocksPS:     0.0,
		BufCheckpointPS: 1.5,
		BufBackendPS:    0.8,
		DeadTuples:      500,
		LiveTuples:      5000,
		AutovacuumCount: 7,
		DBSizeBytes:     1024 * 1024 * 1024,
	}
	varBuf, err := appendVariable(nil, sample.Data)
	if err != nil {
		t.Fatalf("appendVariable: %v", err)
	}

	noPostgres := *sample.Data
	noPostgres.Apps.Postgres = nil
	noPgBuf, err := appendVariable(nil, &noPostgres)
	if err != nil {
		t.Fatalf("appendVariable (no pg): %v", err)
	}
	pgOff := -1
	for i := 0; i < len(noPgBuf) && i < len(varBuf); i++ {
		if noPgBuf[i] != varBuf[i] {
			pgOff = i
			break
		}
	}
	if pgOff < 0 {
		t.Fatal("could not find postgres presence byte offset")
	}

	// Construct a v2-format variable buffer: everything up to the postgres
	// presence byte, then presence=2 and the 104-byte v2 block, then the
	// trailing sections (mysql, apache2, custom) unchanged.
	var v2Var []byte
	v2Var = append(v2Var, varBuf[:pgOff]...)
	v2Var = append(v2Var, 2)
	pg := sample.Data.Apps.Postgres
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
	v2Var = append(v2Var, pb[:]...)
	// Skip the v3 presence byte + 121-byte block from varBuf and keep the rest.
	v2Var = append(v2Var, varBuf[pgOff+1+121:]...)

	target := &collector.Sample{}
	if _, err := decodeVariable(v2Var, target, true, true, true, false); err != nil {
		t.Fatalf("decodeVariable(v2 postgres) error: %v", err)
	}
	got := target.Apps.Postgres
	if got == nil {
		t.Fatal("expected Postgres to be non-nil")
	}
	if got.ActiveConns != 5 || got.IdleConns != 10 || got.MaxConns != 100 {
		t.Errorf("connection fields: active=%d idle=%d max=%d",
			got.ActiveConns, got.IdleConns, got.MaxConns)
	}
	if got.DBSizeBytes != 1024*1024*1024 {
		t.Errorf("DBSizeBytes = %d", got.DBSizeBytes)
	}
	// v3-only replication fields must be zero on a v2 record.
	if got.IsInRecovery || got.ReplicaCount != 0 ||
		got.ReplicationLagBytes != 0 || got.ReplicationLagSeconds != 0 {
		t.Errorf("v3 replication fields should be zero on v2 record: %+v", got)
	}
	// Alignment check: trailing sections must still decode.
	if target.System.Hostname != "test-host" {
		t.Errorf("alignment check failed: Hostname = %q", target.System.Hostname)
	}
}

// ---- TestDecodePostgresV3Block -----------------------------------------------
// Round-trip test for the v3 postgres block: encode a sample with replication
// fields populated, decode it, and verify every field survives.

func TestDecodePostgresV3Block(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	sample := makeSampleFull(now)
	sample.Data.Apps.Postgres = &collector.PostgresStats{
		ActiveConns:           3,
		IdleConns:             7,
		MaxConns:              200,
		TxCommitPS:            12.5,
		BlksHitPct:            98.0,
		DBSizeBytes:           42 * 1024 * 1024,
		IsInRecovery:          true,
		ReplicaCount:          0,
		ReplicationLagBytes:   16384,
		ReplicationLagSeconds: 0.42,
	}

	encoded, err := encodeSample(sample)
	if err != nil {
		t.Fatalf("encodeSample: %v", err)
	}
	decoded, err := decodeSample(encoded)
	if err != nil {
		t.Fatalf("decodeSample: %v", err)
	}
	got := decoded.Data.Apps.Postgres
	if got == nil {
		t.Fatal("expected Postgres to be non-nil after round-trip")
	}
	if !got.IsInRecovery {
		t.Error("IsInRecovery did not round-trip")
	}
	if got.ReplicationLagBytes != 16384 {
		t.Errorf("ReplicationLagBytes = %d, want 16384", got.ReplicationLagBytes)
	}
	if d := got.ReplicationLagSeconds - 0.42; d > 0.01 || d < -0.01 {
		t.Errorf("ReplicationLagSeconds = %v, want ~0.42", got.ReplicationLagSeconds)
	}
	if got.ReplicaCount != 0 {
		t.Errorf("ReplicaCount = %d, want 0", got.ReplicaCount)
	}

	// And the inverse case: a primary with replicas, lag fields zero.
	sample.Data.Apps.Postgres = &collector.PostgresStats{
		IsInRecovery: false,
		ReplicaCount: 4,
	}
	encoded2, err := encodeSample(sample)
	if err != nil {
		t.Fatalf("encodeSample (primary): %v", err)
	}
	dec2, err := decodeSample(encoded2)
	if err != nil {
		t.Fatalf("decodeSample (primary): %v", err)
	}
	if dec2.Data.Apps.Postgres.IsInRecovery || dec2.Data.Apps.Postgres.ReplicaCount != 4 {
		t.Errorf("primary round-trip failed: %+v", dec2.Data.Apps.Postgres)
	}
}

// ---- TestDecodeMysqlV2Block --------------------------------------------------
// Round-trip test for the v2 mysql block: replication fields must survive
// encode/decode. Also verifies the v1 backward-compat path leaves
// ReplicaSecondsBehind = -1 sentinel.

func TestDecodeMysqlV2Block(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	sample := makeSampleFull(now)
	sample.Data.Apps.Mysql = &collector.MysqlStats{
		ThreadsConnected:     8,
		QueriesPS:            100.0,
		ReplicaIORunning:     true,
		ReplicaSQLRunning:    true,
		ReplicaSecondsBehind: 3,
		ReplicaCount:         0,
	}
	encoded, err := encodeSample(sample)
	if err != nil {
		t.Fatalf("encodeSample: %v", err)
	}
	decoded, err := decodeSample(encoded)
	if err != nil {
		t.Fatalf("decodeSample: %v", err)
	}
	got := decoded.Data.Apps.Mysql
	if got == nil {
		t.Fatal("expected Mysql to be non-nil after round-trip")
	}
	if !got.ReplicaIORunning || !got.ReplicaSQLRunning {
		t.Errorf("replica thread flags didn't round-trip: io=%v sql=%v",
			got.ReplicaIORunning, got.ReplicaSQLRunning)
	}
	if got.ReplicaSecondsBehind != 3 {
		t.Errorf("ReplicaSecondsBehind = %d, want 3", got.ReplicaSecondsBehind)
	}

	// Sentinel: -1 must survive the round-trip too (not be confused with zero).
	sample.Data.Apps.Mysql = &collector.MysqlStats{
		QueriesPS:            42.0,
		ReplicaSecondsBehind: -1,
	}
	enc2, err := encodeSample(sample)
	if err != nil {
		t.Fatalf("encodeSample (sentinel): %v", err)
	}
	dec2, err := decodeSample(enc2)
	if err != nil {
		t.Fatalf("decodeSample (sentinel): %v", err)
	}
	if dec2.Data.Apps.Mysql.ReplicaSecondsBehind != -1 {
		t.Errorf("-1 sentinel didn't round-trip: got %d",
			dec2.Data.Apps.Mysql.ReplicaSecondsBehind)
	}
}

// ---- TestDecodeMysqlV3Block --------------------------------------------------
// Round-trip test for the v3 mysql block: the new errno + IOState fields
// must survive, AND a v2 record (presence byte 2 + 66-byte payload) must
// still decode correctly under the v3 decoder (the encoder always writes v3
// now, so we build the v2 buffer by hand).

func TestDecodeMysqlV3Block(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)

	// Part 1 — full v3 round-trip with errno + IOState populated.
	sample := makeSampleFull(now)
	sample.Data.Apps.Mysql = &collector.MysqlStats{
		ThreadsConnected:     12,
		QueriesPS:            50.0,
		ReplicaIORunning:     false,
		ReplicaSQLRunning:    true,
		ReplicaSecondsBehind: -1,
		ReplicaCount:         2,
		LastIOErrno:          1236,
		LastSQLErrno:         0,
		IOState:              "Reconnecting after a failed master event read",
	}
	encoded, err := encodeSample(sample)
	if err != nil {
		t.Fatalf("encodeSample: %v", err)
	}
	decoded, err := decodeSample(encoded)
	if err != nil {
		t.Fatalf("decodeSample: %v", err)
	}
	got := decoded.Data.Apps.Mysql
	if got == nil {
		t.Fatal("expected Mysql to be non-nil after round-trip")
	}
	if got.LastIOErrno != 1236 || got.LastSQLErrno != 0 {
		t.Errorf("errno fields didn't round-trip: io=%d sql=%d", got.LastIOErrno, got.LastSQLErrno)
	}
	if got.IOState != "Reconnecting after a failed master event read" {
		t.Errorf("IOState didn't round-trip: got %q", got.IOState)
	}
	// Alignment check: trailing sections must still decode.
	if decoded.Data.System.Hostname != "test-host" {
		t.Errorf("alignment check failed after v3 mysql: Hostname=%q", decoded.Data.System.Hostname)
	}

	// Part 2 — empty IOState must round-trip too (length byte = 0).
	sample.Data.Apps.Mysql = &collector.MysqlStats{QueriesPS: 1.0, ReplicaSecondsBehind: -1, IOState: ""}
	enc2, err := encodeSample(sample)
	if err != nil {
		t.Fatalf("encodeSample (empty state): %v", err)
	}
	dec2, err := decodeSample(enc2)
	if err != nil {
		t.Fatalf("decodeSample (empty state): %v", err)
	}
	if dec2.Data.Apps.Mysql.IOState != "" {
		t.Errorf("empty IOState round-trip: got %q", dec2.Data.Apps.Mysql.IOState)
	}

	// Part 3 — a hand-built v2 record (presence=2 + 66 bytes) must still
	// decode under the v3 decoder. Build it via the divergence trick: encode
	// with and without Mysql, find the byte offset that diverges.
	sample.Data.Apps.Mysql = &collector.MysqlStats{
		ThreadsConnected:     5,
		ReplicaIORunning:     true,
		ReplicaSQLRunning:    true,
		ReplicaSecondsBehind: 0,
		ReplicaCount:         1,
	}
	varBuf, err := appendVariable(nil, sample.Data)
	if err != nil {
		t.Fatalf("appendVariable: %v", err)
	}
	noMy := *sample.Data
	noMy.Apps.Mysql = nil
	noMyBuf, err := appendVariable(nil, &noMy)
	if err != nil {
		t.Fatalf("appendVariable (no mysql): %v", err)
	}
	myOff := -1
	for i := 0; i < len(noMyBuf) && i < len(varBuf); i++ {
		if noMyBuf[i] != varBuf[i] {
			myOff = i
			break
		}
	}
	if myOff < 0 {
		t.Fatal("could not find mysql presence byte offset")
	}

	// Build a v2 buffer in place of the v3 one. The first 66 bytes after
	// the presence byte are identical to v3's [0:66], so we can borrow them.
	var v2Var []byte
	v2Var = append(v2Var, varBuf[:myOff]...)             // up to mysql presence
	v2Var = append(v2Var, 2)                             // v2 presence tag
	v2Var = append(v2Var, varBuf[myOff+1:myOff+1+66]...) // v3's first 66B == v2 layout
	// In varBuf the v3 trailer is: 8B errno + (1+len) IOState. Skip it.
	v3StateLen := int(varBuf[myOff+1+74])
	v2Var = append(v2Var, varBuf[myOff+1+74+1+v3StateLen:]...) // append remainder

	target := &collector.Sample{}
	if _, err := decodeVariable(v2Var, target, true, true, true, false); err != nil {
		t.Fatalf("decodeVariable(v2 mysql) error: %v", err)
	}
	gotV2 := target.Apps.Mysql
	if gotV2 == nil {
		t.Fatal("expected Mysql non-nil from v2 record")
	}
	if !gotV2.ReplicaIORunning || gotV2.ReplicaCount != 1 {
		t.Errorf("v2 fields didn't decode: %+v", gotV2)
	}
	// v3 fields must be zero/empty on a v2 record.
	if gotV2.LastIOErrno != 0 || gotV2.LastSQLErrno != 0 || gotV2.IOState != "" {
		t.Errorf("v3 fields should be zero on v2 record: errno=%d/%d state=%q",
			gotV2.LastIOErrno, gotV2.LastSQLErrno, gotV2.IOState)
	}
	if target.System.Hostname != "test-host" {
		t.Errorf("alignment check failed after v2 mysql under v3 decoder: %q", target.System.Hostname)
	}
}

// ---- TestEncodeDecodePSU ----------------------------------------------------
// Power supplies live in a trailing, flag-gated section. Before it existed the
// collector's battery data never reached disk, so the dashboard could only ever
// draw the live tail and every history reload wiped the series.

func TestEncodeDecodePSU(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	original := makeSampleFull(now)
	original.Data.PSU = []collector.PowerSupplyStats{
		{
			Name: "BAT0", Type: "Battery", Status: "Discharging",
			Capacity: 87, VoltageV: 12.34, CurrentA: 1.25, PowerW: 15.5,
			EnergyWhNow: 42.75, EnergyWhFull: 50.0,
		},
		{Name: "AC", Type: "Mains", Status: "Charging", Capacity: 0},
	}

	encoded, err := encodeSample(original)
	if err != nil {
		t.Fatalf("encodeSample() error: %v", err)
	}
	decoded, err := decodeSample(encoded)
	if err != nil {
		t.Fatalf("decodeSample() error: %v", err)
	}
	if decoded.Data == nil {
		t.Fatal("decoded Data is nil")
	}
	if len(decoded.Data.PSU) != 2 {
		t.Fatalf("PSU count = %d, want 2", len(decoded.Data.PSU))
	}

	bat := decoded.Data.PSU[0]
	if bat.Name != "BAT0" || bat.Type != "Battery" || bat.Status != "Discharging" {
		t.Errorf("PSU identity = %q/%q/%q, want BAT0/Battery/Discharging", bat.Name, bat.Type, bat.Status)
	}
	if bat.Capacity != 87 {
		t.Errorf("Capacity = %d, want 87", bat.Capacity)
	}
	// float32 round-trip: allow 0.01 epsilon due to float64→float32 narrowing.
	for _, f := range []struct {
		name      string
		got, want float64
	}{
		{"VoltageV", bat.VoltageV, 12.34},
		{"CurrentA", bat.CurrentA, 1.25},
		{"PowerW", bat.PowerW, 15.5},
		{"EnergyWhNow", bat.EnergyWhNow, 42.75},
		{"EnergyWhFull", bat.EnergyWhFull, 50.0},
	} {
		if diff := f.got - f.want; diff > 0.01 || diff < -0.01 {
			t.Errorf("%s = %f, want ~%f", f.name, f.got, f.want)
		}
	}
	if ac := decoded.Data.PSU[1]; ac.Name != "AC" || ac.Type != "Mains" {
		t.Errorf("second supply = %q/%q, want AC/Mains", ac.Name, ac.Type)
	}
}

// TestEncodePSUCapacityClamped verifies an out-of-range capacity cannot wrap
// the byte cast into a plausible-looking percentage.
func TestEncodePSUCapacityClamped(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	for _, tc := range []struct {
		in, want int
	}{{-5, 0}, {300, 100}, {100, 100}} {
		sample := makeSampleFull(now)
		sample.Data.PSU = []collector.PowerSupplyStats{{Name: "BAT0", Type: "Battery", Capacity: tc.in}}
		encoded, err := encodeSample(sample)
		if err != nil {
			t.Fatalf("encodeSample(capacity=%d): %v", tc.in, err)
		}
		decoded, err := decodeSample(encoded)
		if err != nil {
			t.Fatalf("decodeSample(capacity=%d): %v", tc.in, err)
		}
		if got := decoded.Data.PSU[0].Capacity; got != tc.want {
			t.Errorf("Capacity %d encoded/decoded as %d, want %d", tc.in, got, tc.want)
		}
	}
}

// ---- TestDecodeRecordWithoutPSUSection ---------------------------------------
// Regression test: records written before flagHasPSU end after the custom
// metrics section. The decoder must not read supplies out of the bytes that
// follow — in an aggregated record those belong to the next min/max block.

func TestDecodeRecordWithoutPSUSection(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	sample := makeSampleFull(now)

	varBuf, err := appendVariable(nil, sample.Data)
	if err != nil {
		t.Fatalf("appendVariable: %v", err)
	}
	// Strip the trailing PSU section (version byte + zero count) to get the
	// exact bytes a pre-flagHasPSU encoder would have written.
	const psuEmptySize = 3
	oldVarBuf := varBuf[:len(varBuf)-psuEmptySize]

	fixedBuf := appendFixed(nil, sample.Data)
	var record []byte
	var preamble [18]byte
	binary.LittleEndian.PutUint64(preamble[0:], uint64(now.UnixNano()))
	binary.LittleEndian.PutUint64(preamble[8:], uint64(time.Second))
	binary.LittleEndian.PutUint16(preamble[16:],
		flagHasData|flagHasMin|flagHasMax|flagHasApps|flagHasApache2|flagHasMysql)
	record = append(record, preamble[:]...)
	for range 3 {
		record = append(record, fixedBuf...)
		record = append(record, oldVarBuf...)
	}

	decoded, err := decodeSample(record)
	if err != nil {
		t.Fatalf("decodeSample of pre-PSU aggregated record: %v", err)
	}
	if decoded.Data == nil || decoded.Min == nil || decoded.Max == nil {
		t.Fatalf("expected Data/Min/Max non-nil, got Data=%v Min=%v Max=%v",
			decoded.Data != nil, decoded.Min != nil, decoded.Max != nil)
	}
	for name, block := range map[string]*collector.Sample{
		"Data": decoded.Data, "Min": decoded.Min, "Max": decoded.Max,
	} {
		if block.System.Hostname != "test-host" {
			t.Errorf("%s.Hostname = %q, want %q", name, block.System.Hostname, "test-host")
		}
		if len(block.PSU) != 0 {
			t.Errorf("%s.PSU = %+v, want empty", name, block.PSU)
		}
	}
}
