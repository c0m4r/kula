package storage

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// This record-level extension follows ALL Data/Min/Max blocks, including their
// existing variable sections. No metric offsets or old block sizes change.
// Layout: version u8, complete u8, count u32; sorted entries are a u16-length
// path followed by sum f64 and contributing seconds f64. Paths use JSON field
// names and quoted dynamic identities, so reordering members is harmless.
func appendMeanStats(buf []byte, a *AggregatedSample) ([]byte, error) {
	if uint64(len(a.MeanStats)) > math.MaxUint32 {
		return buf, fmt.Errorf("too many mean statistics")
	}
	complete := byte(0)
	if a.MeanWeightsComplete {
		complete = 1
	}
	buf = append(buf, 1, complete)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(a.MeanStats)))
	keys := make([]string, 0, len(a.MeanStats))
	for key := range a.MeanStats {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		stat := a.MeanStats[key]
		if len(key) == 0 || len(key) > math.MaxUint16 || !validMeanStat(stat) {
			return buf, fmt.Errorf("invalid mean statistic for %q", key)
		}
		buf = appendUint16(buf, uint16(len(key)))
		buf = append(buf, key...)
		buf = appendUint64(buf, math.Float64bits(stat.Sum))
		buf = appendUint64(buf, math.Float64bits(stat.Weight))
	}
	return buf, nil
}

func decodeMeanStats(data []byte, a *AggregatedSample) error {
	if len(data) < 6 || data[0] != 1 || data[1] > 1 {
		return fmt.Errorf("invalid mean statistics header")
	}
	count := uint64(binary.LittleEndian.Uint32(data[2:]))
	complete := data[1] == 1
	data = data[6:]
	// Check the count against the payload before allocating from untrusted data.
	if count > uint64(len(data)/19) {
		return fmt.Errorf("truncated mean statistics")
	}
	a.MeanStats = make(map[string]meanAccumulator, int(count))
	a.MeanWeightsComplete = complete
	for range count {
		if len(data) < 2 {
			return fmt.Errorf("truncated mean statistic path")
		}
		n := int(binary.LittleEndian.Uint16(data))
		if n == 0 || n > len(data)-18 {
			return fmt.Errorf("truncated mean statistic")
		}
		key := string(data[2 : 2+n])
		stat := meanAccumulator{
			Sum:    math.Float64frombits(binary.LittleEndian.Uint64(data[2+n:])),
			Weight: math.Float64frombits(binary.LittleEndian.Uint64(data[10+n:])),
		}
		if _, exists := a.MeanStats[key]; exists || !validMeanStat(stat) {
			return fmt.Errorf("invalid mean statistic for %q", key)
		}
		a.MeanStats[key] = stat
		data = data[18+n:]
	}
	if len(data) != 0 {
		return fmt.Errorf("trailing mean statistics bytes")
	}
	return nil
}

func validMeanStat(stat meanAccumulator) bool {
	return !math.IsNaN(stat.Sum) && !math.IsInf(stat.Sum, 0) &&
		!math.IsNaN(stat.Weight) && !math.IsInf(stat.Weight, 0) &&
		stat.Weight >= 0 && (stat.Weight > 0 || stat.Sum == 0)
}
