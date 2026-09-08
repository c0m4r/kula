package web

import (
	"fmt"
	"strings"
	"time"

	"kula/internal/collector"
	"kula/internal/storage"
)

var historySectionOrder = []string{
	"cpu", "lavg", "mem", "swap", "net", "disk", "sys", "proc", "self", "gpu", "psu", "apps",
}

type historySectionSet map[string]struct{}

func parseHistorySections(value string) (historySectionSet, []string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil, nil
	}

	requested := make(historySectionSet)
	for _, raw := range strings.Split(value, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		requested[name] = struct{}{}
	}
	if len(requested) == 0 {
		return nil, nil, fmt.Errorf("no history sections selected")
	}

	ordered := make([]string, 0, len(requested))
	for _, name := range historySectionOrder {
		if _, ok := requested[name]; ok {
			ordered = append(ordered, name)
			delete(requested, name)
		}
	}
	if len(requested) > 0 {
		for name := range requested {
			return nil, nil, fmt.Errorf("unknown history section %q", name)
		}
	}

	set := make(historySectionSet, len(ordered))
	for _, name := range ordered {
		set[name] = struct{}{}
	}
	return set, ordered, nil
}

func (sections historySectionSet) has(name string) bool {
	_, ok := sections[name]
	return ok
}

func selectSampleSections(sample *collector.Sample, sections historySectionSet) map[string]any {
	if sample == nil {
		return nil
	}
	out := map[string]any{"ts": sample.Timestamp}
	if sections.has("cpu") {
		out["cpu"] = sample.CPU
	}
	if sections.has("lavg") {
		out["lavg"] = sample.LoadAvg
	}
	if sections.has("mem") {
		out["mem"] = sample.Memory
	}
	if sections.has("swap") {
		out["swap"] = sample.Swap
	}
	if sections.has("net") {
		out["net"] = sample.Network
	}
	if sections.has("disk") {
		out["disk"] = sample.Disks
	}
	if sections.has("sys") {
		out["sys"] = sample.System
	}
	if sections.has("proc") {
		out["proc"] = sample.Process
	}
	if sections.has("self") {
		out["self"] = sample.Self
	}
	if sections.has("gpu") {
		out["gpu"] = sample.GPU
	}
	if sections.has("psu") {
		out["psu"] = sample.PSU
	}
	if sections.has("apps") {
		out["apps"] = sample.Apps
	}
	return out
}

type sectionedHistorySample struct {
	Timestamp   time.Time      `json:"ts"`
	Duration    time.Duration  `json:"dur"`
	Data        map[string]any `json:"data"`
	Min         map[string]any `json:"min,omitempty"`
	Max         map[string]any `json:"max,omitempty"`
	BucketStart time.Time      `json:"bucket_start"`
	BucketEnd   time.Time      `json:"bucket_end"`
	SampleCount int            `json:"sample_count"`
	Coverage    float64        `json:"coverage"`
}

type sectionedHistoryResult struct {
	Samples           []sectionedHistorySample `json:"samples"`
	Tier              int                      `json:"tier"`
	Resolution        string                   `json:"resolution"`
	SourceResolution  string                   `json:"source_resolution"`
	Downsampled       bool                     `json:"downsampled"`
	RequestedFrom     time.Time                `json:"requested_from"`
	RequestedTo       time.Time                `json:"requested_to"`
	ActualFrom        *time.Time               `json:"actual_from,omitempty"`
	ActualTo          *time.Time               `json:"actual_to,omitempty"`
	Complete          bool                     `json:"complete"`
	ExactComplete     bool                     `json:"exact_complete"`
	ValidAggregations []string                 `json:"valid_aggregations"`
	Sections          []string                 `json:"sections"`
}

func selectHistorySections(result *storage.HistoryResult, sections historySectionSet, ordered []string) *sectionedHistoryResult {
	samples := make([]sectionedHistorySample, 0, len(result.Samples))
	for _, sample := range result.Samples {
		if sample == nil {
			continue
		}
		samples = append(samples, sectionedHistorySample{
			Timestamp:   sample.Timestamp,
			Duration:    sample.Duration,
			Data:        selectSampleSections(sample.Data, sections),
			Min:         selectSampleSections(sample.Min, sections),
			Max:         selectSampleSections(sample.Max, sections),
			BucketStart: sample.BucketStart,
			BucketEnd:   sample.BucketEnd,
			SampleCount: sample.SampleCount,
			Coverage:    sample.Coverage,
		})
	}
	return &sectionedHistoryResult{
		Samples:           samples,
		Tier:              result.Tier,
		Resolution:        result.Resolution,
		SourceResolution:  result.SourceResolution,
		Downsampled:       result.Downsampled,
		RequestedFrom:     result.RequestedFrom,
		RequestedTo:       result.RequestedTo,
		ActualFrom:        result.ActualFrom,
		ActualTo:          result.ActualTo,
		Complete:          result.Complete,
		ExactComplete:     result.ExactComplete,
		ValidAggregations: result.ValidAggregations,
		Sections:          ordered,
	}
}
