package collector

import (
	"sync"
	"time"
)

var (
	procPath = "/proc"
	sysPath  = "/sys"
	runPath  = "/run"
)

// Collector orchestrates all metric sub-collectors.
type Collector struct {
	mu       sync.RWMutex
	latest   *Sample
	prevCPU  []cpuRaw
	prevNet  map[string]netRaw
	prevDisk map[string]diskRaw
	prevSelf selfRaw
	prevTCP  tcpRaw
	prevTime time.Time
}

func New() *Collector {
	return &Collector{
		prevNet:  make(map[string]netRaw),
		prevDisk: make(map[string]diskRaw),
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
	s.LoadAvg = collectLoadAvg()
	s.Memory = collectMemory()
	s.Swap = collectSwap()
	s.Network = c.collectNetwork(elapsed)
	s.Disks = c.collectDisks(elapsed)
	s.System = collectSystem()
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
