package tui

import (
	"errors"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestCleanTextReplacesTerminalControls(t *testing.T) {
	cases := map[string]string{
		"osc52":       "a\x1b]52;c;cm0gLXJmIH4K\x07",
		"title":       "a\x1b]0;PWNED\x1b\\",
		"csi":         "a\x1b[2J\x1b[H",
		"c1 csi":      "a\u009b31m",
		"c1 osc":      "a\u009d0;x\u009c",
		"del":         "a\x7f",
		"newline":     "a\nb\r",
		"tab":         "a\tb",
		"nul":         "a\x00b",
		"rlo":         "a‮b",
		"lri":         "a⁦b⁩",
		"rlm":         "a‏b",
		"invalid utf": "a\xffb\xc3",
	}
	for name, input := range cases {
		got := cleanText(input)
		if !strings.HasPrefix(got, "a") || !strings.ContainsRune(got, utf8.RuneError) {
			t.Errorf("%s: cleanText(%q) = %q, want the control replaced by U+FFFD", name, input, got)
		}
		if bad, found := terminalControl(got); found {
			t.Errorf("%s: cleanText(%q) = %q still carries %q", name, input, got, bad)
		}
	}

	for _, plain := range []string{"", "eth0", "/media/user/My Disk", "café", "Ünïcødé 日本語", "core ✓", "�"} {
		if got := cleanText(plain); got != plain {
			t.Errorf("cleanText(%q) = %q, want it unchanged", plain, got)
		}
	}
}

func TestTruncatePlainCannotCutInsideAnEscape(t *testing.T) {
	for _, input := range []string{
		"abcdefgh\x1b]0;TITLE\x07ijklmnopqrstuvwxyz",
		"abcdefghij\x1b]52;c;QUJD\x07",
		"abc\x1b]0;unterminated",
		"ab\u009b2J",
	} {
		got := truncatePlain(input, 10)
		if bad, found := terminalControl(got); found {
			t.Errorf("truncatePlain(%q, 10) = %q carries %q", input, got, bad)
		}
	}
}

// hostile packs the sequences a crafted name could carry: an OSC 52
// clipboard write, a window title, a screen clear, a C1 CSI and a
// right-to-left override.
const hostile = "\x1b]52;c;cm0gLXJmIH4K\x07\x1b]0;PWNED\x07\x1b[2J\u009b31m‮"

// shown is how a name followed by hostile starts on screen once cleaned.
func shown(name string) string { return name + "�]52" }

func TestHostStringsCannotWriteToTheTerminal(t *testing.T) {
	cases := []struct {
		name   string
		tab    tabID
		width  int
		mutate func(m *model)
		want   []string
	}{
		{"hostname in the header", tabOverview, 120, func(m *model) {
			m.sample.System.Hostname = "h" + hostile
		}, []string{shown("h")}},
		{"OS, kernel and architecture line", tabOverview, 120, func(m *model) {
			m.osName = "Linux" + hostile
			m.kernelVersion = "6.1" + hostile
			m.cpuArch = "x" + hostile
		}, []string{shown("Linux")}},
		{"GPU on the overview", tabOverview, 120, func(m *model) {
			m.sample.GPU[0].Name = "G" + hostile
		}, []string{shown("G")}},
		{"clock source", tabOverview, 120, func(m *model) {
			m.sample.System.ClockSource = "tsc" + hostile
		}, []string{shown("tsc")}},
		{"busy device in the health line", tabOverview, 120, func(m *model) {
			m.sample.Disks.Devices[0].Name = "sd" + hostile
			m.sample.Disks.Devices[0].Utilization = 97
		}, []string{shown("sd")}},
		{"full filesystem in the health line", tabOverview, 120, func(m *model) {
			m.sample.Disks.FileSystems[1].MountPoint = "/media/u/" + hostile
			m.sample.Disks.FileSystems[1].UsedPct = 97
		}, []string{shown("disk /media/u/")}},
		{"CPU sensor", tabCPU, 120, func(m *model) {
			m.sample.CPU.Sensors[0].Name = "core" + hostile
		}, []string{shown("core")}},
		{"interface table", tabNetwork, 120, func(m *model) {
			m.sample.Network.Interfaces[0].Name = "e" + hostile
		}, []string{shown("e")}},
		{"interface list", tabNetwork, 60, func(m *model) {
			m.sample.Network.Interfaces[0].Name = "e" + hostile
		}, []string{shown("e")}},
		{"device table", tabDisk, 120, func(m *model) {
			m.sample.Disks.Devices[0].Name = "sd" + hostile
		}, []string{shown("sd")}},
		{"device list", tabDisk, 60, func(m *model) {
			m.sample.Disks.Devices[0].Name = "sd" + hostile
		}, []string{shown("sd")}},
		{"long mount point on its own line", tabDisk, 120, func(m *model) {
			m.sample.Disks.FileSystems[1].MountPoint = "/media/u/" + hostile
		}, []string{shown("/media/u/")}},
		{"short mount point as a gauge label", tabDisk, 120, func(m *model) {
			m.sample.Disks.FileSystems[1].MountPoint = "/m\x1b]0;T\x07"
		}, []string{"/m�]0;T�"}},
		{"filesystem device and type", tabDisk, 120, func(m *model) {
			m.sample.Disks.FileSystems[1].Device = "/dev/" + hostile
			m.sample.Disks.FileSystems[1].FSType = "ext4" + hostile
		}, []string{shown("/dev/"), shown("ext4")}},
		{"GPU name and driver", tabGPU, 120, func(m *model) {
			m.sample.GPU[0].Name = "G" + hostile
			m.sample.GPU[0].Driver = "drv" + hostile
		}, []string{shown("G"), shown("drv")}},
		{"history storage error and directory", tabHistory, 120, func(m *model) {
			m.history.openErr = errors.New("open /x" + hostile)
			m.history.dir = "/d" + hostile
		}, []string{shown("/x"), shown("/d")}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(tc.width, 60)
			tc.mutate(&m)
			m.activeTab = tc.tab
			// Every line of the view, including any below the viewport.
			output := m.View() + "\n" + strings.Join(m.contentLines(m.contentWidth()), "\n")
			if bad, found := terminalControl(output); found {
				t.Fatalf("the view writes %q to the terminal", bad)
			}
			for _, want := range tc.want {
				if !strings.Contains(stripped(output), want) {
					t.Errorf("the view does not show the cleaned name %q", want)
				}
			}
			assertFrameFits(t, m)
		})
	}
}

// terminalControl returns the first thing in output, other than a newline or
// an SGR colour sequence, that a terminal would act on instead of drawing.
func terminalControl(output string) (string, bool) {
	for index := 0; index < len(output); {
		r, size := utf8.DecodeRuneInString(output[index:])
		switch {
		case r == '\x1b':
			end := index + 1
			if end < len(output) && output[end] == '[' {
				end++
				for end < len(output) && (output[end] >= '0' && output[end] <= '9' || output[end] == ';') {
					end++
				}
				if end < len(output) && output[end] == 'm' {
					index = end + 1
					continue
				}
			}
			return output[index:min(len(output), index+16)], true
		case r == '\n':
		case r == utf8.RuneError && size == 1, unicode.IsControl(r), unicode.Is(unicode.Bidi_Control, r):
			return output[index : index+size], true
		}
		index += size
	}
	return "", false
}
