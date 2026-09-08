package i18n

import (
	"testing"
)

func TestDetectLang(t *testing.T) {
	lang := DetectLang()
	if lang == "" {
		t.Errorf("DetectLang() returned empty string")
	}
}

func TestTranslator_FallbackToEnglish(t *testing.T) {
	// English translator basic functionality
	translator := NewTranslator("en")
	if translator.T("cpu") != "CPU" {
		t.Errorf("Expected 'CPU', got %s", translator.T("cpu"))
	}

	// Unknown language should fall back to English
	unknownTranslator := NewTranslator("unknown_lang")
	if unknownTranslator.T("cpu") != "CPU" {
		t.Errorf("Expected 'CPU', got %s", unknownTranslator.T("cpu"))
	}

	// Missing key fallback
	if unknownTranslator.T("missing_key_that_does_not_exist") != "missing_key_that_does_not_exist" {
		t.Errorf("Expected raw key for missing translation")
	}
}

func TestHistoricalNavigationTranslations(t *testing.T) {
	translator := NewTranslator("en")
	for key, want := range map[string]string{
		"historical_navigation":   "Historical navigation",
		"live":                    "Live",
		"history_back":            "Back",
		"history_forward":         "Forward",
		"zoom_out":                "Zoom out",
		"pinned_time":             "Pinned time",
		"pinned_time_cleared":     "Pinned time cleared",
		"tier":                    "Tier",
		"time_zone":               "Display time zone",
		"time_zone_local":         "Local",
		"time_zone_utc":           "UTC",
		"use_local_time":          "Use local time",
		"use_utc_time":            "Use UTC",
		"bucket_start":            "Bucket start",
		"bucket_end":              "Bucket end",
		"source":                  "Source",
		"output_resolution":       "Output resolution",
		"contributors":            "Contributors",
		"coverage":                "Coverage",
		"more_series":             "more series",
		"interactive_time_series": "Interactive time-series chart",
		"no_chart_data":           "No chart data in the selected range",
		"chart_keyboard_help":     "Press Enter to pin the nearest point; while pinned, use Left and Right to move, Home and End to jump, and Escape to clear. When unpinned, arrows pan and plus or minus zoom",
		"history_loading":         "Loading historical data",
		"history_failed":          "Historical data could not be loaded",
		"history_empty":           "No historical data in this range",
		"history_partial":         "Historical data loaded with partial coverage",
		"history_complete":        "Historical data loaded",
		"data_table":              "Data",
		"view_chart_data":         "View chart data table",
		"download_csv":            "Download CSV",
		"download_chart_csv":      "Download complete chart data as CSV",
		"chart_data_table":        "Chart data table",
		"select_series":           "Select displayed series",
		"timestamp":               "Timestamp",
		"showing_latest":          "Showing latest",
		"download_csv_all_data":   "Download CSV includes all timestamps",
		"chart_canvas_fallback":   "This chart has an accessible summary and keyboard controls; enable chart data controls in Customization for a table",
	} {
		if got := translator.T(key); got != want {
			t.Errorf("T(%q) = %q, want %q", key, got, want)
		}
	}
}
