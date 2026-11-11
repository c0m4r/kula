package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// cleanText makes a host-provided string safe to write to the terminal. It
// replaces every character a terminal acts on instead of drawing with U+FFFD:
// C0 controls (ESC, BEL, CR, LF, TAB…), DEL, C1 controls (including the
// single-byte CSI U+009B), bidirectional overrides, isolates and marks, and
// bytes that are not valid UTF-8.
//
// Hostnames, mount points (which carry volume labels), interface, device,
// sensor and GPU names come from the host and reach the terminal of whoever
// runs the TUI, often root. Left raw, an escape sequence in one could write
// the clipboard (OSC 52), retitle the window or repaint the screen to hide a
// full disk. Each such string passes through here before it is styled or its
// width measured; a string that needs no change is returned as is.
func cleanText(value string) string {
	return strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return utf8.RuneError
		}
		return r
	}, value)
}
