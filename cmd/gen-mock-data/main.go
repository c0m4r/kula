package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"math"
	// nosemgrep: math-random-used -- dev-only mock data generator; no security relevance
	"math/rand"
	"os"
	"strings"
	"time"

	"kula/internal/collector"
	"kula/internal/config"
	"kula/internal/storage"
)

func main() {
	days := flag.Int("days", 7, "number of days of generated data to simulate (1s resolution)")
	cfgPath := flag.String("config", "config.yaml", "path to configuration file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	fmt.Printf("WARNING: This will generate %d days of mock data into '%s'.\n", *days, cfg.Storage.Directory)
	fmt.Printf("This may overwrite or mix with your existing data.\n")
	fmt.Print("Are you sure you want to proceed? (y/N): ")

	reader := bufio.NewReader(os.Stdin)
	response, _ := reader.ReadString('\n')
	response = strings.TrimSpace(strings.ToLower(response))

	if response != "y" && response != "yes" {
		fmt.Println("Aborted by user.")
		os.Exit(0)
	}

	fmt.Printf("Initializing storage at %s\n", cfg.Storage.Directory)
	store, err := storage.NewStore(cfg.Storage)
	if err != nil {
		log.Fatalf("Failed to initialize storage: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Printf("Error closing storage: %v", err)
		}
	}()

	totalSamples := *days * 24 * 60 * 60
	fmt.Printf("Generating %d samples (%d days of 1s resolution)...\n", totalSamples, *days)

	now := time.Now()
	startTime := now.Add(-time.Duration(totalSamples) * time.Second)

	gen := newGenerator()
	startGenTime := time.Now()

	for i := 0; i < totalSamples; i++ {
		ts := startTime.Add(time.Duration(i) * time.Second)
		sample := gen.next(ts, i)

		if err := store.WriteSample(sample); err != nil {
			log.Fatalf("Failed writing sample at index %d: %v", i, err)
		}

		if i > 0 && i%100000 == 0 {
			fmt.Printf("Generated %d / %d samples (%.1f%%)...\n", i, totalSamples, float64(i)/float64(totalSamples)*100)
		}
	}

	elapsed := time.Since(startGenTime)
	fmt.Printf("Finished generating %d samples in %v (%.0f samples/sec).\n",
		totalSamples, elapsed, float64(totalSamples)/elapsed.Seconds())
	fmt.Println("You can now start kula to test the performance boundaries!")
}

const (
	memTotal   = uint64(8 * 1024 * 1024 * 1024)   // 8 GB
	swapTotal  = uint64(2 * 1024 * 1024 * 1024)   // 2 GB
	vramTotal  = uint64(8 * 1024 * 1024 * 1024)   // 8 GB GPU
	fsTotal    = uint64(100 * 1024 * 1024 * 1024) // 100 GB root fs
	ctMemLimit = uint64(1024 * 1024 * 1024)       // 1 GB per container
)

// container is the slowly-evolving identity + state of one mock container. The
// number of "running" containers changes over time so encoded record sizes
// vary sample-to-sample, which mirrors real systems and exercises the storage
// ring buffer under variable-length records.
type container struct {
	id, name string
	cpu, mem float64
}

// generator holds the full random-walk state for the mock stream.
type generator struct {
	rng *rand.Rand

	// system
	cpuBase, cpuTemp       float64
	memUsed, swapUsed      uint64
	rxMbps, txMbps         float64
	rxBytes, txBytes       uint64
	rxPkts, txPkts         uint64
	diskUtil               [2]float64
	diskReadBps, diskWrite [2]float64
	diskTemp               [2]float64

	// gpu
	gpuLoad, gpuTemp, gpuPower float64
	gpuVRAM                    float64

	// nginx (cumulative counters + walked rates)
	ngAccepts, ngHandled, ngReq uint64
	ngRate                      float64

	// apache (cumulative + walked rate)
	apAccesses, apKBytes uint64
	apRate               float64

	// containers
	containers []*container
}

func newGenerator() *generator {
	return &generator{
		rng:         rand.New(rand.NewSource(time.Now().UnixNano())), // nosemgrep: math-random-used
		cpuBase:     15,
		cpuTemp:     45,
		memUsed:     uint64(1.5 * 1024 * 1024 * 1024),
		swapUsed:    200 * 1024 * 1024,
		rxMbps:      5,
		txMbps:      2,
		diskUtil:    [2]float64{5, 2},
		diskReadBps: [2]float64{1024 * 1024, 512 * 1024},
		diskWrite:   [2]float64{512 * 1024, 256 * 1024},
		diskTemp:    [2]float64{34, 32},
		gpuLoad:     8,
		gpuTemp:     40,
		gpuPower:    30,
		gpuVRAM:     1.5 * 1024 * 1024 * 1024,
		ngRate:      40,
		apRate:      20,
		containers: []*container{
			{"3f1c9a2b7e", "web-frontend", 8, 0.20},
			{"a8d4e60195", "api-backend", 16, 0.35},
			{"c2b7f3389d", "worker-queue", 5, 0.15},
			{"e5009ac471", "redis-cache", 2, 0.08},
		},
	}
}

// walk nudges v by up to ±step and clamps to [lo, hi].
func (g *generator) walk(v, step, lo, hi float64) float64 {
	return clamp(v+(g.rng.Float64()*2-1)*step, lo, hi)
}

func (g *generator) next(ts time.Time, i int) *collector.Sample {
	r := g.rng

	// Diurnal baseline: business hours push a higher CPU load.
	hourFrac := float64(ts.Hour())/24.0 + float64(ts.Minute())/1440.0
	diurnal := 10.0 + 30.0*math.Max(0, math.Sin((hourFrac-0.25)*math.Pi))

	g.cpuBase = clamp(g.cpuBase+r.Float64()*4-2, 0, 100)
	g.cpuBase += (diurnal - g.cpuBase) * 0.002
	if r.Float64() < 0.001 { // occasional spike
		g.cpuBase = math.Min(g.cpuBase+r.Float64()*40, 100)
	}
	g.cpuTemp = g.walk(g.cpuTemp+(35+g.cpuBase*0.4-g.cpuTemp)*0.05, 1.5, 30, 95)

	g.memUsed = uint64(clamp(float64(g.memUsed)+(r.Float64()*20-10)*1024*1024,
		256*1024*1024, float64(memTotal-512*1024*1024)))
	g.swapUsed = uint64(clamp(float64(g.swapUsed)+(r.Float64()*4-2)*1024*1024, 0, float64(swapTotal)))

	g.rxMbps = g.walk(g.rxMbps, 1, 0.1, 1000)
	g.txMbps = g.walk(g.txMbps, 0.5, 0.1, 1000)
	g.txMbps += (g.rxMbps*0.3 - g.txMbps) * 0.01
	// Accumulate monotonic byte/packet counters (1s interval).
	g.rxBytes += uint64(g.rxMbps * 1e6 / 8)
	g.txBytes += uint64(g.txMbps * 1e6 / 8)
	g.rxPkts += uint64(g.rxMbps * 100)
	g.txPkts += uint64(g.txMbps * 100)

	for d := 0; d < 2; d++ {
		g.diskUtil[d] = g.walk(g.diskUtil[d], 3, 0, 100)
		g.diskReadBps[d] = g.walk(g.diskReadBps[d], 100*1024, 0, 500*1024*1024)
		g.diskWrite[d] = g.walk(g.diskWrite[d], 50*1024, 0, 500*1024*1024)
		g.diskTemp[d] = g.walk(g.diskTemp[d], 0.5, 28, 60)
	}

	g.gpuLoad = g.walk(g.gpuLoad+(g.cpuBase*0.3-g.gpuLoad)*0.02, 5, 0, 100)
	g.gpuTemp = g.walk(g.gpuTemp+(38+g.gpuLoad*0.4-g.gpuTemp)*0.05, 1.5, 30, 90)
	g.gpuPower = g.walk(20+g.gpuLoad*1.5, 8, 10, 200)
	g.gpuVRAM = clamp(g.gpuVRAM+(r.Float64()*40-20)*1024*1024, 256*1024*1024, float64(vramTotal-256*1024*1024))

	g.ngRate = g.walk(g.ngRate, 8, 1, 500)
	g.ngAccepts += uint64(g.ngRate)
	g.ngHandled += uint64(g.ngRate)
	g.ngReq += uint64(g.ngRate * 1.4)

	g.apRate = g.walk(g.apRate, 5, 1, 400)
	g.apAccesses += uint64(g.apRate)
	g.apKBytes += uint64(g.apRate * 12)

	memFree := memTotal - g.memUsed
	memCached := memTotal / 10
	memBuffers := memTotal / 20

	s := &collector.Sample{
		Timestamp: ts,
		CPU: collector.CPUStats{
			Total: collector.CPUCoreStats{
				User:    round2(g.cpuBase * 0.55),
				System:  round2(g.cpuBase * 0.22),
				IOWait:  round2(g.cpuBase * 0.08),
				IRQ:     round2(g.cpuBase * 0.03),
				SoftIRQ: round2(g.cpuBase * 0.05),
				Steal:   round2(g.cpuBase * 0.01),
				Usage:   round2(g.cpuBase),
			},
			NumCores:    8,
			Temperature: round2(g.cpuTemp),
			Sensors: []collector.CPUTempSensor{
				{Name: "Package id 0", Value: round2(g.cpuTemp)},
				{Name: "Core 0", Value: round2(g.cpuTemp - 2)},
				{Name: "Core 1", Value: round2(g.cpuTemp - 1)},
			},
		},
		LoadAvg: collector.LoadAvg{
			Load1:   round2(g.cpuBase / 12.5),
			Load5:   round2(g.cpuBase / 16),
			Load15:  round2(g.cpuBase / 20),
			Running: 1 + int(g.cpuBase/25),
			Total:   320 + int(g.cpuBase),
		},
		Memory: collector.MemoryStats{
			Total:       memTotal,
			Used:        g.memUsed,
			Free:        memFree,
			Available:   memFree + memCached,
			Cached:      memCached,
			Buffers:     memBuffers,
			Shmem:       memTotal / 50,
			UsedPercent: round2(float64(g.memUsed) / float64(memTotal) * 100),
		},
		Swap: collector.SwapStats{
			Total:       swapTotal,
			Used:        g.swapUsed,
			Free:        swapTotal - g.swapUsed,
			UsedPercent: round2(float64(g.swapUsed) / float64(swapTotal) * 100),
		},
		Network: collector.NetworkStats{
			Interfaces: []collector.NetInterface{
				{
					Name:    "eth0",
					RxBytes: g.rxBytes, TxBytes: g.txBytes,
					RxPkts: g.rxPkts, TxPkts: g.txPkts,
					RxMbps: round2(g.rxMbps), TxMbps: round2(g.txMbps),
					RxPPS: round2(g.rxMbps * 100), TxPPS: round2(g.txMbps * 100),
					RxErrs: uint64(i / 50000), TxErrs: 0,
					RxDrop: uint64(i / 30000), TxDrop: 0,
				},
				{
					Name:    "lo",
					RxBytes: g.rxBytes / 8, TxBytes: g.rxBytes / 8,
					RxPkts: g.rxPkts / 8, TxPkts: g.rxPkts / 8,
					RxMbps: round2(g.rxMbps * 0.1), TxMbps: round2(g.rxMbps * 0.1),
					RxPPS: round2(g.rxMbps * 10), TxPPS: round2(g.rxMbps * 10),
				},
			},
			TCP: collector.TCPStats{
				CurrEstab: uint64(20 + int(g.cpuBase/4)),
				InErrs:    round2(float64(int(g.cpuBase/50) % 3)),
				OutRsts:   round2(float64(int(g.cpuBase/20) % 10)),
				Retrans:   round2(g.cpuBase * 0.02),
			},
			Sockets: collector.SocketStats{
				TCPInUse: 20 + int(g.cpuBase/4),
				UDPInUse: 5,
				TCPTw:    int(g.cpuBase / 8),
			},
		},
		Disks: collector.DiskStats{
			Devices: []collector.DiskDevice{
				{
					Name:         "sda",
					ReadsPerSec:  round2(g.diskReadBps[0] / 4096),
					WritesPerSec: round2(g.diskWrite[0] / 4096),
					ReadBytesPS:  round2(g.diskReadBps[0]),
					WriteBytesPS: round2(g.diskWrite[0]),
					Utilization:  round2(g.diskUtil[0]),
					Temperature:  round2(g.diskTemp[0]),
					Sensors:      []collector.DiskTempSensor{{Name: "Composite", Value: round2(g.diskTemp[0])}},
				},
				{
					Name:         "sdb",
					ReadsPerSec:  round2(g.diskReadBps[1] / 4096),
					WritesPerSec: round2(g.diskWrite[1] / 4096),
					ReadBytesPS:  round2(g.diskReadBps[1]),
					WriteBytesPS: round2(g.diskWrite[1]),
					Utilization:  round2(g.diskUtil[1]),
					Temperature:  round2(g.diskTemp[1]),
					Sensors:      []collector.DiskTempSensor{{Name: "Composite", Value: round2(g.diskTemp[1])}},
				},
			},
			FileSystems: []collector.FileSystemInfo{
				{
					Device: "/dev/sda1", MountPoint: "/", FSType: "ext4",
					Total: fsTotal, Used: 40*1024*1024*1024 + uint64(i)*512,
					Available: fsTotal - (40*1024*1024*1024 + uint64(i)*512),
					UsedPct:   round2(float64(40*1024*1024*1024+uint64(i)*512) / float64(fsTotal) * 100),
				},
				{
					Device: "/dev/sdb1", MountPoint: "/data", FSType: "xfs",
					Total: fsTotal * 2, Used: 120 * 1024 * 1024 * 1024,
					Available: fsTotal*2 - 120*1024*1024*1024, UsedPct: 60,
				},
			},
		},
		System: collector.SystemStats{
			Hostname:    "mock-server",
			Uptime:      float64(i),
			UptimeHuman: collector.FormatUptime(float64(i)),
			ClockSync:   true,
			ClockSource: "tsc",
			Entropy:     3000 + int(g.cpuBase),
			UserCount:   1 + int(g.cpuBase/40),
		},
		Process: collector.ProcessStats{
			Total:    320 + int(g.cpuBase),
			Running:  1 + int(g.cpuBase/25),
			Sleeping: 300 + int(g.cpuBase/2),
			Zombie:   boolToSpike(r, 0.002),
			Blocked:  int(g.cpuBase / 40),
			Threads:  900 + int(g.cpuBase*3),
		},
		Self: collector.SelfStats{
			CPUPercent: round2(0.1 + g.cpuBase*0.01),
			MemRSS:     18*1024*1024 + uint64(i%64)*1024*1024,
			FDs:        24 + int(g.cpuBase/20),
		},
		GPU: []collector.GPUStats{
			{
				Index: 0, Name: "NVIDIA GeForce RTX 3060", Driver: "nvidia 550.90",
				Temperature: round2(g.gpuTemp),
				VRAMUsed:    uint64(g.gpuVRAM), VRAMTotal: vramTotal,
				VRAMUsedPct: round2(g.gpuVRAM / float64(vramTotal) * 100),
				LoadPct:     round2(g.gpuLoad),
				PowerW:      round2(g.gpuPower),
			},
		},
		Apps: g.apps(i),
	}
	return s
}

// apps builds the application-metrics section. The container count slowly
// cycles (2→4) so encoded record sizes vary over the run.
func (g *generator) apps(i int) collector.ApplicationsStats {
	r := g.rng
	pgHit := 90 + r.Float64()*9

	nContainers := 2 + (i/600)%3 // 2,3,4 cycling every 10 minutes
	cts := make([]collector.ContainerStats, 0, nContainers)
	for _, c := range g.containers[:nContainers] {
		c.cpu = clamp(c.cpu+(r.Float64()*4-2), 0, 100)
		c.mem = clamp(c.mem+(r.Float64()*0.04-0.02), 0.02, 0.95)
		used := uint64(c.mem * float64(ctMemLimit))
		cts = append(cts, collector.ContainerStats{
			ID: c.id, Name: c.name,
			CPUPct:  round2(c.cpu),
			MemUsed: used, MemLimit: ctMemLimit,
			MemPct:   round2(c.mem * 100),
			NetRxBPS: round2(g.rxMbps * 1e6 / 8 * c.mem),
			NetTxBPS: round2(g.txMbps * 1e6 / 8 * c.mem),
			DiskRBPS: round2(g.diskReadBps[0] * c.mem),
			DiskWBPS: round2(g.diskWrite[0] * c.mem),
		})
	}

	return collector.ApplicationsStats{
		Containers: cts,
		Nginx: &collector.NginxStats{
			ActiveConnections: 30 + int(g.ngRate/4),
			Accepts:           g.ngAccepts,
			Handled:           g.ngHandled,
			Requests:          g.ngReq,
			AcceptsPS:         round2(g.ngRate),
			HandledPS:         round2(g.ngRate),
			RequestsPS:        round2(g.ngRate * 1.4),
			Reading:           1 + int(g.ngRate/50),
			Writing:           2 + int(g.ngRate/30),
			Waiting:           20 + int(g.ngRate/10),
		},
		Apache2: &collector.Apache2Stats{
			BusyWorkers: 5 + int(g.apRate/10), IdleWorkers: 20,
			TotalAccesses: g.apAccesses, TotalKBytes: g.apKBytes,
			AccessesPS: round2(g.apRate), KBytesPS: round2(g.apRate * 12),
			ReqPerSec: round2(g.apRate), BytesPerSec: round2(g.apRate * 12 * 1024),
			BytesPerReq: 12288, CPULoad: round2(g.cpuBase * 0.3),
			Uptime:  int64(i),
			Waiting: 18, Reading: 1, Sending: 4 + int(g.apRate/20), Keepalive: 6,
			OpenSlots: 100,
		},
		Postgres: &collector.PostgresStats{
			ActiveConns: 3 + int(g.cpuBase/10), IdleConns: 8, IdleInTxConns: 1,
			WaitingConns: int(g.cpuBase / 50), MaxConns: 100,
			TxCommitPS: round2(g.ngRate * 0.8), TxRollbackPS: round2(g.ngRate * 0.01),
			TupFetchedPS: round2(g.ngRate * 12), TupReturnedPS: round2(g.ngRate * 30),
			TupInsertedPS: round2(g.ngRate * 0.5), TupUpdatedPS: round2(g.ngRate * 0.3),
			TupDeletedPS: round2(g.ngRate * 0.05),
			BlksReadPS:   round2(g.ngRate * 2), BlksHitPS: round2(g.ngRate * 40),
			BlksHitPct: round2(pgHit), DeadlocksPS: 0,
			DeadTuples: 1000 + int64(i%5000), LiveTuples: 250000,
			AutovacuumCount: int64(i / 3600),
			BufCheckpointPS: round2(g.cpuBase * 0.01), BufBackendPS: round2(g.cpuBase * 0.02),
			DBSizeBytes:  2*1024*1024*1024 + int64(i)*1024,
			IsInRecovery: false, ReplicaCount: 1,
		},
		Mysql: &collector.MysqlStats{
			ThreadsConnected: 10 + int(g.apRate/20), ThreadsRunning: 1 + int(g.cpuBase/30),
			ThreadsCached: 8, MaxConnections: 151,
			QueriesPS:   round2(g.apRate * 2),
			ComSelectPS: round2(g.apRate * 1.4), ComInsertPS: round2(g.apRate * 0.3),
			ComUpdatePS: round2(g.apRate * 0.2), ComDeletePS: round2(g.apRate * 0.05),
			SlowQueriesPS:          round2(g.apRate * 0.002),
			InnodbBufferPoolHitPct: round2(95 + r.Float64()*4),
			InnodbBPReadsPS:        round2(g.apRate * 5),
			TableLocksWaitedPS:     0, RowLockWaitsPS: round2(g.cpuBase * 0.01),
			ReplicaIORunning: true, ReplicaSQLRunning: true,
			ReplicaSecondsBehind: int(g.cpuBase / 30), ReplicaCount: 1,
			IOState: "Waiting for source to send event",
		},
		Custom: map[string][]collector.CustomMetricValue{
			"myapp": {
				{Name: "queue_depth", Value: round2(g.walk(float64(i%50), 5, 0, 500))},
				{Name: "cache_hit_ratio", Value: round2(pgHit)},
				{Name: "active_sessions", Value: float64(30 + int(g.cpuBase))},
			},
		},
	}
}

// boolToSpike returns 1 with probability p, else 0 (for rare events like zombies).
func boolToSpike(r *rand.Rand, p float64) int {
	if r.Float64() < p {
		return 1
	}
	return 0
}

func clamp(v, minV, maxV float64) float64 {
	if v < minV {
		return minV
	}
	if v > maxV {
		return maxV
	}
	return v
}

// round2 rounds a float to 2 decimal places.
func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
