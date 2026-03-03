package storage

import (
	"fmt"
	"kula-szpiegula/internal/collector"
	"kula-szpiegula/internal/config"
	"os"
	"path/filepath"
	"sync"
	"time"
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
}

func NewStore(cfg config.StorageConfig) (*Store, error) {
	absDir, err := filepath.Abs(cfg.Directory)
	if err != nil {
		return nil, fmt.Errorf("resolving storage directory: %w", err)
	}

	if err := os.MkdirAll(absDir, 0755); err != nil {
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

	return s, nil
}

// WriteSample writes a raw sample to tier 1 and triggers aggregation.
func (s *Store) WriteSample(sample *collector.Sample) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Write to tier 1 (1-second)
	as := &AggregatedSample{
		Timestamp: sample.Timestamp,
		Duration:  time.Second,
		Data:      sample,
	}

	if len(s.tiers) > 0 {
		if err := s.tiers[0].Write(as); err != nil {
			return fmt.Errorf("writing tier 0: %w", err)
		}
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
	result, err := s.QueryRangeWithMeta(from, to)
	if err != nil {
		return nil, err
	}
	return result.Samples, nil
}

// QueryRangeWithMeta returns samples with tier metadata.
// It tries the highest-resolution tier first and falls back to lower tiers
// when the estimated sample count would exceed maxSamples, or when the tier
// doesn't have data covering the requested range.
func (s *Store) QueryRangeWithMeta(from, to time.Time) (*HistoryResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.tiers) == 0 {
		return &HistoryResult{}, nil
	}

	const maxSamples = 3600

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
		if estimatedSamples > maxSamples && tierIdx < len(s.tiers)-1 {
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

				if len(samples) > 800 {
					targetSamples := 450
					groupSize := len(samples) / targetSamples
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
func (s *Store) QueryLatest() (*AggregatedSample, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.tiers) == 0 {
		return nil, fmt.Errorf("no tiers configured")
	}

	samples, err := s.tiers[0].ReadLatest(1)
	if err != nil {
		return nil, err
	}
	if len(samples) == 0 {
		return nil, nil
	}
	return samples[0], nil
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
func (s *Store) aggregateSamples(samples []*collector.Sample, dur time.Duration) *AggregatedSample {
	if len(samples) == 0 {
		return nil
	}

	// Use the last sample as the base (most gauges are "current value")
	last := samples[len(samples)-1]

	avg := *last

	var peakCPU, peakDiskUtil, peakRx, peakTx float64

	if len(samples) > 1 {
		var totalCPU float64
		for _, s := range samples {
			totalCPU += s.CPU.Total.Usage
			if s.CPU.Total.Usage > peakCPU {
				peakCPU = s.CPU.Total.Usage
			}

			// Peak disk utilisation across all devices in this sample
			for _, dev := range s.Disks.Devices {
				if dev.Utilization > peakDiskUtil {
					peakDiskUtil = dev.Utilization
				}
			}

			// Peak network throughput (summed across non-loopback interfaces)
			var rx, tx float64
			for _, iface := range s.Network.Interfaces {
				if iface.Name != "lo" {
					rx += iface.RxMbps
					tx += iface.TxMbps
				}
			}
			if rx > peakRx {
				peakRx = rx
			}
			if tx > peakTx {
				peakTx = tx
			}
		}
		avg.CPU.Total.Usage = totalCPU / float64(len(samples))

		// Average network rates per interface
		for i := range avg.Network.Interfaces {
			var rxSum, txSum float64
			count := 0
			for _, s := range samples {
				for _, iface := range s.Network.Interfaces {
					if iface.Name == avg.Network.Interfaces[i].Name {
						rxSum += iface.RxMbps
						txSum += iface.TxMbps
						count++
					}
				}
			}
			if count > 0 {
				avg.Network.Interfaces[i].RxMbps = rxSum / float64(count)
				avg.Network.Interfaces[i].TxMbps = txSum / float64(count)
			}
		}
	} else {
		// Single sample — peaks equal the observed values
		peakCPU = last.CPU.Total.Usage
		for _, dev := range last.Disks.Devices {
			if dev.Utilization > peakDiskUtil {
				peakDiskUtil = dev.Utilization
			}
		}
		for _, iface := range last.Network.Interfaces {
			if iface.Name != "lo" {
				peakRx += iface.RxMbps
				peakTx += iface.TxMbps
			}
		}
	}

	return &AggregatedSample{
		Timestamp:    last.Timestamp,
		Duration:     dur,
		Data:         &avg,
		PeakCPU:      &peakCPU,
		PeakDiskUtil: &peakDiskUtil,
		PeakRxMbps:   &peakRx,
		PeakTxMbps:   &peakTx,
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

	// Peaks over sub-aggregated samples are the max of their own peak fields,
	// which already captured the true maxima of their respective windows.
	// We only recompute peaks if the incoming samples actually have peak data.
	hasAggregatedPeaks := false
	for _, s := range samples {
		if s.PeakCPU != nil {
			hasAggregatedPeaks = true
			break
		}
	}

	if !hasAggregatedPeaks {
		// These are raw tier-0 samples, aggregateSamples already computed
		// the true peaks accurately. Return it as is.
		return result
	}

	var peakCPU, peakDiskUtil, peakRx, peakTx float64
	for _, s := range samples {
		if s.PeakCPU != nil && *s.PeakCPU > peakCPU {
			peakCPU = *s.PeakCPU
		}
		if s.PeakDiskUtil != nil && *s.PeakDiskUtil > peakDiskUtil {
			peakDiskUtil = *s.PeakDiskUtil
		}
		if s.PeakRxMbps != nil && *s.PeakRxMbps > peakRx {
			peakRx = *s.PeakRxMbps
		}
		if s.PeakTxMbps != nil && *s.PeakTxMbps > peakTx {
			peakTx = *s.PeakTxMbps
		}
	}
	result.PeakCPU = &peakCPU
	result.PeakDiskUtil = &peakDiskUtil
	result.PeakRxMbps = &peakRx
	result.PeakTxMbps = &peakTx
	return result
}
