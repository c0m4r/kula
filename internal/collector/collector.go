package collector

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"kula/internal/config"
)

var (
	procPath   = "/proc"
	sysPath    = "/sys"
	runPath    = "/run"
	varRunPath = "/var/run"
)

// Collector orchestrates all metric sub-collectors.
type Collector struct {
	mu           sync.RWMutex
	cfg          config.GlobalConfig
	collCfg      config.CollectionConfig
	appCfg       config.ApplicationsConfig
	latest       *Sample
	prevCPU      []cpuRaw
	prevNet      map[string]netRaw
	prevDisk     map[string]diskRaw
	diskWarnings map[string]bool
	prevSelf     selfRaw
	cpuTempOnce  sync.Once
	cpuTemps     []sysSensor // CPU temperature sensors, see cpuTempSensors
	prevTCP      tcpRaw
	prevTCPOK    bool                        // prevTCP is a baseline from a successful read
	prevEnergy   map[string]uint64           // for Intel energy derivation
	mounts       mountStats                  // statfs off the collection path
	diskTemps    cadence[diskTemp]           // disk temperatures at their own interval
	psuReads     cadence[[]PowerSupplyStats] // power supplies at their own interval
	gpus         []GPUInfo
	gpuScanAt    time.Time // last discovery that found no GPU; see gpuRescan
	storageDir   string
	prevTime     time.Time
	// debugDone is set after the first Collect() and suppresses repeated
	// debug logs. Atomic: application pollers read it from their goroutines.
	debugDone atomic.Bool

	// Application monitoring state. nginx and Apache2 are polled in their own
	// goroutines (startStatusPoller); each client, baseline and log belongs
	// to its poller.
	nginxClient   *http.Client
	prevNginx     nginxRaw
	nginxLog      pollLog
	nginxPoller   poller[NginxStats]
	apacheClient  *http.Client
	prevApache    apache2Raw
	apacheLog     pollLog
	apachePoller  poller[Apache2Stats]
	containerColl *containerCollector
	pgCollector   *postgresCollector
	myCollector   *mysqlCollector
	customColl    *customCollector
	appCtx        context.Context
	appCancel     context.CancelFunc
}

func New(cfg config.GlobalConfig, collCfg config.CollectionConfig, appCfg config.ApplicationsConfig, storageDir string) *Collector {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Collector{
		cfg:        cfg,
		collCfg:    collCfg,
		appCfg:     appCfg,
		storageDir: storageDir,
		prevNet:    make(map[string]netRaw),
		prevDisk:   make(map[string]diskRaw),
		prevEnergy: make(map[string]uint64),
		diskTemps:  cadence[diskTemp]{name: "[disk] temperature read on"},
		psuReads:   cadence[[]PowerSupplyStats]{name: "[psu] read of"},
		nginxLog:   pollLog{name: "[nginx]"},
		apacheLog:  pollLog{name: "[apache2]"},
		appCtx:     ctx,
		appCancel:  cancel,
	}

	return c
}

// StartApplications initializes the optional application collectors (containers,
// databases, web servers, custom metrics) and emits their startup log lines.
// It is split out from New so callers can defer this noisier output until after
// the main "Kula started" banner has been printed.
func (c *Collector) StartApplications() {
	appCfg := c.appCfg
	collCfg := c.collCfg
	ctx := c.appCtx

	// Initialize container collector if enabled
	if appCfg.Containers.Enabled {
		cc := newContainerCollector(ContainersCollectorConfig{
			Enabled:    true,
			SocketPath: appCfg.Containers.SocketPath,
			Containers: appCfg.Containers.Containers,
			DebugLog:   collCfg.DebugLog,
			Interval:   collCfg.Interval,
		})
		c.containerColl = cc
		cc.Start(ctx, collCfg.Interval)
		log.Printf("[containers] monitoring enabled (mode: %s)", cc.mode)
	}

	// Initialize postgres collector if enabled
	if appCfg.Postgres.Enabled {
		c.pgCollector = newPostgresCollector(
			appCfg.Postgres.Host,
			appCfg.Postgres.Port,
			appCfg.Postgres.User,
			appCfg.Postgres.Password,
			appCfg.Postgres.DBName,
			appCfg.Postgres.SSLMode,
			collCfg.DebugLog,
			collCfg.Interval,
		)
		c.pgCollector.Start(ctx)
		log.Printf("[postgres] monitoring enabled for database %q", appCfg.Postgres.DBName)
	}

	if appCfg.Mysql.Enabled {
		c.myCollector = newMysqlCollector(
			appCfg.Mysql.Host,
			appCfg.Mysql.Port,
			appCfg.Mysql.User,
			appCfg.Mysql.Password,
			appCfg.Mysql.DBName,
			appCfg.Mysql.SSLMode,
			appCfg.Mysql.SSLRootCert,
			collCfg.DebugLog,
			collCfg.Interval,
		)
		c.myCollector.Start(ctx)
		log.Printf("[mysql] monitoring enabled for database %q", appCfg.Mysql.DBName)
	}

	if appCfg.Nginx.Enabled {
		startStatusPoller(ctx, &c.nginxPoller, c.nginxLog.name, collCfg.Interval, c.collectNginx,
			func() { c.prevNginx = nginxRaw{} })
		log.Printf("[nginx] monitoring enabled at %s", logSafe(redactURL(appCfg.Nginx.StatusURL)))
	}

	if appCfg.Apache2.Enabled {
		startStatusPoller(ctx, &c.apachePoller, c.apacheLog.name, collCfg.Interval, c.collectApache2,
			func() { c.prevApache = apache2Raw{} })
		log.Printf("[apache2] monitoring enabled at %s", logSafe(redactURL(appCfg.Apache2.StatusURL)))
	}

	// Initialize custom metrics collector if any groups are configured
	if len(appCfg.Custom) > 0 {
		sockPath := c.storageDir + "/kula.sock"
		staleAfter := appCfg.CustomStaleAfter
		if staleAfter <= 0 {
			staleAfter = defaultCustomStaleAfter(collCfg.Interval)
		}
		cc, err := newCustomCollector(ctx, sockPath, appCfg.Custom, staleAfter, collCfg.DebugLog)
		if err != nil {
			log.Printf("[custom] failed to start: %v", err)
		} else {
			c.customColl = cc
		}
	}
}

// debugf logs a formatted message only when web.logging.level = "debug" is set
// AND only during the first collection cycle. Subsequent calls are no-ops.
// What it logs can come from a monitored server, so it goes through logSafe.
func (c *Collector) debugf(format string, args ...any) {
	if c.collCfg.DebugLog && !c.debugDone.Load() {
		log.Print(logSafe(fmt.Sprintf(format, args...)))
	}
}

// requestContext ends status-page requests when the collector stops.
func (c *Collector) requestContext() context.Context {
	if c.appCtx != nil {
		return c.appCtx
	}
	return context.Background()
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
	s.CPU.Temperature, s.CPU.Sensors = c.collectCPUTemperature()
	s.LoadAvg = c.collectLoadAvg()
	s.Memory, s.Swap = collectMemSwap()
	s.Network = c.collectNetwork(elapsed)
	s.Disks = c.collectDisks(elapsed)
	s.System = c.collectSystem()
	s.Process = collectProcesses()
	s.Self = c.collectSelf(elapsed)
	s.GPU = c.collectGPUs(elapsed)
	s.PSU = c.powerSupplies()
	s.Apps = c.collectApps(elapsed)
	boundSampleStrings(s)

	c.mu.Lock()
	c.latest = s
	c.mu.Unlock()

	// Suppress debug logs after the first collection cycle — devices and
	// interfaces don't change at runtime, so repeating them every second
	// would flood the log.
	c.debugDone.Store(true)

	return s
}

// Latest returns the most recently collected sample.
func (c *Collector) Latest() *Sample {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latest
}

// Stop cleans up application monitoring resources.
func (c *Collector) Stop() {
	if c.appCancel != nil {
		c.appCancel()
	}
	if c.pgCollector != nil {
		c.pgCollector.Close()
	}
	if c.myCollector != nil {
		c.myCollector.Close()
	}
	if c.customColl != nil {
		c.customColl.Close()
	}
}

// collectApps gathers metrics from all enabled application monitors.
func (c *Collector) collectApps(elapsed float64) ApplicationsStats {
	var apps ApplicationsStats

	if c.appCfg.Nginx.Enabled {
		apps.Nginx = c.nginxPoller.Latest()
	}

	if c.appCfg.Apache2.Enabled {
		apps.Apache2 = c.apachePoller.Latest()
	}

	if c.containerColl != nil {
		apps.Containers = c.containerColl.Latest()
	}

	if c.pgCollector != nil {
		apps.Postgres = c.pgCollector.Latest()
	}

	if c.myCollector != nil {
		apps.Mysql = c.myCollector.Latest()
	}

	if c.customColl != nil {
		apps.Custom = c.customColl.Latest()
	}

	return apps
}

// ReadingIntervals returns how often disk temperatures and power supplies are
// read: collection.disk_temp_interval and power_supply_interval, resolved.
func (c *Collector) ReadingIntervals() (diskTemp, powerSupply time.Duration) {
	return newCadenceSchedule(c.collCfg.DiskTempInterval, c.collCfg.Interval).every,
		newCadenceSchedule(c.collCfg.PowerSupplyInterval, c.collCfg.Interval).every
}

// CustomConfig returns the custom metric configurations for use by the API layer.
func (c *Collector) CustomConfig() map[string][]config.CustomMetricConfig {
	return c.appCfg.Custom
}
