package storage

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestInspectorWalksTheRingLikeTheReader keeps addons/inspect_tier.py's ring
// walk in step with the Go reader on the files it is used to diagnose: holes
// an unclean shutdown leaves in the active pass (with a stale record header
// from an older pass inside one), a header claiming more than the file holds,
// and wrapped rings whose tail an older build did not record or recorded
// wrong. Both must visit the same records, or the inspector reports a
// different "latest" record than `kula inspect`.
func TestInspectorWalksTheRingLikeTheReader(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	base := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	// write fills a ring of size bytes with n records (rollups of nIfaces
	// interfaces) and returns each record's offset plus the end offset.
	write := func(t *testing.T, size int64, n, nIfaces int) (string, []int64) {
		t.Helper()
		path := t.TempDir() + "/tier.dat"
		tier, err := OpenTier(path, size)
		if err != nil {
			t.Fatal(err)
		}
		var offsets []int64
		for i := 0; i < n; i++ {
			offsets = append(offsets, tier.writeOff)
			if err := tier.Write(varSample(base.Add(time.Duration(i)*time.Minute), nIfaces)); err != nil {
				t.Fatal(err)
			}
		}
		offsets = append(offsets, tier.writeOff)
		if err := tier.Close(); err != nil {
			t.Fatal(err)
		}
		return path, offsets
	}
	patch := func(t *testing.T, path string, at int64, data []byte) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		if _, err := f.WriteAt(data, at); err != nil {
			t.Fatal(err)
		}
	}
	u64 := func(v uint64) []byte { return binary.LittleEndian.AppendUint64(nil, v) }
	headerFlags := func(t *testing.T, path string) uint32 {
		t.Helper()
		buf := make([]byte, 8)
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		if _, err := f.ReadAt(buf, 0); err != nil {
			t.Fatal(err)
		}
		return binary.LittleEndian.Uint32(buf[4:8])
	}

	// wrapped writes a ring that has wrapped once and is part way through
	// its second pass.
	const wrapRecords = 120
	wrapped := func(t *testing.T) (string, []int64) {
		t.Helper()
		path, offsets := write(t, 32<<10, wrapRecords, 2)
		if headerFlags(t, path)&flagHeaderWrapped == 0 || offsets[wrapRecords] > offsets[wrapRecords/2] {
			t.Fatalf("ring of %d-byte records did not wrap once", offsets[1]-offsets[0])
		}
		return path, offsets
	}

	for name, build := range map[string]func(t *testing.T) (path string, mustVisit int64){
		"clean": func(t *testing.T) (string, int64) {
			path, offsets := write(t, 1<<20, 8, 2)
			return path, offsets[7]
		},
		"hole": func(t *testing.T) (string, int64) {
			path, offsets := write(t, 1<<20, 8, 2)
			patch(t, path, headerSize+offsets[3], make([]byte, offsets[5]-offsets[3]))
			return path, offsets[7]
		},
		"stale header in a hole": func(t *testing.T) (string, int64) {
			// As TestResyncVerifiesChecksummedRecords: records outgrow the
			// resync window, so only the checksum rejects the stale header.
			path, offsets := write(t, 1<<20, 6, 120)
			patch(t, path, headerSize+offsets[2], make([]byte, offsets[3]-offsets[2]))
			stale := offsets[2] + 64
			header := binary.LittleEndian.AppendUint32(nil, uint32((offsets[4]+offsets[5])/2-stale-4))
			header = append(header, recordKindChecksummed)
			header = binary.LittleEndian.AppendUint64(header, uint64(base.Add(-time.Hour).UnixNano()))
			patch(t, path, headerSize+stale, header)
			return path, offsets[5]
		},
		"header past the file end": func(t *testing.T) (string, int64) {
			path, offsets := write(t, 1<<20, 8, 2)
			if err := os.Truncate(path, headerSize+(offsets[6]+offsets[7])/2); err != nil {
				t.Fatal(err)
			}
			return path, offsets[5]
		},
		"wrapped": func(t *testing.T) (string, int64) {
			path, offsets := wrapped(t)
			return path, offsets[wrapRecords-1]
		},
		"wrapped, bad persisted tail": func(t *testing.T) (string, int64) {
			path, offsets := wrapped(t)
			patch(t, path, 56, u64(1)) // before writeOff: readHeader distrusts it
			return path, offsets[wrapRecords-1]
		},
		"wrapped, clock stepped back past the ring": func(t *testing.T) (string, int64) {
			// The newest sample predates the whole previous pass.
			path, offsets := wrapped(t)
			patch(t, path, 48, u64(uint64(base.Add(-24*time.Hour).UnixNano())))
			return path, offsets[wrapRecords-1]
		},
		"wrapped, persisted tail inside a record": func(t *testing.T) (string, int64) {
			path, offsets := wrapped(t)
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 8)
			_, err = f.ReadAt(buf, 56)
			_ = f.Close()
			if err != nil {
				t.Fatal(err)
			}
			patch(t, path, 56, u64(binary.LittleEndian.Uint64(buf)+5))
			return path, offsets[wrapRecords-1]
		},
		"wrapped, no tail tracking": func(t *testing.T) (string, int64) {
			path, offsets := wrapped(t)
			flags := binary.LittleEndian.AppendUint32(nil, headerFlags(t, path)&^(flagHeaderHasTail|flagHeaderWrapped))
			patch(t, path, 4, flags)
			patch(t, path, 56, u64(0))
			return path, offsets[wrapRecords-1]
		},
	} {
		t.Run(name, func(t *testing.T) {
			path, mustVisit := build(t)

			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			tier := &Tier{file: f, path: path}
			if err := tier.readHeader(); err != nil {
				t.Fatal(err)
			}
			want := [][2]int64{}
			for _, seg := range tier.ringSegments() {
				tier.walkSegment(seg, seg.start, func(offset int64, dataLen uint32, _ int64, _ bool) bool {
					want = append(want, [2]int64{offset, int64(dataLen)})
					return true
				})
			}
			if len(want) == 0 || want[len(want)-1][0] != mustVisit {
				t.Fatalf("the Go walk ends at %v, want the record at %d; the scenario is broken", want[len(want)-1:], mustVisit)
			}

			cmd := exec.Command(python, "-c", `import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("inspect_tier", "../../addons/inspect_tier.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
print(json.dumps(module.tier_records(sys.argv[1])))`, path)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("Python inspector: %v: %s", err, out)
			}
			got := [][2]int64{}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("Python output %q: %v", out, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Python visits %d records, Go %d\nPython: %v\nGo:     %v", len(got), len(want), got, want)
			}
		})
	}
}

// TestInspectorCommandLine: inspect_tier.py answers -h/--help with its usage
// instead of trying to open a file of that name, and reads a tier file.
func TestInspectorCommandLine(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	run := func(args ...string) (string, error) {
		out, err := exec.Command(python, append([]string{"-I", "../../addons/inspect_tier.py"}, args...)...).CombinedOutput()
		return string(out), err
	}
	for _, help := range []string{"-h", "--help"} {
		if out, err := run(help); err != nil || !strings.Contains(out, "usage:") || strings.Contains(out, "Traceback") {
			t.Errorf("%s: %v\n%s", help, err, out)
		}
	}
	if out, err := run(); err == nil || !strings.Contains(out, "usage:") {
		t.Errorf("no path: %v, want a usage error\n%s", err, out)
	}

	path := t.TempDir() + "/tier_0.dat"
	tier, err := OpenTier(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := tier.Write(varSample(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC), 1)); err != nil {
		t.Fatal(err)
	}
	if err := tier.Close(); err != nil {
		t.Fatal(err)
	}
	if out, err := run(path); err != nil || !strings.Contains(out, "Latest Record") {
		t.Errorf("reading a tier file: %v\n%s", err, out)
	}
}
