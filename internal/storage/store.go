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
	// readOnly marks a store opened with OpenReadOnly: another process owns
	// the tier files, and every write path returns ErrReadOnly.
	readOnly bool

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

// queryCacheKey identifies a query with its exact bounds, point budget and
// tier hint (stored plus one, so the zero value means none).
type queryCacheKey struct {
	fromNano     int64
	toNano       int64
	targetPoints int
	tierHint     int
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

	// Restore the latest-sample cache, so QueryLatest is O(1) from the first
	// call, and any partially aggregated rollup buffers, so intervals are not
	// dropped across a restart. Each tier's seek index lets this read only the
	// tails of tiers 0 and 1.
	s.restoreTailState()

	return s, nil
}

// restoreTailState reads the newest records of the lower tiers once: tier 0's
// tail supplies both the latest-sample cache and the samples not yet rolled
// into tier 1, and tier 1's tail the rollups not yet rolled into tier 2.
//
// Pending records are replayed through the same windowing as WriteSample,
// clipped where a record already rolled up straddles the newest rollup's end.
// Tier 2 is restored first, so a tier-1 window the replay finds complete (a
// crash between the raw write that closed it and its rollup) cascades into
// it as it would have.
func (s *Store) restoreTailState() {
	if len(s.tiers) == 0 {
		return
	}
	tail := 1
	if len(s.tiers) > 1 && s.ratio1 > 0 {
		tail = s.ratio1 * maxAggBufferFactor
	}
	t0Samples, err := s.tiers[0].ReadLatest(tail)
	if err != nil {
		return
	}
	if len(t0Samples) > 0 {
		s.latestCache = t0Samples[len(t0Samples)-1]
	}
	if len(s.tiers) <= 1 || s.ratio1 <= 0 {
		return
	}

	if len(s.tiers) > 2 && s.ratio2 > 0 {
		t2Newest := s.tiers[2].NewestTimestamp()
		if t1Samples, err := s.tiers[1].ReadLatest(s.ratio2 * maxAggBufferFactor); err == nil {
			for _, as := range t1Samples {
				if as.Data != nil && as.Timestamp.After(t2Newest) {
					s.restorePending(2, as, t2Newest)
				}
			}
		}
	}

	t1Newest := s.tiers[1].NewestTimestamp()
	for _, as := range t0Samples {
		if as.Data != nil && as.Timestamp.After(t1Newest) {
			as.ExtremaProfile = "raw"
			s.restorePending(1, as, t1Newest)
		}
	}
}

// restorePending feeds one record read back from disk into tier's rollup
// buffer, keeping only its part after rolledUp, the end of the newest record
// already rolled into tier. A failed rollup write is reported and retried
// with the next window, as in WriteSample.
func (s *Store) restorePending(tier int, as *AggregatedSample, rolledUp time.Time) {
	width := historySourceWidth(as, s.configs[tier-1].Resolution, tier == 1)
	if start := as.Timestamp.Add(-width); !rolledUp.IsZero() && start.Before(rolledUp) {
		as = rollupFragment(as, rolledUp, as.Timestamp, width)
	}
	if err := s.addToRollup(tier, as); err != nil {
		fmt.Printf("Storage: restoring pending tier-%d rollups: %v\n", tier, err)
	}
}

// WriteSample writes a raw sample to tier 0 and triggers aggregation.
func (s *Store) WriteSample(sample *collector.Sample) error {
	if s.readOnly {
		return ErrReadOnly
	}
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
		ExtremaProfile:     "raw",
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

	// Aggregate for tier 1, one rollup per wall-clock window.
	if s.ratio1 > 0 && len(s.tiers) > 1 {
		return s.addToRollup(1, as)
	}

	return nil
}

// ---- Rollup windows ----------------------------------------------------------
//
// Tier 1 and tier 2 roll up Unix-epoch-aligned windows of their resolution
// (whole minutes, whole five minutes), not every ratio-th record since the
// process started. History buckets are epoch-aligned too, so a native rollup
// is exactly one output bucket: a rolling view, which returns native records,
// and a past window, which re-buckets them, show the same values. With
// process-aligned windows the two split every interval differently, and a
// spike could read up to half as high in one as in the other.
//
// Source intervals are end-timestamped, and a raw sample usually straddles a
// window boundary by a fraction of a second, so each source record is split at
// the boundaries inside it, its duration and sparse mean weights apportioned
// like a history bucket fragment. A rollup then ends exactly on its window's
// end. A window is flushed as soon as a record ends on that end, or else
// (after an outage) when a record of a later window arrives.
// ------------------------------------------------------------------------------

// rollupWindow returns the index of tier's window holding an interval that
// ends at end: windows are end-inclusive, like history buckets.
func (s *Store) rollupWindow(tier int, end time.Time) int64 {
	return epochBucketIndex(end.Add(-time.Nanosecond), s.configs[tier].Resolution)
}

func (s *Store) rollupBuffer(tier int) (*[]*AggregatedSample, *int) {
	if tier == 2 {
		return &s.tier2Buf, &s.tier2Count
	}
	return &s.tier1Buf, &s.tier1Count
}

func (s *Store) rollupRatio(tier int) int {
	if tier == 2 {
		return s.ratio2
	}
	return s.ratio1
}

// addToRollup buffers a source record for tier (raw samples for tier 1,
// tier-1 rollups for tier 2), split at tier's window boundaries, and flushes
// every window it closes. Caller holds s.mu.
func (s *Store) addToRollup(tier int, as *AggregatedSample) error {
	if as == nil || as.Data == nil {
		return nil
	}
	buffer, count := s.rollupBuffer(tier)
	var firstErr error
	for _, part := range s.rollupParts(tier, as) {
		window := s.rollupWindow(tier, part.Timestamp)
		if n := len(*buffer); n > 0 && s.rollupWindow(tier, (*buffer)[n-1].Timestamp) != window {
			if err := s.flushRollup(tier); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		*buffer = append(*buffer, part)
		// Bound the buffer in case this tier's writes keep failing (a
		// stuck/full disk): without this it would grow every tick forever.
		// Drops the oldest excess; those intervals stay available in the
		// tier below.
		if limit := max(1, s.rollupRatio(tier)) * maxAggBufferFactor; len(*buffer) > limit {
			*buffer = append([]*AggregatedSample(nil), (*buffer)[len(*buffer)-limit:]...)
		}
		*count = len(*buffer)
		// A part ending exactly on its window's end completes the window:
		// every later record ends after it.
		if part.Timestamp.Equal(s.rollupWindowEnd(tier, window)) {
			if err := s.flushRollup(tier); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// rollupWindowEnd returns the end of tier's window with the given index.
func (s *Store) rollupWindowEnd(tier int, window int64) time.Time {
	return time.Unix(0, (window+1)*int64(s.configs[tier].Resolution))
}

// rollupParts splits a source record at the window boundaries of tier inside
// its interval. Most records lie within one window and are returned as is.
func (s *Store) rollupParts(tier int, as *AggregatedSample) []*AggregatedSample {
	step := s.configs[tier].Resolution
	width := historySourceWidth(as, s.configs[tier-1].Resolution, tier == 1)
	if step <= 0 || width <= 0 {
		return []*AggregatedSample{as}
	}
	start, end := as.Timestamp.Add(-width), as.Timestamp
	boundary := epochBucketStart(start, step).Add(step)
	if !boundary.Before(end) {
		return []*AggregatedSample{as}
	}
	var parts []*AggregatedSample
	cursor := start
	for ; boundary.Before(end); boundary = boundary.Add(step) {
		parts = append(parts, rollupFragment(as, cursor, boundary, width))
		cursor = boundary
	}
	return append(parts, rollupFragment(as, cursor, end, width))
}

// rollupFragment returns the part of as covering (from, to], where width is
// the length of as's whole interval. The duration and sparse mean weights
// shrink with it; values and extrema envelopes are the record's own.
func rollupFragment(as *AggregatedSample, from, to time.Time, width time.Duration) *AggregatedSample {
	fraction := float64(to.Sub(from)) / float64(width)
	part := *as
	part.Timestamp = to
	if as.Duration == width {
		part.Duration = to.Sub(from)
	} else {
		part.Duration = time.Duration(float64(as.Duration) * fraction)
	}
	if part.Duration <= 0 {
		part.Duration = time.Nanosecond
	}
	if len(as.MeanStats) > 0 {
		part.MeanStats = make(map[string]meanAccumulator, len(as.MeanStats))
		for path, stat := range as.MeanStats {
			part.MeanStats[path] = meanAccumulator{Sum: stat.Sum * fraction, Weight: stat.Weight * fraction}
		}
	}
	return &part
}

// flushRollup writes the buffered windows of tier, one record per window and
// contiguous run: a window can straddle suspend/restart or a missed collection
// interval, and averaging across the gap would relocate pre-outage
// measurements to its end. Partial records retain their observed Duration in
// the existing codec. Each tier-1 record written feeds tier 2. On a write
// error the unwritten records stay buffered for the next attempt.
func (s *Store) flushRollup(tier int) error {
	buffer, count := s.rollupBuffer(tier)
	for len(*buffer) > 0 {
		end := 1
		for end < len(*buffer) {
			previous, next := (*buffer)[end-1], (*buffer)[end]
			width := next.Duration
			if width <= 0 {
				width = s.configs[tier-1].Resolution
			}
			if !next.Timestamp.After(previous.Timestamp) || next.Timestamp.Add(-width).After(previous.Timestamp) ||
				s.rollupWindow(tier, next.Timestamp) != s.rollupWindow(tier, previous.Timestamp) {
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
			if err := s.addToRollup(2, agg); err != nil {
				return err
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
	// Availability is a union. Clients must check each bucket's profile;
	// valid_aggregations keeps its strict contract for existing clients.
	AvailableAggregations []string            `json:"available_aggregations,omitempty"`
	ExtremaProfiles       map[string][]string `json:"extrema_profiles,omitempty"`
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

// rolledUpThrough returns how far coarse tier's rollups are final: its newest
// rollup, or the start of the window that holds the source tier's newest
// record when that is later. Every earlier window has closed, and one with
// no rollup closed with nothing to roll up, as after a downtime: a restart
// would otherwise leave tier 1 short of the live edge until the first minute
// after it ends. A window still buffered before that one (a failed write)
// keeps the newest rollup. Caller holds s.mu.
func (s *Store) rolledUpThrough(tier int) time.Time {
	newest := s.tiers[tier].NewestTimestamp()
	if tier <= 0 || s.rollupRatio(tier) <= 0 {
		return newest
	}
	var source time.Time
	if tier == 1 {
		source = s.tiers[0].NewestTimestamp()
	} else {
		source = s.rolledUpThrough(tier - 1)
	}
	if source.IsZero() {
		return newest
	}
	window := s.rollupWindow(tier, source)
	if buffer, _ := s.rollupBuffer(tier); len(*buffer) > 0 && s.rollupWindow(tier, (*buffer)[0].Timestamp) < window {
		return newest
	}
	if closed := s.rollupWindowEnd(tier, window-1); closed.After(newest) {
		return closed
	}
	return newest
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
	cp.AvailableAggregations = append([]string(nil), result.AvailableAggregations...)
	if result.ExtremaProfiles != nil {
		cp.ExtremaProfiles = make(map[string][]string, len(result.ExtremaProfiles))
		for profile, fields := range result.ExtremaProfiles {
			cp.ExtremaProfiles[profile] = append([]string{}, fields...)
		}
	}
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
func (s *Store) planHistoryQuery(from, to time.Time, cacheKey queryCacheKey, tierHint int) ([]queryTierCandidate, *HistoryResult, uint64) {
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

		// A coarse tier covers the live edge as far as its rollups are final,
		// which may be past its newest rollup (see rolledUpThrough).
		coveredTo := newest
		if tierIdx != 0 {
			coveredTo = s.rolledUpThrough(tierIdx)
		}
		leftTolerance, rightTolerance := tierCoverageTolerances(resDur)
		complete := !oldest.After(from.Add(leftTolerance)) &&
			!coveredTo.Before(to.Add(-rightTolerance))

		// Rank coverage from where the tier certainly holds data. A coarse
		// tier's oldest rollup may be partial — the first one after a fresh
		// start nearly always is, since rollup windows are wall-clock aligned
		// — and its start is unknown without decoding it, so count from its
		// end. Its nominal width would let one partial minute outrank raw
		// data covering most of that minute.
		coveredFrom := oldestStart
		if tierIdx != 0 {
			coveredFrom = oldest
		}
		overlapFrom := from
		if coveredFrom.After(overlapFrom) {
			overlapFrom = coveredFrom
		}
		overlapTo := to
		if coveredTo.Before(overlapTo) {
			overlapTo = coveredTo
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
	// A hinted tier that retains part of the range is tried first, complete
	// or not, as a full view of the longer window would have used it.
	for i, candidate := range candidates {
		if candidate.index == tierHint && i > 0 {
			copy(candidates[1:i+1], candidates[:i])
			candidates[0] = candidate
			break
		}
	}
	return candidates, nil, generation
}

// cacheHistoryResult caches a copy of result for one tier-0 resolution, unless
// a write since the query was planned (generation) has already invalidated
// what it was read from. The cap is a safety bound: when it is reached,
// expired entries are swept first and, failing that, nothing is cached rather
// than letting the map grow without limit.
func (s *Store) cacheHistoryResult(key queryCacheKey, generation uint64, result *HistoryResult) {
	cached := cloneHistoryResult(result)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.writeGeneration != generation {
		return
	}
	s.queryCacheMu.Lock()
	defer s.queryCacheMu.Unlock()
	if len(s.queryCache) >= maxQueryCacheEntries {
		s.sweepQueryCacheLocked(time.Now(), 0)
	}
	if len(s.queryCache) < maxQueryCacheEntries {
		s.queryCache[key] = queryCacheEntry{
			result:    cached,
			expiresAt: time.Now().Add(s.queryCacheTTL),
		}
	}
}

// QueryRangeWithMeta returns samples with tier and coverage metadata.
// It prefers a fully covering tier whose source density is reasonable. A tier
// is complete when both requested edges are within its collection-lag
// tolerance. If no tier is complete, the tier with the greatest overlap is
// returned with Complete=false rather than silently presenting it as complete.
// Results are cached for one tier-0 resolution under their exact bounds, so a
// concurrent or repeated identical call (a past window, not a refresh ending
// now) is served without extra disk I/O.
func (s *Store) QueryRangeWithMeta(from, to time.Time, targetPoints int) (*HistoryResult, error) {
	return s.QueryRangeFromTier(from, to, targetPoints, -1)
}

// QueryRangeFromTier is QueryRangeWithMeta with a tier hint: when tier retains
// part of [from, to] it is tried first, before the tier the window alone would
// select. The dashboard refreshes the newest buckets of a long rolling view
// this way, so that a few minutes of a view built from tier 1 are answered
// from tier 1 too, not from the raw tier that covers them. A negative or
// unknown tier is no hint, and the result always names the tier it came from.
func (s *Store) QueryRangeFromTier(from, to time.Time, targetPoints, tier int) (*HistoryResult, error) {
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
	if tier < 0 || tier >= len(s.tiers) {
		tier = -1
	}
	cacheKey := queryCacheKey{
		fromNano:     from.UnixNano(),
		toNano:       to.UnixNano(),
		targetPoints: targetPoints,
		tierHint:     tier + 1,
	}
	candidates, cachedResult, generation := s.planHistoryQuery(from, to, cacheKey, tier)
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
		// Retention advanced across unread bytes. Restart from the current
		// snapshot, without ever publishing the abandoned partial data. A
		// read-only store first adopts the owner's current header, and allows
		// for the owner writing again during the retry.
		retries := 1
		if s.readOnly {
			retries = 3
		}
		for attempt := 0; attempt < retries && errors.Is(err, errHistorySnapshotExpired); attempt++ {
			if s.readOnly {
				if reloadErr := tier.reloadReadOnly(); reloadErr != nil {
					err = reloadErr
					break
				}
			}
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
		annotateHistoryAggregations(result)
		s.cacheHistoryResult(cacheKey, generation, result)
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

// QueryLatest returns the latest sample from tier 0.
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

// SnapshotTier writes a consistent copy of tier i to dst (see Tier.SnapshotTo,
// which holds the tier lock only briefly). The store's tier slice is immutable
// after construction; the brief store lock here just bounds-checks the index.
func (s *Store) SnapshotTier(i int, dst io.WriterAt) (int64, error) {
	s.mu.RLock()
	if i < 0 || i >= len(s.tiers) {
		s.mu.RUnlock()
		return 0, fmt.Errorf("tier index %d out of range (have %d tiers)", i, len(s.tiers))
	}
	t := s.tiers[i]
	s.mu.RUnlock()
	return t.SnapshotTo(dst)
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
