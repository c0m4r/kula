package main

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"time"

	"kula/internal/collector"
	"kula/internal/config"
)

const (
	kib = float64(1024)
	mib = 1024 * kib
	gib = 1024 * mib
	tib = 1024 * gib

	cpuCores  = 8
	memTotal  = 8 * gib
	swapTotal = 2 * gib
	vramTotal = 8 * gib
	rootTotal = 100 * gib
	dataTotal = 250 * gib
)

type workloadProfile string

const (
	profileRealistic workloadProfile = "realistic"
	profileSteady    workloadProfile = "steady"
)

func parseProfile(value string) (workloadProfile, error) {
	switch workloadProfile(strings.ToLower(strings.TrimSpace(value))) {
	case profileRealistic:
		return profileRealistic, nil
	case profileSteady:
		return profileSteady, nil
	default:
		return "", fmt.Errorf("unknown profile %q (want realistic or steady)", value)
	}
}

type generatorOptions struct {
	Seed          int64
	Interval      time.Duration
	TotalSamples  int
	Profile       workloadProfile
	CustomMetrics map[string][]config.CustomMetricConfig
}

type eventWindow struct {
	name       string
	start, end int // [start, end)
}

func (w eventWindow) active(i int) bool {
	return i >= w.start && i < w.end
}

func (w eventWindow) progress(i int) float64 {
	if !w.active(i) {
		return 0
	}
	if w.end-w.start <= 1 {
		return 1
	}
	return float64(i-w.start) / float64(w.end-w.start-1)
}

// envelope has gentle edges and a long plateau. It looks more like a sustained
// operational incident than independent one-sample spikes.
func (w eventWindow) envelope(i int) float64 {
	if w.active(i) && w.end-w.start <= 2 {
		return 1
	}
	x := w.progress(i)
	if x == 0 && !w.active(i) {
		return 0
	}
	switch {
	case x < 0.2:
		return smoothstep(x / 0.2)
	case x > 0.8:
		return smoothstep((1 - x) / 0.2)
	default:
		return 1
	}
}

type incidentPlan struct {
	traffic     eventWindow
	disk        eventWindow
	memory      eventWindow
	deploy      eventWindow
	network     eventWindow
	replication eventWindow
	power       eventWindow
	rebootAt    int
	recovery    eventWindow
}

type timelineEvent struct {
	Name  string
	Range string
}

type cumulativeCounter struct {
	value uint64
	carry float64
}

func (c *cumulativeCounter) add(rate, seconds float64) {
	amount := math.Max(0, rate)*seconds + c.carry
	whole, fraction := math.Modf(amount)
	c.value += uint64(whole)
	c.carry = fraction
}

func (c *cumulativeCounter) reset() {
	c.value = 0
	c.carry = 0
}

type interfaceState struct {
	rxBytes, txBytes cumulativeCounter
	rxPkts, txPkts   cumulativeCounter
	rxErrs, txErrs   cumulativeCounter
	rxDrop, txDrop   cumulativeCounter
	rxBPS, txBPS     float64
	rxPPS, txPPS     float64
}

func (n *interfaceState) reset() {
	n.rxBytes.reset()
	n.txBytes.reset()
	n.rxPkts.reset()
	n.txPkts.reset()
	n.rxErrs.reset()
	n.txErrs.reset()
	n.rxDrop.reset()
	n.txDrop.reset()
}

type diskState struct {
	readBPS, writeBPS float64
	readsPS, writesPS float64
	util, temp        float64
}

type workloadSignals struct {
	activity, backup, gpuBatch          float64
	traffic, disk, memory, deploy       float64
	network, replication, power         float64
	availability                        float64
	appsVisible, rebooting, afterDeploy bool
}

// generator is a small stateful simulation. Work is driven by shared signals
// (requests, I/O, memory pressure and incidents), making related metrics move
// together while slow thermal/load states lag behind instantaneous activity.
type generator struct {
	opts   generatorOptions
	rng    *rand.Rand
	events incidentPlan

	uptime                        float64
	cpuUsage, cpuTemp             float64
	load1, load5, load15          float64
	memUsed, memCached            float64
	swapUsed                      float64
	webRPS, apacheRPS             float64
	tcpInErrs, tcpOutRsts         float64
	tcpRetrans                    float64
	primaryNet, vpnNet            interfaceState
	disks                         [2]diskState
	rootUsed, dataUsed            float64
	lastCalendarYear, lastYearDay int
	gpuLoad, gpuTemp, gpuPower    float64
	gpuVRAM                       float64
	serverPower, upsCapacity      float64
	selfRSS                       float64
	queueDepth                    float64
	pgDeadTuples, pgLiveTuples    float64
	pgAutovacuumCount             int64
	pgDBSize                      float64
	containerMemory               map[string]float64
	ngAccepts, ngHandled, ngReq   cumulativeCounter
	apAccesses, apKBytes          cumulativeCounter
}

func newGenerator(opts generatorOptions) (*generator, error) {
	if opts.Interval <= 0 {
		return nil, errors.New("generator interval must be positive")
	}
	if opts.TotalSamples <= 0 {
		return nil, errors.New("generator sample count must be positive")
	}
	if opts.Profile == "" {
		opts.Profile = profileRealistic
	}
	if opts.Profile != profileRealistic && opts.Profile != profileSteady {
		return nil, fmt.Errorf("unsupported generator profile %q", opts.Profile)
	}

	g := &generator{
		opts:              opts,
		rng:               rand.New(rand.NewSource(opts.Seed)), //nolint:gosec // deterministic test-data simulation
		uptime:            47*24*60*60 + 3*60*60,
		cpuUsage:          14,
		cpuTemp:           43,
		load1:             0.9,
		load5:             0.85,
		load15:            0.8,
		memUsed:           2.3 * gib,
		memCached:         2.1 * gib,
		swapUsed:          32 * mib,
		webRPS:            70,
		apacheRPS:         15,
		rootUsed:          42 * gib,
		dataUsed:          118 * gib,
		gpuLoad:           4,
		gpuTemp:           38,
		gpuPower:          24,
		gpuVRAM:           768 * mib,
		serverPower:       145,
		upsCapacity:       98,
		selfRSS:           29 * mib,
		queueDepth:        4,
		pgDeadTuples:      18_000,
		pgLiveTuples:      2_400_000,
		pgAutovacuumCount: 842,
		pgDBSize:          36 * gib,
		containerMemory: map[string]float64{
			"web-frontend": 190 * mib,
			"api-backend":  620 * mib,
			"worker-queue": 320 * mib,
			"redis-cache":  210 * mib,
		},
	}
	g.primaryNet.rxBytes.value = uint64(6*tib + 205*gib)
	g.primaryNet.txBytes.value = uint64(18*tib + 717*gib)
	g.primaryNet.rxPkts.value = 8_900_000_000
	g.primaryNet.txPkts.value = 12_600_000_000
	g.primaryNet.rxErrs.value = 41
	g.primaryNet.txErrs.value = 7
	g.primaryNet.rxDrop.value = 126
	g.primaryNet.txDrop.value = 12
	g.vpnNet.rxBytes.value = uint64(420 * gib)
	g.vpnNet.txBytes.value = uint64(610 * gib)
	g.vpnNet.rxPkts.value = 720_000_000
	g.vpnNet.txPkts.value = 810_000_000
	g.ngAccepts.value = 1_840_000_000
	g.ngHandled.value = 1_839_998_700
	g.ngReq.value = 6_720_000_000
	g.apAccesses.value = 740_000_000
	g.apKBytes.value = 9_800_000_000
	g.events = buildIncidentPlan(opts.TotalSamples, opts.Interval)
	return g, nil
}

func buildIncidentPlan(total int, interval time.Duration) incidentPlan {
	return incidentPlan{
		traffic:     makeEvent("traffic surge", total, 0.12, 20*time.Minute, interval),
		disk:        makeEvent("disk saturation", total, 0.27, 35*time.Minute, interval),
		memory:      makeEvent("memory leak", total, 0.40, 2*time.Hour, interval),
		deploy:      makeEvent("rolling deploy", total, 0.55, 12*time.Minute, interval),
		network:     makeEvent("packet loss", total, 0.66, 8*time.Minute, interval),
		replication: makeEvent("replication lag", total, 0.76, 25*time.Minute, interval),
		power:       makeEvent("mains outage", total, 0.86, 15*time.Minute, interval),
		rebootAt:    fractionIndex(total, 0.94),
		recovery:    makeEvent("reboot recovery", total, 0.94, 90*time.Second, interval),
	}
}

func makeEvent(name string, total int, position float64, wanted time.Duration, interval time.Duration) eventWindow {
	length := int((wanted + interval - 1) / interval)
	maxLength := max(1, total/20)
	length = min(max(1, length), maxLength)
	start := fractionIndex(total, position)
	if start+length > total {
		start = max(0, total-length)
	}
	return eventWindow{name: name, start: start, end: start + length}
}

func fractionIndex(total int, fraction float64) int {
	if total <= 1 {
		return 0
	}
	return int(math.Round(float64(total-1) * fraction))
}

func (g *generator) timeline(start time.Time) []timelineEvent {
	if g.opts.Profile != profileRealistic {
		return nil
	}
	windows := []eventWindow{
		g.events.traffic,
		g.events.disk,
		g.events.memory,
		g.events.deploy,
		g.events.network,
		g.events.replication,
		g.events.power,
	}
	result := make([]timelineEvent, 0, len(windows)+1)
	for _, window := range windows {
		from := start.Add(time.Duration(window.start) * g.opts.Interval)
		to := start.Add(time.Duration(max(window.start, window.end-1)) * g.opts.Interval)
		result = append(result, timelineEvent{
			Name:  window.name,
			Range: from.Format(time.RFC3339) + " – " + to.Format(time.RFC3339),
		})
	}
	reboot := start.Add(time.Duration(g.events.rebootAt) * g.opts.Interval)
	result = append(result, timelineEvent{Name: "host reboot", Range: reboot.Format(time.RFC3339)})
	return result
}

func (g *generator) next(ts time.Time, i int) *collector.Sample {
	seconds := g.opts.Interval.Seconds()
	if g.opts.Profile == profileRealistic && i == g.events.rebootAt {
		g.resetAfterReboot()
	} else {
		g.uptime += seconds
	}

	signals := g.signals(ts, i)
	g.updateRequestRates(signals, seconds)
	g.updateDisks(signals, seconds)
	g.updateMemory(signals, seconds)
	g.updateGPU(signals, seconds)
	g.updateCPU(signals, seconds)
	g.updateNetwork(signals, seconds)
	g.updatePersistentState(ts, i, signals, seconds)
	return g.buildSample(ts, signals)
}

func (g *generator) signals(ts time.Time, i int) workloadSignals {
	activity := businessActivity(ts)
	backup := circularGaussian(decimalHour(ts), 2.25, 0.28)
	gpuBatch := circularGaussian(decimalHour(ts), 22.4, 0.9)
	s := workloadSignals{
		activity:     activity,
		backup:       backup,
		gpuBatch:     gpuBatch,
		availability: 1,
		appsVisible:  true,
	}
	if g.opts.Profile != profileRealistic {
		return s
	}

	s.traffic = g.events.traffic.envelope(i)
	s.disk = g.events.disk.envelope(i)
	s.memory = g.events.memory.progress(i)
	s.deploy = g.events.deploy.envelope(i)
	s.network = g.events.network.envelope(i)
	s.replication = g.events.replication.envelope(i)
	s.power = g.events.power.envelope(i)
	s.afterDeploy = i >= g.events.deploy.end
	s.rebooting = g.events.recovery.active(i)
	if s.rebooting {
		progress := g.events.recovery.progress(i)
		if g.events.recovery.end-g.events.recovery.start == 1 {
			s.availability = 0
			s.appsVisible = false
		} else {
			s.availability = smoothstep(progress)
			s.appsVisible = progress >= 0.35
		}
	}
	return s
}

func (g *generator) resetAfterReboot() {
	g.uptime = 0
	g.webRPS = 0
	g.apacheRPS = 0
	g.cpuUsage = 2
	g.load1 = 0.1
	g.primaryNet.reset()
	g.vpnNet.reset()
	g.ngAccepts.reset()
	g.ngHandled.reset()
	g.ngReq.reset()
	g.apAccesses.reset()
	g.apKBytes.reset()
	for name := range g.containerMemory {
		g.containerMemory[name] *= 0.65
	}
}

func (g *generator) updateRequestRates(s workloadSignals, seconds float64) {
	webTarget := 14 + 330*s.activity + 1_050*s.traffic
	webTarget *= 0.04 + 0.96*s.availability
	webTarget += g.rng.NormFloat64() * math.Max(1.5, webTarget*0.025)
	g.webRPS = approach(g.webRPS, clamp(webTarget, 0, 1_600), 12, seconds)

	apacheTarget := (4 + 72*s.activity + 120*s.traffic) * (0.1 + 0.9*s.availability)
	apacheTarget += g.rng.NormFloat64() * math.Max(0.8, apacheTarget*0.035)
	g.apacheRPS = approach(g.apacheRPS, clamp(apacheTarget, 0, 350), 18, seconds)
}

func (g *generator) updateDisks(s workloadSignals, seconds float64) {
	backupLoad := math.Max(s.backup, s.disk)
	read0 := 1.5*mib + g.webRPS*28*kib + 35*mib*s.disk
	write0 := 700*kib + g.webRPS*7*kib + 175*mib*s.disk
	read1 := 350*kib + 70*mib*backupLoad
	write1 := 450*kib + 145*mib*backupLoad
	g.updateDisk(0, read0, write0, 32*kib, 16*kib, seconds)
	g.updateDisk(1, read1, write1, 128*kib, 128*kib, seconds)
}

func (g *generator) updateDisk(index int, readTarget, writeTarget, readSize, writeSize, seconds float64) {
	d := &g.disks[index]
	d.readBPS = approach(d.readBPS, nonNegativeJitter(g.rng, readTarget, 0.04), 8, seconds)
	d.writeBPS = approach(d.writeBPS, nonNegativeJitter(g.rng, writeTarget, 0.05), 8, seconds)
	d.readsPS = d.readBPS / readSize
	d.writesPS = d.writeBPS / writeSize
	throughputCapacity := 320 * mib
	if index == 1 {
		throughputCapacity = 190 * mib
	}
	utilTarget := (d.readBPS+d.writeBPS)/throughputCapacity*100 + (d.readsPS+d.writesPS)/9_000*35
	d.util = approach(d.util, clamp(utilTarget, 0, 99.8), 7, seconds)
	tempTarget := 29 + d.util*0.23
	d.temp = approach(defaultIfZero(d.temp, 33), tempTarget, 150, seconds)
}

func (g *generator) updateMemory(s workloadSignals, seconds float64) {
	leak := 4.8 * gib * s.memory
	usedTarget := 1.95*gib + 1.05*gib*s.activity + g.webRPS*320*kib + leak
	usedTarget += 280 * mib * s.deploy
	tau := 150.0
	if s.memory > 0 {
		tau = 35
	}
	g.memUsed = approach(g.memUsed, clamp(usedTarget, 1.3*gib, 7.35*gib), tau, seconds)

	cacheTarget := 2.25*gib + 650*mib*math.Max(s.backup, s.disk)
	cacheTarget -= math.Max(0, g.memUsed-4.8*gib) * 0.55
	maxCache := math.Max(180*mib, memTotal-g.memUsed-420*mib)
	g.memCached = approach(g.memCached, clamp(cacheTarget, 180*mib, maxCache), 240, seconds)

	swapTarget := 24 * mib
	if g.memUsed > 5.1*gib {
		swapTarget += (g.memUsed - 5.1*gib) * 0.85
	}
	tau = 900
	if swapTarget < g.swapUsed {
		tau = 3_600
	}
	g.swapUsed = approach(g.swapUsed, clamp(swapTarget, 0, swapTotal*0.92), tau, seconds)
}

func (g *generator) updateGPU(s workloadSignals, seconds float64) {
	loadTarget := 3 + 82*s.gpuBatch + 7*s.activity + 25*s.traffic
	g.gpuLoad = approach(g.gpuLoad, clamp(nonNegativeJitter(g.rng, loadTarget, 0.035), 0, 100), 18, seconds)
	g.gpuVRAM = approach(g.gpuVRAM, 620*mib+g.gpuLoad/100*5.8*gib, 90, seconds)
	g.gpuPower = approach(g.gpuPower, 20+g.gpuLoad*1.55, 15, seconds)
	g.gpuTemp = approach(g.gpuTemp, 35+g.gpuLoad*0.43, 85, seconds)
}

func (g *generator) updateCPU(s workloadSignals, seconds float64) {
	memoryPressure := clamp((g.memUsed-5.2*gib)/(2*gib), 0, 1)
	target := 5 + g.webRPS*0.034 + g.apacheRPS*0.025
	target += g.disks[0].util*0.10 + g.disks[1].util*0.045
	target += g.gpuLoad*0.025 + 20*s.traffic + 11*memoryPressure + 7*s.deploy
	target += g.rng.NormFloat64() * 1.1
	g.cpuUsage = approach(g.cpuUsage, clamp(target, 1, 97.5), 10, seconds)

	tempTarget := 34 + g.cpuUsage*0.48
	g.cpuTemp = approach(g.cpuTemp, tempTarget, 75, seconds)
	runnable := g.cpuUsage/100*cpuCores + (g.disks[0].util+g.disks[1].util)/115
	g.load1 = approach(g.load1, runnable, 60, seconds)
	g.load5 = approach(g.load5, runnable, 300, seconds)
	g.load15 = approach(g.load15, runnable, 900, seconds)
}

func (g *generator) updateNetwork(s workloadSignals, seconds float64) {
	delivered := g.webRPS * (1 - 0.32*s.network)
	primaryRx := 220*kib + g.webRPS*1_650 + g.apacheRPS*1_300
	primaryTx := 160*kib + delivered*19*kib + g.apacheRPS*11*kib
	primaryRx += 8 * mib * s.backup
	primaryTx += 22 * mib * s.backup
	g.primaryNet.rxBPS = nonNegativeJitter(g.rng, primaryRx, 0.018)
	g.primaryNet.txBPS = nonNegativeJitter(g.rng, primaryTx, 0.02)
	g.primaryNet.rxPPS = g.primaryNet.rxBPS / 890
	g.primaryNet.txPPS = g.primaryNet.txBPS / 1_280
	g.primaryNet.rxBytes.add(g.primaryNet.rxBPS, seconds)
	g.primaryNet.txBytes.add(g.primaryNet.txBPS, seconds)
	g.primaryNet.rxPkts.add(g.primaryNet.rxPPS, seconds)
	g.primaryNet.txPkts.add(g.primaryNet.txPPS, seconds)
	g.primaryNet.rxErrs.add(0.08*s.network, seconds)
	g.primaryNet.txErrs.add(0.015*s.network, seconds)
	g.primaryNet.rxDrop.add(1.6*s.network, seconds)
	g.primaryNet.txDrop.add(0.35*s.network, seconds)

	vpnLoad := math.Max(s.backup, 0.08*s.activity)
	g.vpnNet.rxBPS = 35*kib + 4*mib*vpnLoad
	g.vpnNet.txBPS = 45*kib + 14*mib*vpnLoad
	g.vpnNet.rxPPS = g.vpnNet.rxBPS / 1_150
	g.vpnNet.txPPS = g.vpnNet.txBPS / 1_180
	g.vpnNet.rxBytes.add(g.vpnNet.rxBPS, seconds)
	g.vpnNet.txBytes.add(g.vpnNet.txBPS, seconds)
	g.vpnNet.rxPkts.add(g.vpnNet.rxPPS, seconds)
	g.vpnNet.txPkts.add(g.vpnNet.txPPS, seconds)

	g.tcpInErrs = round2(0.02 + 0.55*s.network)
	g.tcpOutRsts = round2(0.04 + 1.8*s.network + 0.15*s.deploy)
	g.tcpRetrans = round2(0.08 + delivered*0.0015 + 32*s.network)
}

func (g *generator) updatePersistentState(ts time.Time, i int, s workloadSignals, seconds float64) {
	delivered := g.webRPS * (1 - 0.32*s.network)
	accepts := g.webRPS * 0.34
	handled := accepts * (1 - 0.018*s.network)
	g.ngAccepts.add(accepts, seconds)
	g.ngHandled.add(handled, seconds)
	g.ngReq.add(delivered, seconds)
	g.apAccesses.add(g.apacheRPS, seconds)
	g.apKBytes.add(g.apacheRPS*11.5, seconds)

	insertRate := delivered * 0.12
	updateRate := delivered * 0.18
	deleteRate := delivered * 0.018
	g.pgDeadTuples += (updateRate + deleteRate) * seconds * 0.75
	g.pgLiveTuples += (insertRate - deleteRate) * seconds
	if g.opts.Profile == profileRealistic && i == g.events.disk.start {
		g.pgDeadTuples *= 0.16
		g.pgAutovacuumCount++
	}
	g.pgDBSize += (insertRate*720 + updateRate*65) * seconds

	queueTarget := math.Max(0, (g.webRPS-520)*0.16)
	queueTarget += 260*s.replication + 95*s.deploy
	g.queueDepth = approach(g.queueDepth, queueTarget, 22, seconds)

	year, yearDay := ts.Year(), ts.YearDay()
	if g.lastCalendarYear != 0 && (year != g.lastCalendarYear || yearDay != g.lastYearDay) {
		g.rootUsed = math.Max(39*gib, g.rootUsed-320*mib)
		g.pgDeadTuples *= 0.35
		g.pgAutovacuumCount++
	}
	g.lastCalendarYear, g.lastYearDay = year, yearDay
	g.rootUsed += (18*kib + delivered*145) * seconds
	g.dataUsed += (12*kib + insertRate*520 + updateRate*90) * seconds
	g.rootUsed = clamp(g.rootUsed, 0, rootTotal-2*gib)
	g.dataUsed = clamp(g.dataUsed, 0, dataTotal-5*gib)

	g.serverPower = 82 + g.cpuUsage*1.25 + g.gpuPower + (g.disks[0].util+g.disks[1].util)*0.22
	if s.power > 0 {
		g.upsCapacity -= g.serverPower / 1_500 * 100 * seconds / 3_600 * s.power
	} else if g.upsCapacity < 99.5 {
		g.upsCapacity += 4.5 * seconds / 3_600
	}
	g.upsCapacity = clamp(g.upsCapacity, 5, 100)

	selfTarget := 29*mib + float64(activeContainerCount(s))*180*kib + g.webRPS*320
	g.selfRSS = approach(g.selfRSS, selfTarget, 180, seconds)
}

func businessActivity(ts time.Time) float64 {
	hour := decimalHour(ts)
	morningDistance := (hour - 9.7) / 2.2
	afternoonDistance := (hour - 15.2) / 3.1
	morning := math.Exp(-0.5 * morningDistance * morningDistance)
	afternoon := math.Exp(-0.5 * afternoonDistance * afternoonDistance)
	activity := 0.055 + 0.48*morning + 0.72*afternoon
	if ts.Weekday() == time.Saturday || ts.Weekday() == time.Sunday {
		activity *= 0.48
	}
	return clamp(activity, 0.04, 1)
}

func decimalHour(ts time.Time) float64 {
	return float64(ts.Hour()) + float64(ts.Minute())/60 + float64(ts.Second())/3_600
}

func circularGaussian(hour, center, sigma float64) float64 {
	distance := math.Abs(hour - center)
	distance = math.Min(distance, 24-distance)
	normalized := distance / sigma
	return math.Exp(-0.5 * normalized * normalized)
}

func approach(current, target, tau, seconds float64) float64 {
	if tau <= 0 {
		return target
	}
	alpha := 1 - math.Exp(-seconds/tau)
	return current + (target-current)*alpha
}

func nonNegativeJitter(rng *rand.Rand, value, fraction float64) float64 {
	return math.Max(0, value*(1+rng.NormFloat64()*fraction))
}

func defaultIfZero(value, fallback float64) float64 {
	if value == 0 {
		return fallback
	}
	return value
}

func smoothstep(value float64) float64 {
	value = clamp(value, 0, 1)
	return value * value * (3 - 2*value)
}

func clamp(value, low, high float64) float64 {
	return min(max(value, low), high)
}

func round2(value float64) float64 {
	return math.Round(value*100) / 100
}
