package collector

import (
	"context"
	"kula/internal/config"
	"log"
	"net/http"
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
	mu        sync.RWMutex
	cfg       config.GlobalConfig
	collCfg   config.CollectionConfig
	appCfg    config.ApplicationsConfig
	latest    *Sample
	prevCPU   []cpuRaw
	prevNet   map[string]netRaw
	prevDisk  map[string]diskRaw
	prevSelf  selfRaw
	prevTCP   tcpRaw
	prevEnergy map[string]uint64 // for Intel energy derivation
	gpus      []GPUInfo
	storageDir string
	prevTime  time.Time
	debugDone bool // set after the first Collect(); suppresses repeated debug logs

	// Application monitoring state
	nginxClient    *http.Client
	prevNginx      nginxRaw
	apacheClient   *http.Client
	prevApache     apache2Raw
	containerColl  *containerCollector
	pgCollector    *postgresCollector
	myCollector    *mysqlCollector
	customColl     *customCollector
	appCtx         context.Context
	appCancel      context.CancelFunc
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
		appCtx:     ctx,
		appCancel:  cancel,
	}

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
		log.Printf("[postgres] monitoring enabled for database %q", appCfg.Postgres.DBName)
	}

	if appCfg.Mysql.Enabled {
		c.myCollector = newMysqlCollector(
			appCfg.Mysql.Host,
			appCfg.Mysql.Port,
			appCfg.Mysql.User,
			appCfg.Mysql.Password,
			appCfg.Mysql.DBName,
			collCfg.DebugLog,
			collCfg.Interval,
		)
		log.Printf("[mysql] monitoring enabled for database %q", appCfg.Mysql.DBName)
	}

	if appCfg.Nginx.Enabled {
		log.Printf("[nginx] monitoring enabled at %s", appCfg.Nginx.StatusURL)
	}

	if appCfg.Apache2.Enabled {
		log.Printf("[apache2] monitoring enabled at %s", appCfg.Apache2.StatusURL)
	}

	// Initialize custom metrics collector if any groups are configured
	if len(appCfg.Custom) > 0 {
		sockPath := storageDir + "/kula.sock"
		cc, err := newCustomCollector(ctx, sockPath, appCfg.Custom, collCfg.DebugLog)
		if err != nil {
			log.Printf("[custom] failed to start: %v", err)
		} else {
			c.customColl = cc
		}
	}

	return c
}

// debugf logs a formatted message only when web.logging.level = "debug" is set
// AND only during the first collection cycle. Subsequent calls are no-ops.
func (c *Collector) debugf(format string, args ...any) {
	if c.collCfg.DebugLog && !c.debugDone {
		log.Printf(format, args...)
	}
}

// appErrorf logs an application error only once at startup — regardless of debug mode.
func (c *Collector) appErrorf(format string, args ...any) {
	if !c.debugDone {
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
	s.CPU.Temperature, s.CPU.Sensors = c.collectCPUTemperature()
	s.LoadAvg = c.collectLoadAvg()
	s.Memory, s.Swap = collectMemSwap()
	s.Network = c.collectNetwork(elapsed)
	s.Disks = c.collectDisks(elapsed)
	s.System = c.collectSystem()
	s.Process = collectProcesses()
	s.Self = c.collectSelf(elapsed)
	s.GPU = c.collectGPUs(elapsed)
	s.PSU = c.collectPSU()
	s.Apps = c.collectApps(elapsed)

	c.mu.Lock()
	c.latest = s
	c.mu.Unlock()

	// Suppress debug logs after the first collection cycle — devices and
	// interfaces don't change at runtime, so repeating them every second
	// would flood the log.
	c.debugDone = true

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
		apps.Nginx = c.collectNginx(elapsed)
	}

	if c.appCfg.Apache2.Enabled {
		apps.Apache2 = c.collectApache2(elapsed)
	}

	if c.containerColl != nil {
		apps.Containers = c.containerColl.Latest()
	}

	if c.appCfg.Postgres.Enabled {
		apps.Postgres = c.collectPostgres(elapsed)
	}

	if c.appCfg.Mysql.Enabled {
		apps.Mysql = c.collectMysql(elapsed)
	}

	if c.customColl != nil {
		apps.Custom = c.customColl.Latest()
	}

	return apps
}

// CustomConfig returns the custom metric configurations for use by the API layer.
func (c *Collector) CustomConfig() map[string][]config.CustomMetricConfig {
	return c.appCfg.Custom
}
