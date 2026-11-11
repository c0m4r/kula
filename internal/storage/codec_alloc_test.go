package storage

import (
	"runtime"
	"testing"
)

// TestDecoderAllocatesOnlyWhatThePayloadHolds feeds the decoder records that
// claim 65,535 elements in one dynamic section and end right after the count.
// It pre-allocated every claimed element first, megabytes for a record of a
// few hundred bytes, before it found them missing; the capacity must stay
// within what the rest of the payload can hold.
func TestDecoderAllocatesOnlyWhatThePayloadHolds(t *testing.T) {
	huge := []byte{0xFF, 0xFF}
	none := []byte{0, 0}
	// The sections before each one, all empty: a count, or the hostname and
	// clock source strings, or a presence byte.
	ifaces := none
	sensors := append(append([]byte{}, ifaces...), none...)
	disks := append(append([]byte{}, sensors...), none...)
	fileSystems := append(append([]byte{}, disks...), none...)
	gpus := append(append([]byte{}, fileSystems...), 0, 0) // hostname, clock source
	containers := append(append([]byte{}, gpus...), none...)
	containers = append(containers, 0) // nginx absent
	groups := append(append([]byte{}, containers...), none...)
	groups = append(groups, 0) // postgres absent

	for name, variable := range map[string][]byte{
		"interfaces":  huge,
		"cpu sensors": append(append([]byte{}, ifaces...), huge...),
		"disks":       append(append([]byte{}, sensors...), huge...),
		// One disk with an empty name and zero rates.
		"disk sensors":   append(append(append(append([]byte{}, sensors...), 1, 0, 0), make([]byte, 6*4)...), huge...),
		"filesystems":    append(append([]byte{}, disks...), huge...),
		"gpus":           append(append([]byte{}, gpus...), huge...),
		"containers":     append(append([]byte{}, containers...), huge...),
		"custom groups":  append(append([]byte{}, groups...), huge...),
		"custom metrics": append(append([]byte{}, groups...), 1, 0, 0, 0xFF, 0xFF), // one group, empty name
	} {
		t.Run(name, func(t *testing.T) {
			record := craftHeader(flagHasData | flagHasApps)
			record = append(record, make([]byte, fixedBlockSize)...)
			record = append(record, variable...)
			if _, err := decodeSample(record); err == nil {
				t.Fatal("decoded a record whose elements are missing")
			}
			const runs = 20
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for range runs {
				_, _ = decodeSample(record)
			}
			runtime.ReadMemStats(&after)
			if per := (after.TotalAlloc - before.TotalAlloc) / runs; per > 64<<10 {
				t.Fatalf("decoding a %d-byte record allocated %d KiB", len(record), per>>10)
			}
		})
	}
}
