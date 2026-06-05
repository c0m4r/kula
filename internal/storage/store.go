package storage

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"kula/internal/collector"
	"kula/internal/config"
)

func fmtRes(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return d.String()
}

// Store manages the tiered storage system.
type Store struct {
	mu      sync.RWMutex
	tiers   []*Tier
	configs []config.TierConfig
	dir     string

	// Cached aggregation ratios (computed once at NewStore).
	ratio1 int // how many tier-0 samples make one tier-1 record
	ratio2 int // how many tier-1 samples make one tier-2 record

	// Aggregation state
	tier1Count int
	tier1Buf   []*collector.Sample
	tier2Count int
	tier2Buf   []*AggregatedSample

	// latestCache holds the most recently written sample in memory.
	// This makes QueryLatest O(1) (a guarded pointer read) instead of
	// O(n) (a full disk scan of the tier file).
	latestCache *AggregatedSample

	// queryCache is a short-lived in-process cache for QueryRangeWithMeta.
	// It deduplicates identical or concurrent API calls. Entries carry a TTL of
	// one tier-0 resolution; WriteSample evicts expired and live-edge entries
	// instead of rebuilding the whole map, so cached past-window results survive
	// across collection ticks without a per-second allocation.
	queryCacheMu  sync.Mutex
	queryCache    map[queryCacheKey]queryCacheEntry
	queryCacheTTL time.Duration
}

// maxQueryCacheEntries caps the query cache as a safety bound. In normal
// operation WriteSample sweeps the cache every collection tick, so it stays far
// below this; the cap only matters if writes stall while reads keep arriving.
const maxQueryCacheEntries = 256

// maxAggBufferFactor bounds the in-memory aggregation buffers at this multiple
// of their tier ratio. The buffers normally reset to empty every ratio samples,
// so this never trips in healthy operation; it only matters if a lower tier's
// writes keep failing, bounding memory so a stuck disk can't OOM the process
// (the raw samples remain safe in tier 0).
const maxAggBufferFactor = 4

// queryCacheKey identifies a unique query rounded to tier resolution.
type queryCacheKey struct {
	fromNano     int64
	toNano       int64
	targetPoints int
}

// queryCacheEntry is a cached query result tagged with its expiry.
type queryCacheEntry struct {
	result    *HistoryResult
	expiresAt time.Time
}

func NewStore(cfg config.StorageConfig) (*Store, error) {
	absDir, err := filepath.Abs(cfg.Directory)
	if err != nil {
		return nil, fmt.Errorf("resolving storage directory: %w", err)
	}

	if err := os.MkdirAll(absDir, 0750); err != nil {
		return nil, fmt.Errorf("creating storage directory: %w", err)
	}

	s := &Store{
		dir:           absDir,
		configs:       cfg.Tiers,
		queryCache:    make(map[queryCacheKey]queryCacheEntry),
		queryCacheTTL: time.Second,
	}
	// Cache results for one tier-0 resolution so freshness matches the data's own
	// granularity (falls back to 1s when no positive resolution is configured).
	if len(cfg.Tiers) > 0 && cfg.Tiers[0].Resolution > 0 {
		s.queryCacheTTL = cfg.Tiers[0].Resolution
	}

	// Compute aggregation ratios once — used on every WriteSample tick.
	if len(cfg.Tiers) > 1 && cfg.Tiers[0].Resolution > 0 {
		s.ratio1 = int(cfg.Tiers[1].Resolution / cfg.Tiers[0].Resolution)
	}
	if s.ratio1 < 0 {
		s.ratio1 = 0
	}

	if len(cfg.Tiers) > 2 && cfg.Tiers[1].Resolution > 0 {
		s.ratio2 = int(cfg.Tiers[2].Resolution / cfg.Tiers[1].Resolution)
	}
	if s.ratio2 < 0 {
		s.ratio2 = 0
	}

	for i, tc := range cfg.Tiers {
		path := filepath.Join(absDir, fmt.Sprintf("tier_%d.dat", i))
		tier, err := OpenTier(path, tc.MaxBytes)
		if err != nil {
			// Close already opened tiers
			for _, t := range s.tiers {
				_ = t.Close()
			}
			return nil, fmt.Errorf("opening tier %d: %w", i, err)
		}
		s.tiers = append(s.tiers, tier)
	}

	// Warm the latest-sample cache so QueryLatest is O(1) from the first call.
	// This is the only full-tier scan at startup; every subsequent QueryLatest
	// uses the in-memory pointer set by WriteSample.
	s.warmLatestCache()

	// Reconstruct any partially-aggregated buffers so we don't drop intervals
	// after a process restart.
	s.reconstructAggregationState()

	return s, nil
}

// warmLatestCache reads the most recent sample from tier 0 and stores it
// in latestCache. Called once during NewStore to avoid the first QueryLatest
// being a full disk scan after a process restart.
func (s *Store) warmLatestCache() {
	if len(s.tiers) == 0 {
		return
	}
	samples, err := s.tiers[0].ReadLatest(1)
	if err == nil && len(samples) > 0 {
		s.latestCache = samples[0]
	}
}

// reconstructAggregationState reads the tails of lower tiers to restore
// the unaggregated memory buffers (tier1Buf, tier2Buf) and counters on startup.
func (s *Store) reconstructAggregationState() {
	if len(s.tiers) <= 1 {
		return
	}

	// Reconstruct Tier 1 state using the cached ratio.
	t1Newest := s.tiers[1].NewestTimestamp()
	t0Samples, err := s.tiers[0].ReadLatest(s.ratio1)
	if err == nil {
		var pending []*collector.Sample
		for _, as := range t0Samples {
			if as.Timestamp.After(t1Newest) {
				if as.Data != nil {
					pending = append(pending, as.Data)
				}
			}
		}
		s.tier1Buf = pending
		s.tier1Count = len(pending)
	}

	// Reconstruct Tier 2 state.
	if len(s.tiers) > 2 {
		t2Newest := s.tiers[2].NewestTimestamp()
		t1Samples, err := s.tiers[1].ReadLatest(s.ratio2)
		if err == nil {
			var pending []*AggregatedSample
			for _, as := range t1Samples {
				if as.Timestamp.After(t2Newest) {
					pending = append(pending, as)
				}
			}
			s.tier2Buf = pending
			s.tier2Count = len(pending)
		}
	}
}

// WriteSample writes a raw sample to tier 0 and triggers aggregation.
func (s *Store) WriteSample(sample *collector.Sample) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Guard against wall-clock regressions (NTP step-back, suspend/resume, VM
	// migration, an RTC that was ahead at boot). The ring buffer and ReadRange
	// assume non-decreasing timestamps; a record older than the previous one
	// makes range scans skip the finest tier or mis-window records. Clamp to the
	// last stored timestamp so on-disk order stays monotonic. During a backward
	// step timestamps briefly flatten until the wall clock catches up — far
	// preferable to losing query coverage. (This mutates the sample's timestamp,
	// which is intentional: the stored and broadcast views stay consistent.)
	if s.latestCache != nil && sample.Timestamp.Before(s.latestCache.Timestamp) {
		sample.Timestamp = s.latestCache.Timestamp
	}

	// Use the actual tier-0 resolution as the default/fallback duration.
	fallbackDur := s.configs[0].Resolution
	dur := fallbackDur
	if s.latestCache != nil {
		dur = sample.Timestamp.Sub(s.latestCache.Timestamp)
		if dur <= 0 {
			dur = fallbackDur
		}
	}

	as := &AggregatedSample{
		Timestamp: sample.Timestamp,
		Duration:  dur,
		Data:      sample,
	}

	if len(s.tiers) > 0 {
		if err := s.tiers[0].Write(as); err != nil {
			return fmt.Errorf("writing tier 0: %w", err)
		}
		// Update the in-memory cache so QueryLatest never needs a disk scan.
		s.latestCache = as
	}

	// Aggregate for tier 1 (every ratio1 samples)
	if s.ratio1 > 0 && len(s.tiers) > 1 {
		s.tier1Buf = append(s.tier1Buf, sample)
		s.tier1Count++
		// Bound the buffer in case tier-1 writes keep failing (a stuck/full
		// disk): without this the buffer would grow every tick forever. Drops
		// the oldest excess; those intervals stay available as raw tier-0 data.
		if cap1 := s.ratio1 * maxAggBufferFactor; len(s.tier1Buf) > cap1 {
			s.tier1Buf = append([]*collector.Sample(nil), s.tier1Buf[len(s.tier1Buf)-cap1:]...)
			s.tier1Count = len(s.tier1Buf)
		}

		if s.tier1Count >= s.ratio1 {
			agg := s.aggregateSamples(s.tier1Buf, s.configs[1].Resolution)
			if err := s.tiers[1].Write(agg); err != nil {
				return fmt.Errorf("writing tier 1: %w", err)
			}
			s.tier1Buf = nil
			s.tier1Count = 0

			if s.ratio2 > 0 && len(s.tiers) > 2 {
				s.tier2Buf = append(s.tier2Buf, agg)
				s.tier2Count++
				if cap2 := s.ratio2 * maxAggBufferFactor; len(s.tier2Buf) > cap2 {
					s.tier2Buf = append([]*AggregatedSample(nil), s.tier2Buf[len(s.tier2Buf)-cap2:]...)
					s.tier2Count = len(s.tier2Buf)
				}

				if s.tier2Count >= s.ratio2 {
					agg3 := s.aggregateAggregated(s.tier2Buf, s.configs[2].Resolution)
					if err := s.tiers[2].Write(agg3); err != nil {
						return fmt.Errorf("writing tier 2: %w", err)
					}
					s.tier2Buf = nil
					s.tier2Count = 0
				}
			}
		}
	}

	// Evict stale query-cache entries instead of rebuilding the whole map each
	// tick: drop anything expired plus any window reaching the live edge (which
	// must now reflect this new sample). Immutable past-window results stay cached.
	s.queryCacheMu.Lock()
	s.sweepQueryCacheLocked(time.Now(), sample.Timestamp.Truncate(time.Second).UnixNano())
	s.queryCacheMu.Unlock()

	return nil
}

// sweepQueryCacheLocked drops expired query-cache entries. When liveEdgeNano > 0
// it also drops any entry whose window reaches that timestamp, since a sample
// newly written at the live edge makes those results stale. Past-window entries
// (toNano < liveEdgeNano) are immutable and kept until their TTL lapses. The
// caller must hold s.queryCacheMu.
func (s *Store) sweepQueryCacheLocked(now time.Time, liveEdgeNano int64) {
	for k, e := range s.queryCache {
		if !now.Before(e.expiresAt) || (liveEdgeNano > 0 && k.toNano >= liveEdgeNano) {
			delete(s.queryCache, k)
		}
	}
}

// HistoryResult wraps query results with tier metadata for the API.
type HistoryResult struct {
	Samples    []*AggregatedSample `json:"samples"`
	Tier       int                 `json:"tier"`
	Resolution string              `json:"resolution"`
}

// QueryRange returns samples for a time range, choosing the best tier.
func (s *Store) QueryRange(from, to time.Time) ([]*AggregatedSample, error) {
	result, err := s.QueryRangeWithMeta(from, to, 450)
	if err != nil {
		return nil, err
	}
	return result.Samples, nil
}

// QueryRangeWithMeta returns samples with tier metadata.
// It returns the finest-resolution tier that (a) has data covering the window
// and (b) would not produce more than targetPoints*2 samples before downsampling.
// Results are cached for the duration of one tier-0 resolve cycle to serve
// concurrent or repeated API calls without extra disk I/O.
func (s *Store) QueryRangeWithMeta(from, to time.Time, targetPoints int) (*HistoryResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.tiers) == 0 {
		return &HistoryResult{}, nil
	}

	const maxSamples = 3600
	const maxScreenPoints = 7200
	if targetPoints <= 0 {
		targetPoints = 450
	} else if targetPoints > maxScreenPoints {
		targetPoints = maxScreenPoints
	}

	// --- Query cache lookup (lock-free read under store RLock) ---
	// Round from/to down to nearest second to unify slightly-different wall-clock calls.
	cacheKey := queryCacheKey{
		fromNano:     from.Truncate(time.Second).UnixNano(),
		toNano:       to.Truncate(time.Second).UnixNano(),
		targetPoints: targetPoints,
	}
	s.queryCacheMu.Lock()
	if entry, ok := s.queryCache[cacheKey]; ok {
		if time.Now().Before(entry.expiresAt) {
			cached := entry.result
			s.queryCacheMu.Unlock()
			cp := &HistoryResult{
				Samples:    append([]*AggregatedSample(nil), cached.Samples...),
				Tier:       cached.Tier,
				Resolution: cached.Resolution,
			}
			return cp, nil
		}
		// Expired — drop it and fall through to recompute.
		delete(s.queryCache, cacheKey)
	}
	s.queryCacheMu.Unlock()

	var resolutions []string
	var resDurations []time.Duration
	for _, tc := range s.configs {
		resolutions = append(resolutions, fmtRes(tc.Resolution))
		resDurations = append(resDurations, tc.Resolution)
	}

	duration := to.Sub(from)

	// Try each tier from finest (0) to coarsest.
	for tierIdx := 0; tierIdx < len(s.tiers); tierIdx++ {
		tier := s.tiers[tierIdx]

		// Skip entirely if this tier has no data for the window.
		if tier.Count() == 0 {
			continue
		}
		oldest := tier.OldestTimestamp()
		newest := tier.NewestTimestamp()
		// Tier doesn't cover any part of [from, to]
		if oldest.After(to) || newest.Before(from) {
			continue
		}

		// Estimate sample count for this tier.
		resDur := resDurations[tierIdx]
		estimatedSamples := int(duration / resDur)

		// If the estimated count is far beyond what the screen needs AND there
		// is a coarser tier available, prefer the coarser tier to avoid
		// reading and downsampling a large slice in-process.
		maxAllowed := maxSamples
		if targetPoints > maxAllowed {
			maxAllowed = targetPoints
		}
		if estimatedSamples > maxAllowed*2 && tierIdx < len(s.tiers)-1 {
			continue
		}

		samples, err := tier.ReadRange(from, to)
		if err != nil {
			return nil, fmt.Errorf("reading tier %d: %w", tierIdx, err)
		}
		if len(samples) == 0 {
			continue
		}

		res := resolutions[tierIdx]

		if len(samples) > int(float64(targetPoints)*1.5) {
			groupSize := len(samples) / targetPoints
			if groupSize > 1 {
				downsampled := make([]*AggregatedSample, 0, (len(samples)/groupSize)+1)
				for i := 0; i < len(samples); i += groupSize {
					end := i + groupSize
					if end > len(samples) {
						end = len(samples)
					}
					group := samples[i:end]

					var totalDur time.Duration
					for _, s := range group {
						totalDur += s.Duration
					}

					agg := s.aggregateAggregated(group, totalDur)
					if agg != nil {
						downsampled = append(downsampled, agg)
					}
				}
				samples = downsampled
				resDur := resDurations[tierIdx]
				res = fmtRes(resDur * time.Duration(groupSize))
			}
		}

		result := &HistoryResult{
			Samples:    samples,
			Tier:       tierIdx,
			Resolution: res,
		}

		// Cache with a TTL of one tier-0 resolution. The cap is a safety bound:
		// if it's reached we sweep expired entries first and, failing that, skip
		// caching rather than let the map grow without limit.
		s.queryCacheMu.Lock()
		if len(s.queryCache) >= maxQueryCacheEntries {
			s.sweepQueryCacheLocked(time.Now(), 0)
		}
		if len(s.queryCache) < maxQueryCacheEntries {
			s.queryCache[cacheKey] = queryCacheEntry{
				result:    result,
				expiresAt: time.Now().Add(s.queryCacheTTL),
			}
		}
		s.queryCacheMu.Unlock()

		return result, nil
	}

	// No data found in any tier
	res := resolutions[0]
	return &HistoryResult{Tier: 0, Resolution: res}, nil
}

// QueryLatest returns the latest sample from tier 1.
// After the first WriteSample call the result comes from the in-memory
// latestCache and requires no disk I/O at all.
func (s *Store) QueryLatest() (*AggregatedSample, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.tiers) == 0 {
		return nil, fmt.Errorf("no tiers configured")
	}

	// Fast path: in-memory cache is always kept current by WriteSample.
	// Return a shallow copy so callers cannot mutate the cached entry.
	if s.latestCache != nil {
		cp := *s.latestCache
		return &cp, nil
	}

	// Cold path: only reached on an empty store where no sample has been
	// written yet this process lifetime and warmLatestCache found nothing.
	return nil, nil
}

// TierCount returns the number of configured storage tiers.
func (s *Store) TierCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.tiers)
}

// SnapshotTier writes a consistent copy of tier i to w. The store's tier slice
// is immutable after construction, so only the per-tier read lock (taken inside
// SnapshotTo) is needed to guard against an in-flight Write; the brief store
// lock here just bounds-checks the index without blocking the collection loop
// for the duration of the copy.
func (s *Store) SnapshotTier(i int, w io.Writer) (int64, error) {
	s.mu.RLock()
	if i < 0 || i >= len(s.tiers) {
		s.mu.RUnlock()
		return 0, fmt.Errorf("tier index %d out of range (have %d tiers)", i, len(s.tiers))
	}
	t := s.tiers[i]
	s.mu.RUnlock()
	return t.SnapshotTo(w)
}

func (s *Store) Close() error {
	var firstErr error
	for _, t := range s.tiers {
		// Tier.Close already calls writeHeader; Flush is redundant.
		// Accumulate errors so all tiers are closed even if one fails.
		if err := t.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// aggregateSamples creates an aggregated sample from raw samples.
// Uses the last sample's values (for gauges) and averages for rates.
// Also tracks peak (maximum) values for CPU, disk utilisation, and network throughput.
// minSample returns an element-wise minimum of two samples.
func minSample(a, b *collector.Sample) *collector.Sample { return mergeSample(a, b, minF, minU) }
func maxSample(a, b *collector.Sample) *collector.Sample { return mergeSample(a, b, maxF, maxU) }

func mergeSample(a, b *collector.Sample, ff func(float64, float64) float64, fu func(uint64, uint64) uint64) *collector.Sample {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	res := *a // copy structure and unchanged fields (like timestamps, names)

	res.CPU.Total.Usage = ff(a.CPU.Total.Usage, b.CPU.Total.Usage)
	res.CPU.Total.User = ff(a.CPU.Total.User, b.CPU.Total.User)
	res.CPU.Total.System = ff(a.CPU.Total.System, b.CPU.Total.System)
	res.CPU.Total.IOWait = ff(a.CPU.Total.IOWait, b.CPU.Total.IOWait)
	res.CPU.Total.Steal = ff(a.CPU.Total.Steal, b.CPU.Total.Steal)
	res.CPU.Temperature = ff(a.CPU.Temperature, b.CPU.Temperature)

	res.LoadAvg.Load1 = ff(a.LoadAvg.Load1, b.LoadAvg.Load1)
	res.LoadAvg.Load5 = ff(a.LoadAvg.Load5, b.LoadAvg.Load5)
	res.LoadAvg.Load15 = ff(a.LoadAvg.Load15, b.LoadAvg.Load15)

	res.Memory.Used = fu(a.Memory.Used, b.Memory.Used)
	res.Memory.UsedPercent = ff(a.Memory.UsedPercent, b.Memory.UsedPercent)

	res.Swap.Used = fu(a.Swap.Used, b.Swap.Used)
	res.Swap.UsedPercent = ff(a.Swap.UsedPercent, b.Swap.UsedPercent)

	res.Disks.Devices = make([]collector.DiskDevice, len(a.Disks.Devices))
	for i := range a.Disks.Devices {
		devA := a.Disks.Devices[i]
		var devB collector.DiskDevice
		for _, dev := range b.Disks.Devices {
			if dev.Name == devA.Name {
				devB = dev
				break
			}
		}
		res.Disks.Devices[i] = collector.DiskDevice{
			Name:         devA.Name,
			Utilization:  ff(devA.Utilization, devB.Utilization),
			ReadBytesPS:  ff(devA.ReadBytesPS, devB.ReadBytesPS),
			WriteBytesPS: ff(devA.WriteBytesPS, devB.WriteBytesPS),
			ReadsPerSec:  ff(devA.ReadsPerSec, devB.ReadsPerSec),
			WritesPerSec: ff(devA.WritesPerSec, devB.WritesPerSec),
		}
	}

	res.Network.Interfaces = make([]collector.NetInterface, len(a.Network.Interfaces))
	for i := range a.Network.Interfaces {
		ifA := a.Network.Interfaces[i]
		var ifB collector.NetInterface
		for _, iface := range b.Network.Interfaces {
			if iface.Name == ifA.Name {
				ifB = iface
				break
			}
		}
		res.Network.Interfaces[i] = collector.NetInterface{
			Name:   ifA.Name,
			RxMbps: ff(ifA.RxMbps, ifB.RxMbps),
			TxMbps: ff(ifA.TxMbps, ifB.TxMbps),
			RxPPS:  ff(ifA.RxPPS, ifB.RxPPS),
			TxPPS:  ff(ifA.TxPPS, ifB.TxPPS),
		}
	}
	return &res
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
func minU(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
func maxU(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func roundF(v float64) float64 {
	return math.Round(v*100) / 100
}

func (s *Store) aggregateSamples(samples []*collector.Sample, dur time.Duration) *AggregatedSample {
	if len(samples) == 0 {
		return nil
	}

	// Use the last sample as the base (most gauges are "current value")
	last := samples[len(samples)-1]

	avg := *last
	// Deep-copy the three slices we mutate in-place below. The shallow struct
	// copy shares their backing arrays with last, so in-place element writes
	// would silently corrupt the original sample still held by tier 0.
	if len(last.CPU.Sensors) > 0 {
		avg.CPU.Sensors = make([]collector.CPUTempSensor, len(last.CPU.Sensors))
		copy(avg.CPU.Sensors, last.CPU.Sensors)
	}
	if len(last.Network.Interfaces) > 0 {
		avg.Network.Interfaces = make([]collector.NetInterface, len(last.Network.Interfaces))
		copy(avg.Network.Interfaces, last.Network.Interfaces)
	}
	if len(last.Disks.Devices) > 0 {
		avg.Disks.Devices = make([]collector.DiskDevice, len(last.Disks.Devices))
		copy(avg.Disks.Devices, last.Disks.Devices)
	}
	// Deep-copy Apps to avoid sharing pointers with the original sample.
	if last.Apps.Nginx != nil {
		ngCopy := *last.Apps.Nginx
		avg.Apps.Nginx = &ngCopy
	}
	if last.Apps.Apache2 != nil {
		apCopy := *last.Apps.Apache2
		avg.Apps.Apache2 = &apCopy
	}
	if len(last.Apps.Containers) > 0 {
		avg.Apps.Containers = make([]collector.ContainerStats, len(last.Apps.Containers))
		copy(avg.Apps.Containers, last.Apps.Containers)
	}
	if last.Apps.Postgres != nil {
		pgCopy := *last.Apps.Postgres
		avg.Apps.Postgres = &pgCopy
	}
	if last.Apps.Mysql != nil {
		myCopy := *last.Apps.Mysql
		avg.Apps.Mysql = &myCopy
	}
	if len(last.Apps.Custom) > 0 {
		avg.Apps.Custom = make(map[string][]collector.CustomMetricValue, len(last.Apps.Custom))
		for k, v := range last.Apps.Custom {
			cv := make([]collector.CustomMetricValue, len(v))
			copy(cv, v)
			avg.Apps.Custom[k] = cv
		}
	}

	var minS, maxS *collector.Sample

	if len(samples) > 1 {
		// Initialize min/max with a deep copy of the first element
		first := *samples[0]
		minS = &first
		firstMax := *samples[0]
		maxS = &firstMax

		var totalCPUUsage, totalCPUUser, totalCPUSys, totalCPUIowait, totalCPUSteal float64
		var totalLoad1, totalLoad5, totalLoad15 float64
		for _, s := range samples {
			totalCPUUsage += s.CPU.Total.Usage
			totalCPUUser += s.CPU.Total.User
			totalCPUSys += s.CPU.Total.System
			totalCPUIowait += s.CPU.Total.IOWait
			totalCPUSteal += s.CPU.Total.Steal

			totalLoad1 += s.LoadAvg.Load1
			totalLoad5 += s.LoadAvg.Load5
			totalLoad15 += s.LoadAvg.Load15

			minS = minSample(minS, s)
			maxS = maxSample(maxS, s)
		}

		fLen := float64(len(samples))
		avg.CPU.Total.Usage = roundF(totalCPUUsage / fLen)
		avg.CPU.Total.User = roundF(totalCPUUser / fLen)
		avg.CPU.Total.System = roundF(totalCPUSys / fLen)
		avg.CPU.Total.IOWait = roundF(totalCPUIowait / fLen)
		avg.CPU.Total.Steal = roundF(totalCPUSteal / fLen)

		avg.LoadAvg.Load1 = roundF(totalLoad1 / fLen)
		avg.LoadAvg.Load5 = roundF(totalLoad5 / fLen)
		avg.LoadAvg.Load15 = roundF(totalLoad15 / fLen)

		// Average CPU Temperature Sensors
		for i := range avg.CPU.Sensors {
			var tempSum float64
			count := 0
			for _, s := range samples {
				for _, sens := range s.CPU.Sensors {
					if sens.Name == avg.CPU.Sensors[i].Name {
						tempSum += sens.Value
						count++
					}
				}
			}
			if count > 0 {
				avg.CPU.Sensors[i].Value = roundF(tempSum / float64(count))
			}
		}

		// Average network rates per interface
		for i := range avg.Network.Interfaces {
			var rxSum, txSum, rxPpsSum, txPpsSum float64
			count := 0
			for _, s := range samples {
				for _, iface := range s.Network.Interfaces {
					if iface.Name == avg.Network.Interfaces[i].Name {
						rxSum += iface.RxMbps
						txSum += iface.TxMbps
						rxPpsSum += iface.RxPPS
						txPpsSum += iface.TxPPS
						count++
					}
				}
			}
			if count > 0 {
				avg.Network.Interfaces[i].RxMbps = roundF(rxSum / float64(count))
				avg.Network.Interfaces[i].TxMbps = roundF(txSum / float64(count))
				avg.Network.Interfaces[i].RxPPS = roundF(rxPpsSum / float64(count))
				avg.Network.Interfaces[i].TxPPS = roundF(txPpsSum / float64(count))
			}
		}

		// Average Disk I/O rates per device
		for i := range avg.Disks.Devices {
			var rBpsSum, wBpsSum, rIopsSum, wIopsSum float64
			count := 0
			for _, s := range samples {
				for _, dev := range s.Disks.Devices {
					if dev.Name == avg.Disks.Devices[i].Name {
						rBpsSum += dev.ReadBytesPS
						wBpsSum += dev.WriteBytesPS
						rIopsSum += dev.ReadsPerSec
						wIopsSum += dev.WritesPerSec
						count++
					}
				}
			}
			if count > 0 {
				avg.Disks.Devices[i].ReadBytesPS = roundF(rBpsSum / float64(count))
				avg.Disks.Devices[i].WriteBytesPS = roundF(wBpsSum / float64(count))
				avg.Disks.Devices[i].ReadsPerSec = roundF(rIopsSum / float64(count))
				avg.Disks.Devices[i].WritesPerSec = roundF(wIopsSum / float64(count))
			}
		}

		// ---- Average App metrics rates ----

		// Nginx rates
		if avg.Apps.Nginx != nil {
			var accPS, handPS, reqPS float64
			count := 0
			for _, s := range samples {
				if s.Apps.Nginx != nil {
					accPS += s.Apps.Nginx.AcceptsPS
					handPS += s.Apps.Nginx.HandledPS
					reqPS += s.Apps.Nginx.RequestsPS
					count++
				}
			}
			if count > 0 {
				fC := float64(count)
				avg.Apps.Nginx.AcceptsPS = roundF(accPS / fC)
				avg.Apps.Nginx.HandledPS = roundF(handPS / fC)
				avg.Apps.Nginx.RequestsPS = roundF(reqPS / fC)
			}
		}

		// Apache2 rates
		if avg.Apps.Apache2 != nil {
			var accPS, kbps float64
			count := 0
			for _, s := range samples {
				if s.Apps.Apache2 != nil {
					accPS += s.Apps.Apache2.AccessesPS
					kbps += s.Apps.Apache2.KBytesPS
					count++
				}
			}
			if count > 0 {
				fC := float64(count)
				avg.Apps.Apache2.AccessesPS = roundF(accPS / fC)
				avg.Apps.Apache2.KBytesPS = roundF(kbps / fC)
			}
		}

		// Container rates (match by ID)
		for i := range avg.Apps.Containers {
			ct := &avg.Apps.Containers[i]
			var cpuSum, memPctSum, rxSum, txSum, drSum, dwSum float64
			count := 0
			for _, s := range samples {
				for _, sc := range s.Apps.Containers {
					if sc.ID == ct.ID {
						cpuSum += sc.CPUPct
						memPctSum += sc.MemPct
						rxSum += sc.NetRxBPS
						txSum += sc.NetTxBPS
						drSum += sc.DiskRBPS
						dwSum += sc.DiskWBPS
						count++
						break
					}
				}
			}
			if count > 0 {
				fC := float64(count)
				ct.CPUPct = roundF(cpuSum / fC)
				ct.MemPct = roundF(memPctSum / fC)
				ct.NetRxBPS = roundF(rxSum / fC)
				ct.NetTxBPS = roundF(txSum / fC)
				ct.DiskRBPS = roundF(drSum / fC)
				ct.DiskWBPS = roundF(dwSum / fC)
			}
		}

		// Postgres rates
		if avg.Apps.Postgres != nil {
			var (
				commitPS, rollPS                           float64
				fetchPS, retPS, insPS, updPS, delPS        float64
				blksReadPS, blksHitPS, hitPct, deadlocksPS float64
				bufCkptPS, bufBackPS                       float64
			)
			count := 0
			for _, s := range samples {
				if s.Apps.Postgres != nil {
					pg := s.Apps.Postgres
					commitPS += pg.TxCommitPS
					rollPS += pg.TxRollbackPS
					fetchPS += pg.TupFetchedPS
					retPS += pg.TupReturnedPS
					insPS += pg.TupInsertedPS
					updPS += pg.TupUpdatedPS
					delPS += pg.TupDeletedPS
					blksReadPS += pg.BlksReadPS
					blksHitPS += pg.BlksHitPS
					hitPct += pg.BlksHitPct
					deadlocksPS += pg.DeadlocksPS
					bufCkptPS += pg.BufCheckpointPS
					bufBackPS += pg.BufBackendPS
					count++
				}
			}
			if count > 0 {
				fC := float64(count)
				avg.Apps.Postgres.TxCommitPS = roundF(commitPS / fC)
				avg.Apps.Postgres.TxRollbackPS = roundF(rollPS / fC)
				avg.Apps.Postgres.TupFetchedPS = roundF(fetchPS / fC)
				avg.Apps.Postgres.TupReturnedPS = roundF(retPS / fC)
				avg.Apps.Postgres.TupInsertedPS = roundF(insPS / fC)
				avg.Apps.Postgres.TupUpdatedPS = roundF(updPS / fC)
				avg.Apps.Postgres.TupDeletedPS = roundF(delPS / fC)
				avg.Apps.Postgres.BlksReadPS = roundF(blksReadPS / fC)
				avg.Apps.Postgres.BlksHitPS = roundF(blksHitPS / fC)
				avg.Apps.Postgres.BlksHitPct = roundF(hitPct / fC)
				avg.Apps.Postgres.DeadlocksPS = roundF(deadlocksPS / fC)
				avg.Apps.Postgres.BufCheckpointPS = roundF(bufCkptPS / fC)
				avg.Apps.Postgres.BufBackendPS = roundF(bufBackPS / fC)
			}
		}

		// MySQL rates
		if avg.Apps.Mysql != nil {
			var (
				queriesPS, selectPS, insertPS, updatePS, deletePS float64
				slowPS, bpReadsPS, bpHitPct                       float64
				tableLkWaitedPS, rowLkWaitsPS                     float64
			)
			count := 0
			for _, s := range samples {
				if s.Apps.Mysql != nil {
					my := s.Apps.Mysql
					queriesPS += my.QueriesPS
					selectPS += my.ComSelectPS
					insertPS += my.ComInsertPS
					updatePS += my.ComUpdatePS
					deletePS += my.ComDeletePS
					slowPS += my.SlowQueriesPS
					bpReadsPS += my.InnodbBPReadsPS
					bpHitPct += my.InnodbBufferPoolHitPct
					tableLkWaitedPS += my.TableLocksWaitedPS
					rowLkWaitsPS += my.RowLockWaitsPS
					count++
				}
			}
			if count > 0 {
				fC := float64(count)
				avg.Apps.Mysql.QueriesPS = roundF(queriesPS / fC)
				avg.Apps.Mysql.ComSelectPS = roundF(selectPS / fC)
				avg.Apps.Mysql.ComInsertPS = roundF(insertPS / fC)
				avg.Apps.Mysql.ComUpdatePS = roundF(updatePS / fC)
				avg.Apps.Mysql.ComDeletePS = roundF(deletePS / fC)
				avg.Apps.Mysql.SlowQueriesPS = roundF(slowPS / fC)
				avg.Apps.Mysql.InnodbBPReadsPS = roundF(bpReadsPS / fC)
				avg.Apps.Mysql.InnodbBufferPoolHitPct = roundF(bpHitPct / fC)
				avg.Apps.Mysql.TableLocksWaitedPS = roundF(tableLkWaitedPS / fC)
				avg.Apps.Mysql.RowLockWaitsPS = roundF(rowLkWaitsPS / fC)
			}
		}

		// Custom metric values
		for group, metrics := range avg.Apps.Custom {
			for mi := range metrics {
				var sum float64
				count := 0
				for _, s := range samples {
					if sMetrics, ok := s.Apps.Custom[group]; ok {
						for _, sm := range sMetrics {
							if sm.Name == metrics[mi].Name {
								sum += sm.Value
								count++
								break
							}
						}
					}
				}
				if count > 0 {
					avg.Apps.Custom[group][mi].Value = roundF(sum / float64(count))
				}
			}
		}
	} else {
		// Single sample — min and max equal the observed values
		minCopy := *last
		minS = &minCopy
		maxCopy := *last
		maxS = &maxCopy
	}

	return &AggregatedSample{
		Timestamp: last.Timestamp,
		Duration:  dur,
		Data:      &avg,
		Min:       minS,
		Max:       maxS,
	}
}

func (s *Store) aggregateAggregated(samples []*AggregatedSample, dur time.Duration) *AggregatedSample {
	if len(samples) == 0 {
		return nil
	}

	raw := make([]*collector.Sample, 0, len(samples))
	for _, s := range samples {
		if s.Data != nil {
			raw = append(raw, s.Data)
		}
	}
	result := s.aggregateSamples(raw, dur)
	if result == nil {
		return nil
	}

	hasAggregatedMinMax := false
	for _, s := range samples {
		if s.Min != nil || s.Max != nil {
			hasAggregatedMinMax = true
			break
		}
	}

	if !hasAggregatedMinMax {
		// These are raw tier-0 samples, aggregateSamples already computed
		// the true min and max accurately. Return it as is.
		return result
	}

	var minS, maxS *collector.Sample
	if len(samples) > 0 {
		minS = samples[0].Min
		if minS == nil {
			minS = samples[0].Data
		}
		maxS = samples[0].Max
		if maxS == nil {
			maxS = samples[0].Data
		}
	}

	for _, s := range samples {
		candMin := s.Min
		if candMin == nil {
			candMin = s.Data
		}
		candMax := s.Max
		if candMax == nil {
			candMax = s.Data
		}
		minS = minSample(minS, candMin)
		maxS = maxSample(maxS, candMax)
	}

	result.Min = minS
	result.Max = maxS
	return result
}
