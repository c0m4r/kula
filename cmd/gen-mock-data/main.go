package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"kula/internal/config"
	"kula/internal/storage"
)

const defaultSeed int64 = 0x4b554c41 // "KULA"

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		log.Fatalf("gen-mock-data: %v", err)
	}
}

func run(input io.Reader, output io.Writer) error {
	days := flag.Int("days", 7, "number of days to generate")
	duration := flag.Duration("duration", 0, "exact duration to generate (overrides -days, for example 6h or 30m)")
	cfgPath := flag.String("config", "config.yaml", "path to configuration file")
	seed := flag.Int64("seed", defaultSeed, "deterministic pseudo-random seed")
	profileName := flag.String("profile", string(profileRealistic), "workload profile: realistic or steady")
	startText := flag.String("start", "", "timestamp of the first sample in RFC3339 format (default: end near now)")
	yes := flag.Bool("yes", false, "skip the interactive confirmation")
	flag.Parse()

	profile, err := parseProfile(*profileName)
	if err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	interval := cfg.Collection.Interval
	if interval <= 0 {
		return errors.New("collection interval must be positive")
	}

	span, err := generationDuration(*days, *duration)
	if err != nil {
		return err
	}
	totalSamples, err := sampleCount(span, interval)
	if err != nil {
		return err
	}
	start, end, err := generationRange(*startText, totalSamples, interval)
	if err != nil {
		return err
	}

	fmt.Fprintf(output, "WARNING: This will generate %d mock samples into %q.\n", totalSamples, cfg.Storage.Directory)
	fmt.Fprintln(output, "The tier ring buffers may replace retained data in that directory.")
	if !*yes {
		proceed, err := confirm(input, output)
		if err != nil {
			return err
		}
		if !proceed {
			fmt.Fprintln(output, "Aborted by user.")
			return nil
		}
	}

	gen, err := newGenerator(generatorOptions{
		Seed:          *seed,
		Interval:      interval,
		TotalSamples:  totalSamples,
		Profile:       profile,
		CustomMetrics: cfg.Applications.Custom,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(output, "Initializing storage at %s\n", cfg.Storage.Directory)
	store, err := storage.NewStore(cfg.Storage)
	if err != nil {
		return fmt.Errorf("initialize storage: %w", err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			log.Printf("Error closing storage: %v", closeErr)
		}
	}()

	fmt.Fprintf(output, "Generating %s samples at %s resolution (%s profile, seed %d).\n",
		formatCount(totalSamples), interval, profile, *seed)
	fmt.Fprintf(output, "Time range: %s to %s\n", start.Format(time.RFC3339), end.Format(time.RFC3339))
	for _, event := range gen.timeline(start) {
		fmt.Fprintf(output, "  %-19s %s\n", event.Name+":", event.Range)
	}

	started := time.Now()
	progressEvery := progressInterval(totalSamples)
	for i := 0; i < totalSamples; i++ {
		ts := start.Add(time.Duration(i) * interval)
		if err := store.WriteSample(gen.next(ts, i)); err != nil {
			return fmt.Errorf("write sample %d at %s: %w", i, ts.Format(time.RFC3339), err)
		}
		if i > 0 && i%progressEvery == 0 {
			fmt.Fprintf(output, "Generated %s / %s samples (%.1f%%)...\n",
				formatCount(i), formatCount(totalSamples), float64(i)/float64(totalSamples)*100)
		}
	}

	elapsed := time.Since(started)
	rate := float64(totalSamples) / elapsed.Seconds()
	fmt.Fprintf(output, "Finished generating %s samples in %v (%.0f samples/sec).\n",
		formatCount(totalSamples), elapsed.Round(time.Millisecond), rate)
	fmt.Fprintln(output, "Start Kula and inspect the labeled windows to exercise history and aggregation behavior.")
	return nil
}

func generationDuration(days int, exact time.Duration) (time.Duration, error) {
	if exact < 0 {
		return 0, errors.New("duration must be positive")
	}
	if exact > 0 {
		return exact, nil
	}
	if days <= 0 {
		return 0, errors.New("days must be positive")
	}
	const maxDays int64 = (1<<63 - 1) / int64(24*time.Hour)
	if int64(days) > maxDays {
		return 0, fmt.Errorf("days exceeds the maximum supported value (%d)", maxDays)
	}
	return time.Duration(days) * 24 * time.Hour, nil
}

func sampleCount(span, interval time.Duration) (int, error) {
	if span <= 0 {
		return 0, errors.New("generation duration must be positive")
	}
	if interval <= 0 {
		return 0, errors.New("collection interval must be positive")
	}
	count := span / interval
	if span%interval != 0 {
		count++
	}
	if count == 0 {
		count = 1
	}
	maxInt := int64(^uint(0) >> 1)
	if int64(count) > maxInt {
		return 0, errors.New("requested dataset has too many samples")
	}
	return int(count), nil
}

func generationRange(startText string, totalSamples int, interval time.Duration) (time.Time, time.Time, error) {
	if totalSamples <= 0 {
		return time.Time{}, time.Time{}, errors.New("sample count must be positive")
	}
	span := time.Duration(totalSamples-1) * interval
	if startText != "" {
		start, err := time.Parse(time.RFC3339, startText)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("parse start time: %w", err)
		}
		return start, start.Add(span), nil
	}
	end := time.Now().Truncate(interval)
	return end.Add(-span), end, nil
}

func confirm(input io.Reader, output io.Writer) (bool, error) {
	fmt.Fprint(output, "Are you sure you want to proceed? (y/N): ")
	response, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	response = strings.ToLower(strings.TrimSpace(response))
	return response == "y" || response == "yes", nil
}

func progressInterval(total int) int {
	if total < 1_000 {
		return total + 1
	}
	interval := total / 10
	if interval > 100_000 {
		return 100_000
	}
	return interval
}

func formatCount(value int) string {
	s := fmt.Sprintf("%d", value)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
