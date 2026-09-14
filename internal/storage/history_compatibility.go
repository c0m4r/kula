package storage

// Query-only profiles: the allowlist is the intersection of scalar extrema
// calculated by pre-policy minSample/maxSample and mergeSample (ea92ea1 and
// 35c50b0). Exclude copied gauges, temperature sentinels and dynamic identities.
var legacyExtremaFields = []string{
	"cpu.total.usage", "cpu.total.user", "cpu.total.system", "cpu.total.iowait", "cpu.total.steal",
	"lavg.load1", "lavg.load5", "lavg.load15",
	"mem.used", "mem.used_pct", "swap.used", "swap.used_pct",
}

func decodedExtremaProfile(sample *AggregatedSample, flags uint16) string {
	if sample.Data == nil || sample.Min == nil || sample.Max == nil {
		return "none"
	}
	if sample.AggregationVersion >= currentAggregationVersion {
		return "current"
	}
	// New writers also persist incomplete rollups. Do not mistake those for
	// old-reducer output and certify invented extrema after a restart.
	if flags&(flagHasMeanStats|flagHasDiskIDs) != 0 {
		return "none"
	}
	return "legacy"
}

func sampleExtremaProfile(sample *AggregatedSample) string {
	if sample == nil || sample.Data == nil {
		return "none"
	}
	if sample.ExtremaProfile != "" {
		return sample.ExtremaProfile
	}
	if sample.Min != nil && sample.Max != nil && sample.AggregationVersion >= currentAggregationVersion {
		return "current"
	}
	return "none"
}

func mergedExtremaProfile(samples []*AggregatedSample) string {
	var legacy, current bool
	for _, sample := range samples {
		if sample == nil || sample.Data == nil {
			continue
		}
		profile := sampleExtremaProfile(sample)
		// Raw reducer callers may use unannotated in-memory wrappers. Queries
		// explicitly annotate lossy coarse Data-only records as none.
		if sample.ExtremaProfile == "" && sample.Min == nil && sample.Max == nil {
			profile = "raw"
		}
		switch profile {
		case "raw", "current":
			current = true
		case "legacy":
			legacy = true
		case "mixed":
			legacy, current = true, true
		default:
			return "none"
		}
	}
	if legacy && current {
		return "mixed"
	}
	if legacy {
		return "legacy"
	}
	if current {
		return "current"
	}
	return "none"
}

func annotateHistoryAggregations(result *HistoryResult) {
	result.ValidAggregations = validHistoryAggregations(result.Samples)
	result.AvailableAggregations = []string{"data"}
	result.ExtremaProfiles = make(map[string][]string)
	for _, sample := range result.Samples {
		if sample == nil {
			continue
		}
		profile := sampleExtremaProfile(sample)
		if sample.Min == nil || sample.Max == nil {
			profile = "none"
		}
		sample.ExtremaProfile = profile
		if _, exists := result.ExtremaProfiles[profile]; exists {
			continue
		}
		switch profile {
		case "current":
			result.ExtremaProfiles[profile] = []string{"*"}
		case "legacy", "mixed":
			result.ExtremaProfiles[profile] = append([]string(nil), legacyExtremaFields...)
		default:
			result.ExtremaProfiles[profile] = []string{}
		}
		if len(result.ExtremaProfiles[profile]) > 0 {
			result.AvailableAggregations = []string{"data", "min", "max"}
		}
	}
}
