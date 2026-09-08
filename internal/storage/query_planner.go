package storage

import (
	"sort"
	"time"
)

// maxHistorySourceRecords is the source-tier selection target, independent of
// display density. Denser ranges are streamed in bounded batches, not rejected.
const maxHistorySourceRecords = 7500

const historyDecodeBatchSize = 512

var niceHistorySteps = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	15 * time.Second,
	30 * time.Second,
	1 * time.Minute,
	2 * time.Minute,
	5 * time.Minute,
	10 * time.Minute,
	15 * time.Minute,
	30 * time.Minute,
	1 * time.Hour,
	2 * time.Hour,
	3 * time.Hour,
	6 * time.Hour,
	12 * time.Hour,
	24 * time.Hour,
	48 * time.Hour,
	5 * 24 * time.Hour,
	10 * 24 * time.Hour,
	15 * 24 * time.Hour,
	30 * 24 * time.Hour,
}

func estimatedSourceRecords(duration, resolution time.Duration) int64 {
	if duration < 0 || resolution <= 0 {
		return 0
	}
	return int64(duration/resolution) + 1
}

// alignedBucketCount treats to as the exclusive end of the represented time
// interval. Source records are end-timestamped, so a record exactly at to
// contributes to the bucket ending there rather than starting a new bucket.
func alignedBucketCount(from, to time.Time, step time.Duration) int64 {
	if step <= 0 || to.Before(from) {
		return 0
	}
	if to.Equal(from) {
		return 1
	}
	return epochBucketIndex(to.Add(-time.Nanosecond), step) - epochBucketIndex(from, step) + 1
}

func epochBucketIndex(timestamp time.Time, step time.Duration) int64 {
	nanos := timestamp.UnixNano()
	stepNanos := int64(step)
	index := nanos / stepNanos
	if nanos < 0 && nanos%stepNanos != 0 {
		index--
	}
	return index
}

func epochBucketStart(timestamp time.Time, step time.Duration) time.Time {
	return time.Unix(0, epochBucketIndex(timestamp, step)*int64(step)).UTC()
}

func chooseHistoryStep(from, to time.Time, targetPoints int, sourceResolution time.Duration) time.Duration {
	if targetPoints < 1 {
		targetPoints = 1
	}
	if sourceResolution <= 0 {
		sourceResolution = time.Second
	}

	for _, step := range niceHistorySteps {
		if step < sourceResolution {
			continue
		}
		if alignedBucketCount(from, to, step) <= int64(targetPoints) {
			return step
		}
	}

	// The public API currently caps ranges at 31 days, so the table above is
	// normally sufficient. Continue geometrically for internal callers rather
	// than weakening the strict point bound.
	step := niceHistorySteps[len(niceHistorySteps)-1]
	for alignedBucketCount(from, to, step) > int64(targetPoints) {
		if step > time.Duration(1<<63-1)/2 {
			// time.Duration cannot represent a wider step. Public requests
			// cannot reach this path, but internal callers must still receive
			// a positive, terminating result for an extreme interval.
			return time.Duration(1<<63 - 1)
		}
		step *= 2
	}
	return step
}

func setHistoryBucketMetadata(sample *AggregatedSample, start, end time.Time, sampleCount int) {
	if sample == nil {
		return
	}
	sample.BucketStart = start
	sample.BucketEnd = end
	sample.SampleCount = sampleCount
	sample.Coverage = 0
	if width := end.Sub(start); sampleCount > 0 && width > 0 && sample.Duration > 0 {
		sample.Coverage = sample.Duration.Seconds() / width.Seconds()
		if sample.Coverage > 1 {
			sample.Coverage = 1
		}
	}
}

// historyMaxSourceWidth bounds lookahead without decoding tier payloads.
func historyMaxSourceWidth(sourceResolution time.Duration, raw bool) time.Duration {
	if raw {
		// WriteSample accepts ticker jitter up to twice the native interval.
		return 2 * sourceResolution
	}
	return sourceResolution
}

// Raw Duration is the observed collection interval (with outages bounded by
// WriteSample). Partial rollups must not reach back across an outage to fill
// their native tier width. Legacy full-width records retain their old bounds.
func historySourceWidth(sample *AggregatedSample, sourceResolution time.Duration, raw bool) time.Duration {
	if sample.Duration > 0 {
		if raw {
			return sample.Duration
		}
		return min(sample.Duration, sourceResolution)
	}
	return sourceResolution
}

func setSourceBucketMetadata(samples []*AggregatedSample, sourceResolution time.Duration, raw bool) {
	for _, sample := range samples {
		if sample == nil {
			continue
		}
		end := sample.Timestamp
		contributors := 0
		if sample.Data != nil {
			contributors = 1
		}
		setHistoryBucketMetadata(sample, end.Add(-historySourceWidth(sample, sourceResolution, raw)), end, contributors)
	}
}

// observedHistoryBounds returns the portion of the selected source buckets
// that intersects the requested interval. Source timestamps mark bucket ends;
// partial rollups use their contributing duration as a conservative width,
// capped at the native resolution, so they cannot extend across an outage.
func observedHistoryBounds(
	samples []*AggregatedSample,
	from, to time.Time,
	sourceResolution time.Duration, raw bool,
) (*time.Time, *time.Time) {
	if sourceResolution <= 0 {
		return nil, nil
	}

	var first, last time.Time
	for _, sample := range samples {
		if sample == nil || sample.Data == nil {
			continue
		}
		start := sample.Timestamp.Add(-historySourceWidth(sample, sourceResolution, raw))
		end := sample.Timestamp
		if !end.After(from) || !start.Before(to) {
			continue
		}
		if start.Before(from) {
			start = from
		}
		if end.After(to) {
			end = to
		}
		if first.IsZero() || start.Before(first) {
			first = start
		}
		if last.IsZero() || end.After(last) {
			last = end
		}
	}
	if first.IsZero() || last.IsZero() {
		return nil, nil
	}
	return &first, &last
}

// historyFragment returns the portion of sample contributing to one output
// bucket. Sparse sums/weights are scaled along with Duration so cascading
// means remain equivalent when a jittered source bucket crosses a boundary.
func historyFragment(sample *AggregatedSample, end time.Time, fraction float64) *AggregatedSample {
	fragment := *sample
	fragment.Timestamp = end
	fragment.Duration = time.Duration(float64(sample.Duration) * fraction)
	if fragment.Duration <= 0 {
		fragment.Duration = time.Nanosecond
	}
	fragment.BucketStart = time.Time{}
	fragment.BucketEnd = time.Time{}
	fragment.SampleCount = 0
	fragment.Coverage = 0
	if len(sample.MeanStats) > 0 && fraction != 1 {
		fragment.MeanStats = make(map[string]meanAccumulator, len(sample.MeanStats))
		for path, stat := range sample.MeanStats {
			fragment.MeanStats[path] = meanAccumulator{
				Sum:    stat.Sum * fraction,
				Weight: stat.Weight * fraction,
			}
		}
	}
	return &fragment
}

func (s *Store) downsampleHistory(
	samples []*AggregatedSample,
	from, to time.Time,
	targetPoints int,
	sourceResolution time.Duration,
	trustedRawSource bool,
) ([]*AggregatedSample, time.Duration) {
	step := chooseHistoryStep(from, to, targetPoints, sourceResolution)
	if step <= sourceResolution && len(samples) <= targetPoints {
		setSourceBucketMetadata(samples, sourceResolution, trustedRawSource)
		return samples, sourceResolution
	}
	return s.reduceHistoryBuckets(samples, from, to, targetPoints, sourceResolution, trustedRawSource, step), step
}

// reduceHistoryBuckets always uses the supplied epoch-aligned step, even for
// a final small batch. This keeps bucket identities stable across batch splits.
func (s *Store) reduceHistoryBuckets(samples []*AggregatedSample, from, to time.Time, targetPoints int, sourceResolution time.Duration, trustedRawSource bool, step time.Duration) []*AggregatedSample {
	capacity := len(samples)
	if targetPoints <= 0 {
		capacity = 0
	} else if targetPoints < capacity {
		capacity = targetPoints
	}
	downsampled := make([]*AggregatedSample, 0, capacity)
	// Source intervals can overlap because coarse rollups retain collection
	// jitter but no precise wall-clock start. Accumulate by bucket, rather than
	// flushing when the next fragment changes buckets: that can emit the same
	// bucket twice when adjacent source intervals straddle the same boundary.
	groups := make(map[int64][]*AggregatedSample)
	flush := func(bucketIndex int64, group []*AggregatedSample) {
		if len(group) == 0 {
			return
		}
		if aggregate := s.aggregateAggregated(group, 0); aggregate != nil {
			// Old raw tier records can be safely recomputed from Data, but a
			// legacy coarse record with no envelope is already lossy. Its shape
			// alone is indistinguishable from raw, so retain the tier-level trust
			// decision here instead of promoting it to reducer-v2 provenance.
			if !trustedRawSource {
				for _, input := range group {
					if input != nil && input.AggregationVersion < currentAggregationVersion {
						aggregate.AggregationVersion = 0
						break
					}
				}
			}
			contributors := 0
			for _, input := range group {
				if input != nil && input.Data != nil {
					contributors++
				}
			}
			bucketStart := time.Unix(0, bucketIndex*int64(step)).UTC()
			setHistoryBucketMetadata(aggregate, bucketStart, bucketStart.Add(step), contributors)
			downsampled = append(downsampled, aggregate)
		}
	}

	for _, sample := range samples {
		if sample == nil || sample.Data == nil || sourceResolution <= 0 {
			continue
		}

		sourceWidth := historySourceWidth(sample, sourceResolution, trustedRawSource)
		sourceStart := sample.Timestamp.Add(-sourceWidth)
		sourceEnd := sample.Timestamp
		start := sourceStart
		end := sourceEnd
		if start.Before(from) {
			start = from
		}
		if end.After(to) {
			end = to
		}
		if !end.After(start) {
			continue
		}

		for cursor := start; cursor.Before(end); {
			outputStart := epochBucketStart(cursor, step)
			outputEnd := outputStart.Add(step)
			segmentEnd := end
			if outputEnd.Before(segmentEnd) {
				segmentEnd = outputEnd
			}
			fraction := float64(segmentEnd.Sub(cursor)) / float64(sourceWidth)
			index := epochBucketIndex(outputStart, step)
			groups[index] = append(groups[index], historyFragment(sample, segmentEnd, fraction))
			cursor = segmentEnd
		}
	}
	indices := make([]int64, 0, len(groups))
	for index := range groups {
		indices = append(indices, index)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
	for _, index := range indices {
		flush(index, groups[index])
	}

	return downsampled
}
