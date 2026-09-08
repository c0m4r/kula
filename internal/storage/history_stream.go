package storage

import (
	"sort"
	"time"
)

// readHistory keeps at most one decoded batch, a native-output buffer bounded
// by targetPoints, and the reduced output buckets. Every source observation
// contributes; crossing a batch boundary never drops extrema or sparse weights.
func (s *Store) readHistory(tier *Tier, from, to time.Time, targetPoints int, resolution time.Duration, raw bool) (*HistoryResult, error) {
	step := chooseHistoryStep(from, to, targetPoints, resolution)
	result := &HistoryResult{}
	var pending []*AggregatedSample
	var count int
	reducing := step > resolution
	buckets := make(map[int64]*AggregatedSample)
	add := func(batch []*AggregatedSample) {
		for _, partial := range s.reduceHistoryBuckets(batch, from, to, targetPoints, resolution, raw, step) {
			key := partial.BucketStart.UnixNano()
			if previous := buckets[key]; previous != nil {
				merged := s.aggregateAggregated([]*AggregatedSample{previous, partial}, 0)
				setHistoryBucketMetadata(merged, partial.BucketStart, partial.BucketEnd, previous.SampleCount+partial.SampleCount)
				buckets[key] = merged
			} else {
				buckets[key] = partial
			}
		}
	}
	// End-timestamped records beyond to can still cover the right edge. Keep
	// the low-level scanner's timestamp contract and bound history's lookahead
	// by the maximum source interval, then discard nonoverlapping lookahead.
	scanTo := to.Add(historyMaxSourceWidth(resolution, raw))
	_, err := tier.scanRange(from, scanTo, historyDecodeBatchSize, func(batch []*AggregatedSample) error {
		selected := batch[:0]
		clipRight := false
		for _, sample := range batch {
			if sample.Timestamp.After(to) {
				start := sample.Timestamp.Add(-historySourceWidth(sample, resolution, raw))
				if !to.After(from) || sample.Data == nil || !start.Before(to) {
					continue
				}
				clipRight = true
			}
			selected = append(selected, sample)
		}
		batch = selected
		left, right := observedHistoryBounds(batch, from, to, resolution, raw)
		if left != nil && (result.ActualFrom == nil || left.Before(*result.ActualFrom)) {
			result.ActualFrom = left
		}
		if right != nil && (result.ActualTo == nil || right.After(*result.ActualTo)) {
			result.ActualTo = right
		}
		count += len(batch)
		// A right-boundary record must be clipped even at native resolution:
		// returning its original endpoint would put it outside the viewport.
		if !reducing && (count > targetPoints || clipRight) {
			reducing = true
			add(pending)
			pending = nil
		}
		if reducing {
			add(batch)
		} else {
			pending = append(pending, batch...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if reducing {
		keys := make([]int64, 0, len(buckets))
		for key := range buckets {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		for _, key := range keys {
			result.Samples = append(result.Samples, buckets[key])
		}
	} else {
		setSourceBucketMetadata(pending, resolution, raw)
		result.Samples = pending
		step = resolution
	}
	result.Resolution = fmtRes(step)
	result.Downsampled = reducing
	return result, nil
}
