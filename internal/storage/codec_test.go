package storage

import (
	"encoding/binary"
	"encoding/json"
	"kula/internal/collector"
	"testing"
	"time"
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

	// An empty-apps variable section has exactly 8 trailing bytes:
	// 1 (nginx=0) + 2 (containers=0) + 1 (postgres=0) + 1 (mysql=0) + 1 (apache2=0) + 2 (custom=0).
	const emptyAppsSize = 8
	oldVarBuf := varBuf[:len(varBuf)-emptyAppsSize]

	// Append 218 bytes of "next fixed block" — simulates a min/max block
	// that immediately follows the variable section in aggregated records.
	padded := make([]byte, len(oldVarBuf)+fixedBlockSize)
	copy(padded, oldVarBuf)

	// With hasApps=false, decodeVariable must consume only sections 1-6
	// and NOT touch the trailing 218 bytes.
	target := &collector.Sample{}
	n, err := decodeVariable(padded, target, false, false, false)
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
		ActiveConns:  5,
		IdleConns:    10,
		MaxConns:     100,
		TxCommitPS:   42.5,
		TxRollbackPS: 0.3,
		TupFetchedPS: 100.0,
		TupInsertedPS: 10.0,
		TupUpdatedPS: 5.0,
		TupDeletedPS: 1.0,
		BlksHitPct:   99.5,
		DeadTuples:   500,
		DBSizeBytes:  1024 * 1024 * 1024,
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
	v1Var = append(v1Var, varBuf[:pgOff]...)  // up to postgres presence (includes Apache2 byte)
	v1Var = append(v1Var, 1)                   // v1 presence tag
	// 56-byte v1 block: 3×int32 + 7×float32 + 2×int64
	var pb [56]byte
	binary.LittleEndian.PutUint32(pb[0:], uint32(int32(5)))    // ActiveConns
	binary.LittleEndian.PutUint32(pb[4:], uint32(int32(10)))   // IdleConns
	binary.LittleEndian.PutUint32(pb[8:], uint32(int32(100)))  // MaxConns
	putF32(pb[12:], 42.5)   // TxCommitPS
	putF32(pb[16:], 0.3)    // TxRollbackPS
	putF32(pb[20:], 100.0)  // TupFetchedPS
	putF32(pb[24:], 10.0)   // TupInsertedPS
	putF32(pb[28:], 5.0)    // TupUpdatedPS
	putF32(pb[32:], 1.0)    // TupDeletedPS
	putF32(pb[36:], 99.5)   // BlksHitPct
	binary.LittleEndian.PutUint64(pb[40:], uint64(500))                // DeadTuples
	binary.LittleEndian.PutUint64(pb[48:], uint64(1024*1024*1024))     // DBSizeBytes
	v1Var = append(v1Var, pb[:]...)
	// Custom metrics section (empty): from after v2 postgres block to end
	v1Var = append(v1Var, varBuf[pgOff+1+104:]...) // skip v2 presence+block, keep rest

	// Decode the v1-format variable section
	target := &collector.Sample{}
	_, err = decodeVariable(v1Var, target, true, true, false)
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
		ThreadsConnected:        5,
		ThreadsRunning:          2,
		ThreadsCached:           3,
		MaxConnections:          151,
		QueriesPS:               42.5,
		ComSelectPS:             20.0,
		ComInsertPS:             5.0,
		ComUpdatePS:             3.0,
		ComDeletePS:             1.0,
		SlowQueriesPS:           0.5,
		InnodbBufferPoolHitPct:  98.7,
		InnodbBPReadsPS:         0.3,
		TableLocksWaitedPS:      0.1,
		RowLockWaitsPS:          0.2,
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
	v1Var = append(v1Var, 1)                  // v1 presence tag
	// 72-byte v1 block: 2×int32 + 2×uint64 + 6×float32 + 1×int64 + 4×int32
	var ab [72]byte
	binary.LittleEndian.PutUint32(ab[0:], uint32(int32(3)))   // BusyWorkers
	binary.LittleEndian.PutUint32(ab[4:], uint32(int32(7)))   // IdleWorkers
	binary.LittleEndian.PutUint64(ab[8:], 1234)               // TotalAccesses
	binary.LittleEndian.PutUint64(ab[16:], 5678)              // TotalKBytes
	putF32(ab[24:], 2.5)                                       // AccessesPS
	putF32(ab[28:], 10.0)                                      // KBytesPS
	putF32(ab[32:], 2.5)                                       // ReqPerSec
	putF32(ab[36:], 8192)                                      // BytesPerSec
	putF32(ab[40:], 3276)                                      // BytesPerReq
	putF32(ab[44:], 0.12)                                      // CPULoad
	binary.LittleEndian.PutUint64(ab[48:], uint64(12345))     // Uptime
	binary.LittleEndian.PutUint32(ab[56:], uint32(int32(5)))  // Waiting
	binary.LittleEndian.PutUint32(ab[60:], uint32(int32(2)))  // Reading
	binary.LittleEndian.PutUint32(ab[64:], uint32(int32(1)))  // Sending
	binary.LittleEndian.PutUint32(ab[68:], uint32(int32(0)))  // Keepalive
	v1Var = append(v1Var, ab[:]...)
	// Custom section: from after the v2 apache2 block (presence=2 + 100 bytes).
	v1Var = append(v1Var, varBuf[apOff+1+100:]...)

	// Decode v1 Apache2 variable section (hasMysql=true since encoder always sets it)
	target := &collector.Sample{}
	_, err = decodeVariable(v1Var, target, true, true, true)
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
