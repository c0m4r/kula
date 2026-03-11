package collector

import (
	"kula-szpiegula/internal/config"
	"log"
	"sync"
	"time"
)

var (
	procPath   = "/proc"
	sysPath    = "/sys"
	runPath    = "/run"
	varRunPath = "/var/run"
)

// Collector orchestrates all metric sub-collectors.
type Collector struct {
	mu       sync.RWMutex
	cfg      config.GlobalConfig
	collCfg  config.CollectionConfig
	latest   *Sample
	prevCPU  []cpuRaw
	prevNet  map[string]netRaw
	prevDisk map[string]diskRaw
	prevSelf selfRaw
	prevTCP  tcpRaw
	prevTime time.Time
}

func New(cfg config.GlobalConfig, collCfg config.CollectionConfig) *Collector {
	return &Collector{
		cfg:      cfg,
		collCfg:  collCfg,
		prevNet:  make(map[string]netRaw),
		prevDisk: make(map[string]diskRaw),
	}
}

// debugf logs a formatted message only when web.logging.level = "debug" is set.
func (c *Collector) debugf(format string, args ...any) {
	if c.collCfg.DebugLog {
		log.Printf(format, args...)
	}
}

// Collect gathers all metrics and returns a Sample.
func (c *Collector) Collect() *Sample {
	now := time.Now()
	var elapsed float64
	if c.prevTime.IsZero() {
		elapsed = 1
	} else {
		elapsed = now.Sub(c.prevTime).Seconds()
		if elapsed <= 0 {
			elapsed = 1
		}
	}
	c.prevTime = now

	s := &Sample{
		Timestamp: now,
	}

	s.CPU = c.collectCPU(elapsed)
	s.CPU.Temperature, s.CPU.Sensors = collectCPUTemperature()
	s.LoadAvg = collectLoadAvg()
	s.Memory = collectMemory()
	s.Swap = collectSwap()
	s.Network = c.collectNetwork(elapsed)
	s.Disks = c.collectDisks(elapsed)
	s.System = c.collectSystem()
	s.Process = collectProcesses()
	s.Self = c.collectSelf(elapsed)

	c.mu.Lock()
	c.latest = s
	c.mu.Unlock()

	return s
}

// Latest returns the most recently collected sample.
func (c *Collector) Latest() *Sample {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latest
}
