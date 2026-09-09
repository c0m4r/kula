package main

import (
	"math"
	"strings"
	"time"

	"kula/internal/collector"
	"kula/internal/config"
)

func (g *generator) buildSample(ts time.Time, s workloadSignals) *collector.Sample {
	used := uint64(clamp(g.memUsed, 0, memTotal))
	cached := uint64(clamp(g.memCached, 0, memTotal-float64(used)))
	buffers := uint64(105*mib + 35*mib*math.Max(s.backup, s.disk))
	if used+cached+buffers > uint64(memTotal) {
		cached = uint64(memTotal) - used - buffers
	}
	free := uint64(memTotal) - used - cached - buffers
	available := free + uint64(float64(cached)*0.84) + uint64(float64(buffers)*0.72)
	swapUsedBytes := uint64(clamp(g.swapUsed, 0, swapTotal))

	processes := g.processStats(s)
	cpu := g.cpuStats(s)
	rootUsedBytes := uint64(g.rootUsed)
	dataUsedBytes := uint64(g.dataUsed)

	sample := &collector.Sample{
		Timestamp: ts,
		CPU:       cpu,
		LoadAvg: collector.LoadAvg{
			Load1:   round2(g.load1),
			Load5:   round2(g.load5),
			Load15:  round2(g.load15),
			Running: processes.Running,
			Total:   processes.Total,
		},
		Memory: collector.MemoryStats{
			Total:       uint64(memTotal),
			Free:        free,
			Available:   available,
			Used:        used,
			Buffers:     buffers,
			Cached:      cached,
			Shmem:       uint64(135*mib + 90*mib*s.memory),
			UsedPercent: percent(float64(used), memTotal),
		},
		Swap: collector.SwapStats{
			Total:       uint64(swapTotal),
			Free:        uint64(swapTotal) - swapUsedBytes,
			Used:        swapUsedBytes,
			UsedPercent: percent(float64(swapUsedBytes), swapTotal),
		},
		Network: g.networkStats(s),
		Disks: collector.DiskStats{
			Devices: []collector.DiskDevice{
				g.diskDevice(0, "wwid:eui.002538b321a4c8ef", "nvme0n1", true),
				g.diskDevice(1, "wwid:naa.5000c500d91a7e31", "sda", false),
			},
			FileSystems: []collector.FileSystemInfo{
				{
					Device: "/dev/nvme0n1p2", MountPoint: "/", FSType: "ext4",
					Total: uint64(rootTotal), Used: rootUsedBytes,
					Available: uint64(math.Max(0, rootTotal-float64(rootUsedBytes)-5*gib)),
					UsedPct:   percent(float64(rootUsedBytes), rootTotal),
				},
				{
					Device: "/dev/sda1", MountPoint: "/srv/data", FSType: "xfs",
					Total: uint64(dataTotal), Used: dataUsedBytes,
					Available: uint64(math.Max(0, dataTotal-float64(dataUsedBytes)-1*gib)),
					UsedPct:   percent(float64(dataUsedBytes), dataTotal),
				},
			},
		},
		System: collector.SystemStats{
			Hostname:    "mock-prod-01",
			Uptime:      round2(g.uptime),
			UptimeHuman: collector.FormatUptime(g.uptime),
			Entropy:     max(96, 256-int(120*(1-s.availability))),
			ClockSync:   !s.rebooting || s.availability > 0.72,
			ClockSource: "tsc",
			UserCount:   max(1, int(math.Round(1+3*s.activity))),
		},
		Process: processes,
		Self: collector.SelfStats{
			CPUPercent: round2(0.12 + float64(activeContainerCount(s))*0.012 + g.webRPS*0.00018),
			MemRSS:     uint64(g.selfRSS),
			FDs:        27 + int(g.webRPS/95),
		},
		GPU:  g.gpuStats(s),
		PSU:  g.powerStats(s),
		Apps: g.applicationStats(s),
	}
	return sample
}

func (g *generator) cpuStats(s workloadSignals) collector.CPUStats {
	usage := clamp(g.cpuUsage, 0, 100)
	diskPressure := clamp((g.disks[0].util+g.disks[1].util)/180, 0, 1)
	iowait := usage * (0.025 + 0.20*diskPressure)
	softIRQ := usage * (0.025 + min(0.07, g.webRPS/22_000))
	irq := usage * 0.006
	steal := usage * (0.006 + 0.01*s.traffic)
	system := usage * (0.17 + 0.055*s.memory)
	user := math.Max(0, usage-iowait-softIRQ-irq-steal-system)

	core := collector.CPUCoreStats{
		User:    round2(user),
		System:  round2(system),
		IOWait:  round2(iowait),
		IRQ:     round2(irq),
		SoftIRQ: round2(softIRQ),
		Steal:   round2(steal),
	}
	core.Usage = round2(core.User + core.System + core.IOWait + core.IRQ + core.SoftIRQ + core.Steal)
	return collector.CPUStats{
		Total:       core,
		NumCores:    cpuCores,
		Temperature: round2(g.cpuTemp),
		Sensors: []collector.CPUTempSensor{
			{Name: "Package id 0", Value: round2(g.cpuTemp)},
			{Name: "Core 0", Value: round2(g.cpuTemp - 1.7)},
			{Name: "Core 1", Value: round2(g.cpuTemp - 0.9)},
			{Name: "Core 2", Value: round2(g.cpuTemp - 2.3)},
			{Name: "Core 3", Value: round2(g.cpuTemp - 1.2)},
		},
	}
}

func (g *generator) processStats(s workloadSignals) collector.ProcessStats {
	total := 238 + int(math.Round(24*s.activity))
	running := max(1, int(math.Ceil(g.load1*0.62)))
	blocked := int(math.Round((g.disks[0].util + g.disks[1].util) / 62))
	zombie := 0
	if s.deploy > 0.72 {
		zombie = 1
	}
	if running+blocked+zombie >= total {
		total = running + blocked + zombie + 1
	}
	return collector.ProcessStats{
		Total:    total,
		Running:  running,
		Sleeping: total - running - blocked - zombie,
		Zombie:   zombie,
		Blocked:  blocked,
		Threads:  total*3 + 110 + int(g.webRPS/18),
	}
}

func (g *generator) networkStats(s workloadSignals) collector.NetworkStats {
	established := max(2, int(math.Round(12+g.webRPS*0.11*(1-0.2*s.network))))
	return collector.NetworkStats{
		Interfaces: []collector.NetInterface{
			interfaceMetric("enp1s0", &g.primaryNet),
			interfaceMetric("wg0", &g.vpnNet),
		},
		TCP: collector.TCPStats{
			CurrEstab: uint64(established),
			InErrs:    g.tcpInErrs,
			OutRsts:   g.tcpOutRsts,
			Retrans:   g.tcpRetrans,
		},
		Sockets: collector.SocketStats{
			TCPInUse: established + 18,
			TCPTw:    max(1, int(g.webRPS*0.075)),
			UDPInUse: 7 + int(3*s.activity),
		},
	}
}

func interfaceMetric(name string, state *interfaceState) collector.NetInterface {
	return collector.NetInterface{
		Name:    name,
		RxBytes: state.rxBytes.value,
		TxBytes: state.txBytes.value,
		RxMbps:  round2(state.rxBPS * 8 / 1_000_000),
		TxMbps:  round2(state.txBPS * 8 / 1_000_000),
		RxPkts:  state.rxPkts.value,
		TxPkts:  state.txPkts.value,
		RxPPS:   round2(state.rxPPS),
		TxPPS:   round2(state.txPPS),
		RxErrs:  state.rxErrs.value,
		TxErrs:  state.txErrs.value,
		RxDrop:  state.rxDrop.value,
		TxDrop:  state.txDrop.value,
	}
}

func (g *generator) diskDevice(index int, id, name string, nvme bool) collector.DiskDevice {
	d := g.disks[index]
	sensors := []collector.DiskTempSensor{{Name: "Composite", Value: round2(d.temp)}}
	if nvme {
		sensors = append(sensors,
			collector.DiskTempSensor{Name: "Sensor 1", Value: round2(d.temp + 1.8)},
			collector.DiskTempSensor{Name: "Sensor 2", Value: round2(d.temp - 1.1)},
		)
	}
	return collector.DiskDevice{
		ID:           id,
		Name:         name,
		ReadsPerSec:  round2(d.readsPS),
		WritesPerSec: round2(d.writesPS),
		ReadBytesPS:  round2(d.readBPS),
		WriteBytesPS: round2(d.writeBPS),
		Utilization:  round2(d.util),
		Temperature:  round2(d.temp),
		Sensors:      sensors,
	}
}

func (g *generator) gpuStats(s workloadSignals) []collector.GPUStats {
	if s.rebooting && s.availability < 0.2 {
		return nil
	}
	return []collector.GPUStats{{
		Index:       0,
		Name:        "NVIDIA GeForce RTX 3060",
		Driver:      "nvidia 580.82.09",
		Temperature: round2(g.gpuTemp),
		VRAMUsed:    uint64(g.gpuVRAM),
		VRAMTotal:   uint64(vramTotal),
		VRAMUsedPct: percent(g.gpuVRAM, vramTotal),
		LoadPct:     round2(g.gpuLoad),
		PowerW:      round2(g.gpuPower),
	}}
}

func (g *generator) powerStats(s workloadSignals) []collector.PowerSupplyStats {
	upsStatus := "Full"
	if s.power > 0 {
		upsStatus = "Discharging"
	} else if g.upsCapacity < 99.5 {
		upsStatus = "Charging"
	}
	mainsOnline := 1 - s.power
	mainsStatus := "Online"
	if s.power >= 0.5 {
		mainsStatus = "Offline"
	}
	return []collector.PowerSupplyStats{
		{
			Name: "AC0", Type: "Mains", Status: mainsStatus,
			VoltageV: round2(230 * mainsOnline), CurrentA: round2(g.serverPower / 230 * mainsOnline),
			PowerW: round2(g.serverPower * mainsOnline),
		},
		{
			Name: "ups0", Type: "UPS", Status: upsStatus,
			Capacity: int(math.Round(g.upsCapacity)), VoltageV: round2(52.4 - (100-g.upsCapacity)*0.035),
			CurrentA: round2(g.serverPower / 52.4), PowerW: round2(g.serverPower),
			EnergyWhNow: round2(1_500 * g.upsCapacity / 100), EnergyWhFull: 1_500,
		},
	}
}

func (g *generator) applicationStats(s workloadSignals) collector.ApplicationsStats {
	if !s.appsVisible {
		return collector.ApplicationsStats{}
	}
	delivered := g.webRPS * (1 - 0.32*s.network)
	accepts := g.webRPS * 0.34
	handled := accepts * (1 - 0.018*s.network)
	reading := max(1, int(math.Ceil(g.webRPS/260)))
	writing := max(1, int(math.Ceil(delivered/24)))
	waiting := max(1, int(math.Ceil(delivered*0.085)))

	return collector.ApplicationsStats{
		Nginx: &collector.NginxStats{
			ActiveConnections: reading + writing + waiting,
			Accepts:           g.ngAccepts.value,
			Handled:           g.ngHandled.value,
			Requests:          g.ngReq.value,
			AcceptsPS:         round2(accepts),
			HandledPS:         round2(handled),
			RequestsPS:        round2(delivered),
			Reading:           reading,
			Writing:           writing,
			Waiting:           waiting,
		},
		Apache2:    g.apacheStats(s),
		Containers: g.containerStats(s),
		Postgres:   g.postgresStats(s),
		Mysql:      g.mysqlStats(s),
		Custom:     g.customMetricStats(s),
	}
}

func (g *generator) customMetricStats(s workloadSignals) map[string][]collector.CustomMetricValue {
	latencyPressure := g.cpuUsage / 22
	if len(g.opts.CustomMetrics) == 0 {
		return map[string][]collector.CustomMetricValue{
			"request_pipeline": {
				{Name: "queue_depth", Value: round2(g.queueDepth)},
				{Name: "p95_latency_ms", Value: round2(18 + latencyPressure*latencyPressure + 240*s.replication + 85*s.network)},
				{Name: "error_rate_pct", Value: round2(0.08 + 4.5*s.network + 1.8*s.deploy)},
				{Name: "cache_hit_ratio", Value: round2(clamp(99.4-4.8*s.memory-2.2*s.replication, 80, 100))},
			},
			"go_runtime": {
				{Name: "goroutines", Value: float64(68 + activeContainerCount(s)*3 + int(g.webRPS/40))},
				{Name: "gc_pause_ms", Value: round2(0.35 + g.memUsed/gib*0.12 + 4*s.memory)},
			},
		}
	}

	result := make(map[string][]collector.CustomMetricValue, len(g.opts.CustomMetrics))
	for group, definitions := range g.opts.CustomMetrics {
		metrics := make([]collector.CustomMetricValue, 0, len(definitions))
		for _, definition := range definitions {
			metrics = append(metrics, collector.CustomMetricValue{
				Name:  definition.Name,
				Value: g.customMetricValue(group, definition, s),
			})
		}
		result[group] = metrics
	}
	return result
}

func (g *generator) customMetricValue(group string, definition config.CustomMetricConfig, s workloadSignals) float64 {
	key := strings.ToLower(group + " " + definition.Name + " " + definition.Unit)
	latencyPressure := g.cpuUsage / 22
	ratio := clamp(99.4-4.8*s.memory-2.2*s.replication, 75, 100)
	var value float64
	switch {
	case strings.Contains(key, "queue") || strings.Contains(key, "backlog"):
		value = g.queueDepth
	case strings.Contains(key, "latency") || strings.Contains(key, "duration") || strings.Contains(key, "pause"):
		value = 18 + latencyPressure*latencyPressure + 240*s.replication + 85*s.network
	case strings.Contains(key, "error") || strings.Contains(key, "fail"):
		value = 0.08 + 4.5*s.network + 1.8*s.deploy
	case strings.Contains(key, "hit") || strings.Contains(key, "ratio") || strings.Contains(key, "percent"):
		value = ratio
		if definition.Max > 0 && definition.Max <= 1 {
			value /= 100
		}
	case strings.Contains(key, "temp") || strings.Contains(key, "celsius"):
		value = g.cpuTemp
	case strings.Contains(key, "fan") || strings.Contains(key, "rpm"):
		value = 850 + g.cpuUsage*42
	case strings.Contains(key, "power") || strings.Contains(key, "watt"):
		value = g.serverPower
	case strings.Contains(key, "byte") || strings.Contains(key, "bps"):
		value = g.primaryNet.txBPS
	case strings.Contains(key, "connection") || strings.Contains(key, "session") || strings.Contains(key, "worker"):
		value = 8 + g.webRPS*0.11
	default:
		value = 8 + 64*s.activity + 25*s.traffic + 12*s.deploy
	}
	if definition.Max > 0 {
		value = clamp(value, 0, definition.Max)
	}
	return round2(value)
}

func (g *generator) apacheStats(s workloadSignals) *collector.Apache2Stats {
	reading := max(1, int(math.Ceil(g.apacheRPS/85)))
	sending := max(1, int(math.Ceil(g.apacheRPS/12)))
	keepalive := max(1, int(math.Ceil(g.apacheRPS/18)))
	starting := int(math.Ceil(2 * s.deploy))
	dns := int(math.Ceil(s.network))
	closing := max(1, int(math.Ceil(g.apacheRPS/90)))
	logging := max(1, int(math.Ceil(g.apacheRPS/120)))
	graceful := int(math.Ceil(3 * s.deploy))
	idleCleanup := 1
	busy := reading + sending + keepalive + starting + dns + closing + logging + graceful + idleCleanup
	waiting := max(2, 64-busy)
	return &collector.Apache2Stats{
		BusyWorkers: busy, IdleWorkers: waiting,
		TotalAccesses: g.apAccesses.value, TotalKBytes: g.apKBytes.value,
		AccessesPS: round2(g.apacheRPS), KBytesPS: round2(g.apacheRPS * 11.5),
		ReqPerSec: round2(g.apacheRPS), BytesPerSec: round2(g.apacheRPS * 11.5 * kib),
		BytesPerReq: 11.5 * kib, CPULoad: round2(g.cpuUsage * 0.18), Uptime: int64(g.uptime),
		Waiting: waiting, Reading: reading, Sending: sending, Keepalive: keepalive,
		Starting: starting, DNS: dns, Closing: closing, Logging: logging,
		Graceful: graceful, IdleCleanup: idleCleanup, OpenSlots: max(0, 256-busy-waiting),
	}
}

type containerTarget struct {
	id, name                string
	limit, memory           float64
	cpu, network, diskShare float64
}

func activeContainerCount(s workloadSignals) int {
	count := 4
	if math.Max(s.backup, s.disk) > 0.12 {
		count++
	}
	if s.deploy > 0 {
		count++
	}
	return count
}

func (g *generator) containerStats(s workloadSignals) []collector.ContainerStats {
	apiID := "a8d4e60195c2"
	if s.afterDeploy {
		apiID = "f941b0276d8a"
	}
	targets := []containerTarget{
		{id: "3f1c9a2b7e04", name: "web-frontend", limit: 768 * mib, memory: 180*mib + g.webRPS*18*kib, cpu: 3 + g.webRPS*0.014, network: 0.34, diskShare: 0.03},
		{id: apiID, name: "api-backend", limit: 2 * gib, memory: 520*mib + g.webRPS*95*kib + 180*mib*s.memory, cpu: 6 + g.webRPS*0.047, network: 0.46, diskShare: 0.18},
		{id: "c2b7f3389d11", name: "worker-queue", limit: 1536 * mib, memory: 280*mib + g.queueDepth*850*kib, cpu: 4 + g.queueDepth*0.34, network: 0.07, diskShare: 0.42},
		{id: "e5009ac471fe", name: "redis-cache", limit: 768 * mib, memory: 210*mib + g.webRPS*48*kib, cpu: 2 + g.webRPS*0.009, network: 0.13, diskShare: 0.08},
	}
	if math.Max(s.backup, s.disk) > 0.12 {
		targets = append(targets, containerTarget{
			id: "b44c0ffee901", name: "backup-job", limit: 384 * mib, memory: 145 * mib,
			cpu: 4 + 22*math.Max(s.backup, s.disk), network: 0.2, diskShare: 0.7,
		})
	}
	if s.deploy > 0 {
		targets = append(targets, containerTarget{
			id: "71ca9a4e03d2", name: "api-backend-canary", limit: 1 * gib,
			memory: 390*mib + g.webRPS*25*kib, cpu: 3 + g.webRPS*0.012, network: 0.1, diskShare: 0.06,
		})
	}

	result := make([]collector.ContainerStats, 0, len(targets))
	seconds := g.opts.Interval.Seconds()
	for _, target := range targets {
		memory := g.containerMemory[target.name]
		if memory == 0 {
			memory = target.memory * 0.75
		}
		memory = approach(memory, target.memory, 80, seconds)
		memory = clamp(memory, 8*mib, target.limit*0.98)
		g.containerMemory[target.name] = memory
		result = append(result, collector.ContainerStats{
			ID: target.id, Name: target.name,
			CPUPct:  round2(clamp(nonNegativeJitter(g.rng, target.cpu, 0.025), 0, cpuCores*100)),
			MemUsed: uint64(memory), MemLimit: uint64(target.limit), MemPct: percent(memory, target.limit),
			NetRxBPS: round2(g.primaryNet.rxBPS * target.network),
			NetTxBPS: round2(g.primaryNet.txBPS * target.network),
			DiskRBPS: round2(g.disks[0].readBPS * target.diskShare),
			DiskWBPS: round2(g.disks[0].writeBPS * target.diskShare),
		})
	}
	return result
}

func (g *generator) postgresStats(s workloadSignals) *collector.PostgresStats {
	delivered := g.webRPS * (1 - 0.32*s.network)
	commit := delivered * 0.62
	rollback := delivered * (0.002 + 0.045*s.replication)
	fetched := delivered * 8.5
	returned := delivered * 21
	inserted := delivered * 0.12
	updated := delivered * 0.18
	deleted := delivered * 0.018
	hitPct := clamp(99.65-3.6*s.memory-2.8*s.replication-0.7*s.disk, 82, 99.9)
	totalBlocks := returned * 0.085
	readBlocks := totalBlocks * (1 - hitPct/100)
	lagSeconds := 0.08 + 145*s.replication + 18*s.network
	return &collector.PostgresStats{
		ActiveConns: 4 + int(delivered/38), IdleConns: 12 + int(s.activity*8),
		IdleInTxConns: int(math.Ceil(2 * s.replication)), WaitingConns: int(math.Ceil(7*s.replication + 3*s.disk)), MaxConns: 200,
		TxCommitPS: round2(commit), TxRollbackPS: round2(rollback),
		TupFetchedPS: round2(fetched), TupReturnedPS: round2(returned),
		TupInsertedPS: round2(inserted), TupUpdatedPS: round2(updated), TupDeletedPS: round2(deleted),
		BlksReadPS: round2(readBlocks), BlksHitPS: round2(totalBlocks - readBlocks), BlksHitPct: round2(hitPct),
		DeadlocksPS: round2(0.018*s.replication + 0.004*s.disk),
		DeadTuples:  int64(g.pgDeadTuples), LiveTuples: int64(g.pgLiveTuples), AutovacuumCount: g.pgAutovacuumCount,
		BufCheckpointPS: round2(0.3 + 8*s.disk), BufBackendPS: round2(0.08 + 2.8*s.disk + 1.5*s.memory),
		DBSizeBytes: int64(g.pgDBSize), IsInRecovery: false, ReplicaCount: 1,
		ReplicationLagBytes: int64(lagSeconds * commit * 900), ReplicationLagSeconds: round2(lagSeconds),
	}
}

func (g *generator) mysqlStats(s workloadSignals) *collector.MysqlStats {
	queries := g.apacheRPS*2.4 + g.webRPS*0.15
	lag := int(math.Round(110*s.replication + 24*s.network))
	ioRunning := s.network < 0.72
	sqlRunning := s.replication < 0.82
	ioErr, sqlErr := 0, 0
	ioState := "Waiting for source to send event"
	if !ioRunning {
		ioErr = 2003
		ioState = "Reconnecting after a failed source event read"
	} else if !sqlRunning {
		sqlErr = 1205
		ioState = "Waiting for dependent transaction to commit"
	}
	return &collector.MysqlStats{
		ThreadsConnected: 11 + int(queries/35), ThreadsRunning: 1 + int(queries/90), ThreadsCached: 14, MaxConnections: 180,
		QueriesPS: round2(queries), ComSelectPS: round2(queries * 0.68), ComInsertPS: round2(queries * 0.11),
		ComUpdatePS: round2(queries * 0.13), ComDeletePS: round2(queries * 0.025),
		SlowQueriesPS:          round2(queries * (0.0004 + 0.012*s.replication + 0.004*s.disk)),
		InnodbBufferPoolHitPct: round2(clamp(99.7-3.2*s.memory-2.4*s.disk, 85, 100)),
		InnodbBPReadsPS:        round2(queries * (0.012 + 0.12*s.memory)),
		TableLocksWaitedPS:     round2(0.02 + 0.9*s.replication), RowLockWaitsPS: round2(0.04 + 2.5*s.replication),
		ReplicaIORunning: ioRunning, ReplicaSQLRunning: sqlRunning, ReplicaSecondsBehind: lag, ReplicaCount: 1,
		LastIOErrno: ioErr, LastSQLErrno: sqlErr, IOState: ioState,
	}
}

func percent(value, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return round2(value / total * 100)
}
