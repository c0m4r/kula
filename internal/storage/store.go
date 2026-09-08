package storage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	tier1Buf   []*AggregatedSample
	tier2Count int
	tier2Buf   []*AggregatedSample

	// latestCache holds the most recently written sample in memory.
	// QueryLatest copies one sample rather than scanning the tier file.
	latestCache *AggregatedSample
	// Changes after every successful raw write, including failed rollup writes.
	writeGeneration uint64

	// queryCache is a short-lived in-process cache for QueryRangeWithMeta.
	// It reuses completed results of identical API calls. Entries carry a TTL of
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

// queryCacheKey identifies a query with its exact bounds and point budget.
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
		var pending []*AggregatedSample
		for _, as := range t0Samples {
			if as.Timestamp.After(t1Newest) {
				if as.Data != nil {
					pending = append(pending, as)
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
	// Own the complete sample graph once this call returns. Callers must not
	// mutate their input concurrently with the call itself.
	owned := cloneAggregatedSample(&AggregatedSample{Data: sample})
	sample = owned.Data
	s.mu.Lock()
	defer s.mu.Unlock()

	// Use the actual tier-0 resolution as the default/fallback duration.
	fallbackDur := s.configs[0].Resolution
	dur := fallbackDur
	if s.latestCache != nil {
		dur = sample.Timestamp.Sub(s.latestCache.Timestamp)
		// A long collection gap is missing coverage, not evidence that the
		// next observation represented the entire outage. Keep normal ticker
		// jitter, but do not let a restart or suspend dominate weighted means.
		if dur <= 0 || dur > 2*fallbackDur {
			dur = fallbackDur
		}
	}

	as := &AggregatedSample{
		Timestamp:          sample.Timestamp,
		Duration:           dur,
		Data:               sample,
		AggregationVersion: currentAggregationVersion,
	}

	if len(s.tiers) > 0 {
		if err := s.tiers[0].Write(as); err != nil {
			return fmt.Errorf("writing tier 0: %w", err)
		}
		// Update the in-memory cache so QueryLatest never needs a disk scan.
		s.latestCache = as
		s.writeGeneration++
		// Invalidate immediately: a later rollup failure must not leave stale
		// live history cached after the raw write succeeded.
		s.queryCacheMu.Lock()
		s.sweepQueryCacheLocked(time.Now(), sample.Timestamp.UnixNano())
		s.queryCacheMu.Unlock()
	}

	// Aggregate for tier 1 (every ratio1 samples)
	if s.ratio1 > 0 && len(s.tiers) > 1 {
		s.tier1Buf = append(s.tier1Buf, as)
		s.tier1Count++
		// Bound the buffer in case tier-1 writes keep failing (a stuck/full
		// disk): without this the buffer would grow every tick forever. Drops
		// the oldest excess; those intervals stay available as raw tier-0 data.
		if cap1 := s.ratio1 * maxAggBufferFactor; len(s.tier1Buf) > cap1 {
			s.tier1Buf = append([]*AggregatedSample(nil), s.tier1Buf[len(s.tier1Buf)-cap1:]...)
			s.tier1Count = len(s.tier1Buf)
		}

		if s.tier1Count >= s.ratio1 {
			return s.flushRollup(1)
		}
	}

	return nil
}

// flushRollup preserves each contiguous run as its own record. A count-based
// window can straddle suspend/restart or a missed collection interval; averaging
// the entire buffer would relocate pre-outage measurements to its final minute.
// Partial records retain their observed Duration in the existing codec.
func (s *Store) flushRollup(tier int) error {
	buffer, count := &s.tier1Buf, &s.tier1Count
	if tier == 2 {
		buffer, count = &s.tier2Buf, &s.tier2Count
	}
	for len(*buffer) > 0 {
		end := 1
		for end < len(*buffer) {
			previous, next := (*buffer)[end-1], (*buffer)[end]
			width := next.Duration
			if width <= 0 {
				width = s.configs[tier-1].Resolution
			}
			if !next.Timestamp.After(previous.Timestamp) || next.Timestamp.Add(-width).After(previous.Timestamp) {
				break
			}
			end++
		}
		agg := s.aggregateAggregated((*buffer)[:end], s.configs[tier].Resolution)
		if err := s.tiers[tier].Write(agg); err != nil {
			return fmt.Errorf("writing tier %d: %w", tier, err)
		}
		*buffer = (*buffer)[end:]
		*count = len(*buffer)
		if *count == 0 {
			*buffer = nil
		}
		if tier == 1 && s.ratio2 > 0 && len(s.tiers) > 2 {
			s.tier2Buf = append(s.tier2Buf, agg)
			s.tier2Count++
			if cap2 := s.ratio2 * maxAggBufferFactor; len(s.tier2Buf) > cap2 {
				s.tier2Buf = append([]*AggregatedSample(nil), s.tier2Buf[len(s.tier2Buf)-cap2:]...)
				s.tier2Count = len(s.tier2Buf)
			}
			if s.tier2Count >= s.ratio2 {
				if err := s.flushRollup(2); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// sweepQueryCacheLocked drops expired query-cache entries. When liveEdgeNano > 0
// it also drops entries overlapping a newly written source interval. A rollup
// ending after a query's end can now contribute to that query, including when
// it replaces a partial result from another tier. The caller must hold s.mu
// and s.queryCacheMu.
func (s *Store) sweepQueryCacheLocked(now time.Time, liveEdgeNano int64) {
	var lookback time.Duration
	for i, tc := range s.configs {
		lookback = max(lookback, historyMaxSourceWidth(tc.Resolution, i == 0))
	}
	for k, e := range s.queryCache {
		if !now.Before(e.expiresAt) || (liveEdgeNano > 0 && k.toNano >= liveEdgeNano-int64(lookback)) {
			delete(s.queryCache, k)
		}
	}
}

// HistoryResult wraps query results with tier metadata for the API.
type HistoryResult struct {
	Samples          []*AggregatedSample `json:"samples"`
	Tier             int                 `json:"tier"`
	Resolution       string              `json:"resolution"`
	SourceResolution string              `json:"source_resolution"`
	Downsampled      bool                `json:"downsampled"`
	RequestedFrom    time.Time           `json:"requested_from"`
	RequestedTo      time.Time           `json:"requested_to"`
	ActualFrom       *time.Time          `json:"actual_from,omitempty"`
	ActualTo         *time.Time          `json:"actual_to,omitempty"`
	// Complete is the retention-selection result and tolerates normal live
	// collection/rollup lag. ExactComplete describes the literal requested
	// edges and is the value exact/custom views should present to users.
	Complete          bool     `json:"complete"`
	ExactComplete     bool     `json:"exact_complete"`
	ValidAggregations []string `json:"valid_aggregations"`
}

// validHistoryAggregations exposes extrema only when every returned bucket was
// produced by the exhaustive policy reducer. Old tier files remain readable,
// but their incomplete legacy envelopes stay hidden until retention replaces
// them (or a query recomputes buckets directly from raw observations).
func validHistoryAggregations(samples []*AggregatedSample) []string {
	for _, sample := range samples {
		if sample == nil || sample.Min == nil || sample.Max == nil ||
			sample.AggregationVersion < currentAggregationVersion {
			return []string{"data"}
		}
	}
	if len(samples) == 0 {
		return []string{"data"}
	}
	return []string{"data", "min", "max"}
}

type queryTierCandidate struct {
	index      int
	resolution time.Duration
	overlap    time.Duration
	complete   bool
}

// tierCoverageTolerances allow for bucket-end timestamps and normal collection
// lag without hiding meaningful retention loss. The oldest record may sit one
// resolution after the effective start of its bucket. At the live edge, a
// completed rollup can be almost one full resolution old before collection and
// scheduling delay are counted, so allow two resolutions.
func tierCoverageTolerances(resolution time.Duration) (left, right time.Duration) {
	return resolution, 2 * resolution
}

func cloneHistoryResult(result *HistoryResult) *HistoryResult {
	if result == nil {
		return nil
	}
	cp := *result
	cp.Samples = make([]*AggregatedSample, len(result.Samples))
	for i, sample := range result.Samples {
		cp.Samples[i] = cloneAggregatedSample(sample)
	}
	cp.ValidAggregations = append([]string(nil), result.ValidAggregations...)
	if result.ActualFrom != nil {
		actualFrom := *result.ActualFrom
		cp.ActualFrom = &actualFrom
	}
	if result.ActualTo != nil {
		actualTo := *result.ActualTo
		cp.ActualTo = &actualTo
	}
	return &cp
}

// QueryRange returns samples for a time range, choosing the best tier.
func (s *Store) QueryRange(from, to time.Time) ([]*AggregatedSample, error) {
	result, err := s.QueryRangeWithMeta(from, to, 450)
	if err != nil {
		return nil, err
	}
	return result.Samples, nil
}

// planHistoryQuery snapshots tier selection and cache state under a scoped
// read lock. Callers clone cache hits and reduce history after the lock is
// released, so neither operation holds up collection.
func (s *Store) planHistoryQuery(from, to time.Time, cacheKey queryCacheKey) ([]queryTierCandidate, *HistoryResult, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	generation := s.writeGeneration

	s.queryCacheMu.Lock()
	if entry, ok := s.queryCache[cacheKey]; ok {
		if time.Now().Before(entry.expiresAt) {
			s.queryCacheMu.Unlock()
			return nil, entry.result, generation
		}
		// Expired — drop it and fall through to recompute.
		delete(s.queryCache, cacheKey)
	}
	s.queryCacheMu.Unlock()

	duration := to.Sub(from)
	var fullCandidates, partialCandidates []queryTierCandidate

	// Classify every overlapping tier before applying the source-density
	// preference. This prevents a partial fine tier from winning merely because
	// it appears first, and prevents a complete fine tier from being skipped for
	// a coarser tier that does not cover the request either.
	for tierIdx := 0; tierIdx < len(s.tiers); tierIdx++ {
		tier := s.tiers[tierIdx]
		if tier.Count() == 0 {
			continue
		}

		oldest := tier.OldestTimestamp()
		newest := tier.NewestTimestamp()
		resDur := s.configs[tierIdx].Resolution
		// Candidate overlap uses interval starts, not only record endpoints.
		// Raw widths are checked precisely after decoding the bounded lookahead.
		oldestStart := oldest.Add(-historyMaxSourceWidth(resDur, tierIdx == 0))
		if oldestStart.After(to) || newest.Before(from) {
			continue
		}

		leftTolerance, rightTolerance := tierCoverageTolerances(resDur)
		complete := !oldest.After(from.Add(leftTolerance)) &&
			!newest.Before(to.Add(-rightTolerance))

		overlapFrom := from
		if oldestStart.After(overlapFrom) {
			overlapFrom = oldestStart
		}
		overlapTo := to
		if newest.Before(overlapTo) {
			overlapTo = newest
		}

		candidate := queryTierCandidate{
			index:      tierIdx,
			resolution: resDur,
			overlap:    overlapTo.Sub(overlapFrom),
			complete:   complete,
		}
		if complete {
			fullCandidates = append(fullCandidates, candidate)
		} else {
			partialCandidates = append(partialCandidates, candidate)
		}
	}

	// Partial results prefer the tier covering the largest fraction of the
	// requested range. Stable sorting retains the finer tier when overlap ties.
	sort.SliceStable(partialCandidates, func(i, j int) bool {
		return partialCandidates[i].overlap > partialCandidates[j].overlap
	})

	// Source quality is independent of display density: requested point count
	// must never unlock a finer tier. Skip a dense tier only at the fixed read
	// budget and only when another fully covering tier is available.
	firstFull := 0
	for firstFull < len(fullCandidates)-1 {
		candidate := fullCandidates[firstFull]
		if estimatedSourceRecords(duration, candidate.resolution) <= maxHistorySourceRecords {
			break
		}
		firstFull++
	}

	candidates := make([]queryTierCandidate, 0, len(fullCandidates)+len(partialCandidates))
	if len(fullCandidates) > 0 {
		candidates = append(candidates, fullCandidates[firstFull:]...)
		candidates = append(candidates, fullCandidates[:firstFull]...)
	}
	candidates = append(candidates, partialCandidates...)
	return candidates, nil, generation
}

// QueryRangeWithMeta returns samples with tier and coverage metadata.
// It prefers a fully covering tier whose source density is reasonable. A tier
// is complete when both requested edges are within its collection-lag
// tolerance. If no tier is complete, the tier with the greatest overlap is
// returned with Complete=false rather than silently presenting it as complete.
// Results are cached for the duration of one tier-0 resolve cycle to serve
// concurrent or repeated API calls without extra disk I/O.
func (s *Store) QueryRangeWithMeta(from, to time.Time, targetPoints int) (*HistoryResult, error) {
	// Tier handles and their configuration are immutable after construction.
	if len(s.tiers) == 0 {
		return &HistoryResult{
			RequestedFrom:     from,
			RequestedTo:       to,
			ValidAggregations: validHistoryAggregations(nil),
		}, nil
	}

	const maxScreenPoints = 7200
	if targetPoints <= 0 {
		targetPoints = 450
	} else if targetPoints > maxScreenPoints {
		targetPoints = maxScreenPoints
	}

	// Exact bounds are part of the result: coalescing sub-second requests can
	// return samples outside the caller's interval even when their display
	// buckets happen to be identical.
	cacheKey := queryCacheKey{
		fromNano:     from.UnixNano(),
		toNano:       to.UnixNano(),
		targetPoints: targetPoints,
	}
	candidates, cachedResult, generation := s.planHistoryQuery(from, to, cacheKey)
	if cachedResult != nil {
		// Cache entries are immutable. Copy without serializing other hits.
		return cloneHistoryResult(cachedResult), nil
	}

	// Each scanner protects its own snapshot and releases the tier lock
	// between reduction batches. No store lock is held during reduction.
	for _, candidate := range candidates {
		tierIdx := candidate.index
		tier := s.tiers[tierIdx]

		result, err := s.readHistory(tier, from, to, targetPoints, candidate.resolution, tierIdx == 0)
		if errors.Is(err, errHistorySnapshotExpired) {
			// Retention advanced across unread bytes. Restart from the current
			// snapshot once, without ever publishing the abandoned partial data.
			result, err = s.readHistory(tier, from, to, targetPoints, candidate.resolution, tierIdx == 0)
			candidate.complete = false
		}
		if err != nil {
			return nil, fmt.Errorf("reading tier %d: %w", tierIdx, err)
		}
		// A fully retained raw interval with no observations is an outage.
		// Falling back to a legacy coarse rollup can invent coverage there.
		if len(result.Samples) == 0 && (tierIdx != 0 || !candidate.complete) {
			continue
		}

		if len(result.Samples) > targetPoints {
			return nil, fmt.Errorf("downsampling tier %d returned %d samples for target %d", tierIdx, len(result.Samples), targetPoints)
		}

		result.Tier = tierIdx
		result.SourceResolution = fmtRes(candidate.resolution)
		result.RequestedFrom = from
		result.RequestedTo = to
		result.Complete = candidate.complete && len(result.Samples) > 0
		result.ExactComplete = result.ActualFrom != nil && result.ActualTo != nil &&
			!result.ActualFrom.After(from) && !result.ActualTo.Before(to)
		result.ValidAggregations = validHistoryAggregations(result.Samples)

		// Cache with a TTL of one tier-0 resolution. The cap is a safety bound:
		// if it's reached we sweep expired entries first and, failing that, skip
		// caching rather than let the map grow without limit.
		cached := cloneHistoryResult(result)
		s.mu.RLock()
		defer s.mu.RUnlock()
		// A write during reduction already invalidated the old snapshot. Do
		// not reinsert it after that invalidation.
		if s.writeGeneration != generation {
			return result, nil
		}
		s.queryCacheMu.Lock()
		if len(s.queryCache) >= maxQueryCacheEntries {
			s.sweepQueryCacheLocked(time.Now(), 0)
		}
		if len(s.queryCache) < maxQueryCacheEntries {
			s.queryCache[cacheKey] = queryCacheEntry{
				result:    cached,
				expiresAt: time.Now().Add(s.queryCacheTTL),
			}
		}
		s.queryCacheMu.Unlock()

		return result, nil
	}

	// No data found in any tier
	res := fmtRes(s.configs[0].Resolution)
	return &HistoryResult{
		Tier:              0,
		Resolution:        res,
		SourceResolution:  res,
		RequestedFrom:     from,
		RequestedTo:       to,
		ValidAggregations: validHistoryAggregations(nil),
	}, nil
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
	// Return an independent sample graph so callers cannot mutate the cache.
	if s.latestCache != nil {
		return cloneAggregatedSample(s.latestCache), nil
	}

	// Cold path: only reached on an empty store where no sample has been
	// written yet this process lifetime and warmLatestCache found nothing.
	return nil, nil
}

// RetainedRange is a tier's timestamp envelope, not a guarantee of gap-free data.
type RetainedRange struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// RetainedRanges reads only in-memory tier headers. Calendar constraints must
// not scan/decode tier files or hold up collection for a full retention scan.
func (s *Store) RetainedRanges() ([]RetainedRange, time.Duration) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ranges := make([]RetainedRange, 0, len(s.tiers))
	interval := time.Second
	if len(s.configs) > 0 {
		interval = s.configs[0].Resolution
	}
	for i, tier := range s.tiers {
		if tier.Count() > 0 {
			// Records are end-timestamped, so the oldest retained interval begins
			// one native tier resolution before its record timestamp.
			from := tier.OldestTimestamp()
			if i < len(s.configs) {
				from = from.Add(-s.configs[i].Resolution)
			}
			ranges = append(ranges, RetainedRange{From: from, To: tier.NewestTimestamp()})
		}
	}
	return ranges, interval
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
