package storage

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"kula-szpiegula/internal/collector"
	"kula-szpiegula/internal/config"
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

	// Aggregation state
	tier1Count int
	tier1Buf   []*collector.Sample
	tier2Count int
	tier2Buf   []*AggregatedSample

	// latestCache holds the most recently written sample in memory.
	// This makes QueryLatest O(1) (a guarded pointer read) instead of
	// O(n) (a full disk scan of the tier file).
	latestCache *AggregatedSample
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
		dir:     absDir,
		configs: cfg.Tiers,
	}

	for i, tc := range cfg.Tiers {
		path := filepath.Join(cfg.Directory, fmt.Sprintf("tier_%d.dat", i))
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

	// Configure aggregation ratios based on resolutions
	ratio1 := 60
	if len(s.configs) > 1 && s.configs[0].Resolution > 0 {
		ratio1 = int(s.configs[1].Resolution / s.configs[0].Resolution)
	}
	if ratio1 <= 0 {
		ratio1 = 1
	}

	ratio2 := 5
	if len(s.configs) > 2 && s.configs[1].Resolution > 0 {
		ratio2 = int(s.configs[2].Resolution / s.configs[1].Resolution)
	}
	if ratio2 <= 0 {
		ratio2 = 1
	}

	// Reconstruct Tier 1 state
	t1Newest := s.tiers[1].NewestTimestamp()
	t0Samples, err := s.tiers[0].ReadLatest(ratio1)
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

	// Reconstruct Tier 2 state
	if len(s.tiers) > 2 {
		t2Newest := s.tiers[2].NewestTimestamp()
		t1Samples, err := s.tiers[1].ReadLatest(ratio2)
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

// WriteSample writes a raw sample to tier 1 and triggers aggregation.
func (s *Store) WriteSample(sample *collector.Sample) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Write to tier 1 (1-second)
	dur := time.Second
	if s.latestCache != nil {
		dur = sample.Timestamp.Sub(s.latestCache.Timestamp)
		if dur <= 0 {
			dur = time.Second // Fallback for out-of-order or duplicate samples
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

	// Aggregate for tier 2 (every 60 samples = 1 minute)
	s.tier1Buf = append(s.tier1Buf, sample)
	s.tier1Count++

	// Configure aggregation ratios based on resolutions
	ratio1 := 60
	if len(s.configs) > 1 && s.configs[0].Resolution > 0 {
		ratio1 = int(s.configs[1].Resolution / s.configs[0].Resolution)
	}
	if ratio1 <= 0 {
		ratio1 = 1
	}

	ratio2 := 5
	if len(s.configs) > 2 && s.configs[1].Resolution > 0 {
		ratio2 = int(s.configs[2].Resolution / s.configs[1].Resolution)
	}
	if ratio2 <= 0 {
		ratio2 = 1
	}

	if s.tier1Count >= ratio1 && len(s.tiers) > 1 {
		agg := s.aggregateSamples(s.tier1Buf, s.configs[1].Resolution)
		if err := s.tiers[1].Write(agg); err != nil {
			return fmt.Errorf("writing tier 1: %w", err)
		}
		s.tier2Buf = append(s.tier2Buf, agg)
		s.tier2Count++
		s.tier1Buf = nil
		s.tier1Count = 0

		if s.tier2Count >= ratio2 && len(s.tiers) > 2 {
			agg3 := s.aggregateAggregated(s.tier2Buf, s.configs[2].Resolution)
			if err := s.tiers[2].Write(agg3); err != nil {
				return fmt.Errorf("writing tier 2: %w", err)
			}
			s.tier2Buf = nil
			s.tier2Count = 0
		}
	}

	return nil
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
// It tries the highest-resolution tier first and falls back to lower tiers
// when the estimated sample count would exceed maxSamples, or when the tier
// doesn't have data covering the requested range.
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

	var resolutions []string
	var resDurations []time.Duration
	for _, tc := range s.configs {
		resolutions = append(resolutions, fmtRes(tc.Resolution))
		resDurations = append(resDurations, tc.Resolution)
	}

	duration := to.Sub(from)

	// Try each tier from highest to lowest resolution
	for tierIdx := 0; tierIdx < len(s.tiers); tierIdx++ {
		// Estimate sample count for this tier
		resDur := time.Second
		if tierIdx < len(resDurations) {
			resDur = resDurations[tierIdx]
		}
		estimatedSamples := int(duration / resDur)

		// Skip this tier if it would produce too many samples
		// (unless it's the last tier — always use it as fallback)
		maxAllowed := maxSamples
		if targetPoints > maxAllowed {
			maxAllowed = targetPoints
		}
		if estimatedSamples > maxAllowed && tierIdx < len(s.tiers)-1 {
			continue
		}

		tier := s.tiers[tierIdx]
		oldest := tier.OldestTimestamp()

		// If this tier has data covering (or partially covering) the requested range, use it
		if tier.Count() > 0 && !oldest.After(to) {
			samples, err := tier.ReadRange(from, to)
			if err != nil {
				return nil, fmt.Errorf("reading tier %d: %w", tierIdx, err)
			}
			if len(samples) > 0 {
				res := "1s"
				if tierIdx < len(resolutions) {
					res = resolutions[tierIdx]
				}

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
						resDur := time.Second
						if tierIdx < len(resDurations) {
							resDur = resDurations[tierIdx]
						}
						res = fmtRes(resDur * time.Duration(groupSize))
					}
				}

				return &HistoryResult{
					Samples:    samples,
					Tier:       tierIdx,
					Resolution: res,
				}, nil
			}
		}
	}

	// No data found in any tier
	return &HistoryResult{Tier: 0, Resolution: resolutions[0]}, nil
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
	if s.latestCache != nil {
		return s.latestCache, nil
	}

	// Cold path: only reached on an empty store where no sample has been
	// written yet this process lifetime and warmLatestCache found nothing.
	return nil, nil
}

func (s *Store) Close() error {
	for _, t := range s.tiers {
		if err := t.Flush(); err != nil {
			return err
		}
		if err := t.Close(); err != nil {
			return err
		}
	}
	return nil
}

// aggregateSamples creates an aggregated sample from raw samples.
// Uses the last sample's values (for gauges) and averages for rates.
// Also tracks peak (maximum) values for CPU, disk utilisation, and network throughput.
// minSample returns an element-wise minimum of two samples.
func minSample(a, b *collector.Sample) *collector.Sample {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	res := *a // copy structure and unchanged fields (like timestamps, names)

	res.CPU.Total.Usage = minF(a.CPU.Total.Usage, b.CPU.Total.Usage)
	res.CPU.Total.User = minF(a.CPU.Total.User, b.CPU.Total.User)
	res.CPU.Total.System = minF(a.CPU.Total.System, b.CPU.Total.System)
	res.CPU.Total.IOWait = minF(a.CPU.Total.IOWait, b.CPU.Total.IOWait)
	res.CPU.Total.Steal = minF(a.CPU.Total.Steal, b.CPU.Total.Steal)
	res.CPU.Temperature = minF(a.CPU.Temperature, b.CPU.Temperature)

	res.LoadAvg.Load1 = minF(a.LoadAvg.Load1, b.LoadAvg.Load1)
	res.LoadAvg.Load5 = minF(a.LoadAvg.Load5, b.LoadAvg.Load5)
	res.LoadAvg.Load15 = minF(a.LoadAvg.Load15, b.LoadAvg.Load15)

	res.Memory.Used = minU(a.Memory.Used, b.Memory.Used)
	res.Memory.UsedPercent = minF(a.Memory.UsedPercent, b.Memory.UsedPercent)

	res.Swap.Used = minU(a.Swap.Used, b.Swap.Used)
	res.Swap.UsedPercent = minF(a.Swap.UsedPercent, b.Swap.UsedPercent)

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
			Utilization:  minF(devA.Utilization, devB.Utilization),
			ReadBytesPS:  minF(devA.ReadBytesPS, devB.ReadBytesPS),
			WriteBytesPS: minF(devA.WriteBytesPS, devB.WriteBytesPS),
			ReadsPerSec:  minF(devA.ReadsPerSec, devB.ReadsPerSec),
			WritesPerSec: minF(devA.WritesPerSec, devB.WritesPerSec),
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
			RxMbps: minF(ifA.RxMbps, ifB.RxMbps),
			TxMbps: minF(ifA.TxMbps, ifB.TxMbps),
			RxPPS:  minF(ifA.RxPPS, ifB.RxPPS),
			TxPPS:  minF(ifA.TxPPS, ifB.TxPPS),
		}
	}
	return &res
}

// maxSample returns an element-wise maximum of two samples.
func maxSample(a, b *collector.Sample) *collector.Sample {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	res := *a // copy structure

	res.CPU.Total.Usage = maxF(a.CPU.Total.Usage, b.CPU.Total.Usage)
	res.CPU.Total.User = maxF(a.CPU.Total.User, b.CPU.Total.User)
	res.CPU.Total.System = maxF(a.CPU.Total.System, b.CPU.Total.System)
	res.CPU.Total.IOWait = maxF(a.CPU.Total.IOWait, b.CPU.Total.IOWait)
	res.CPU.Total.Steal = maxF(a.CPU.Total.Steal, b.CPU.Total.Steal)
	res.CPU.Temperature = maxF(a.CPU.Temperature, b.CPU.Temperature)

	res.LoadAvg.Load1 = maxF(a.LoadAvg.Load1, b.LoadAvg.Load1)
	res.LoadAvg.Load5 = maxF(a.LoadAvg.Load5, b.LoadAvg.Load5)
	res.LoadAvg.Load15 = maxF(a.LoadAvg.Load15, b.LoadAvg.Load15)

	res.Memory.Used = maxU(a.Memory.Used, b.Memory.Used)
	res.Memory.UsedPercent = maxF(a.Memory.UsedPercent, b.Memory.UsedPercent)

	res.Swap.Used = maxU(a.Swap.Used, b.Swap.Used)
	res.Swap.UsedPercent = maxF(a.Swap.UsedPercent, b.Swap.UsedPercent)

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
			Utilization:  maxF(devA.Utilization, devB.Utilization),
			ReadBytesPS:  maxF(devA.ReadBytesPS, devB.ReadBytesPS),
			WriteBytesPS: maxF(devA.WriteBytesPS, devB.WriteBytesPS),
			ReadsPerSec:  maxF(devA.ReadsPerSec, devB.ReadsPerSec),
			WritesPerSec: maxF(devA.WritesPerSec, devB.WritesPerSec),
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
			RxMbps: maxF(ifA.RxMbps, ifB.RxMbps),
			TxMbps: maxF(ifA.TxMbps, ifB.TxMbps),
			RxPPS:  maxF(ifA.RxPPS, ifB.RxPPS),
			TxPPS:  maxF(ifA.TxPPS, ifB.TxPPS),
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
