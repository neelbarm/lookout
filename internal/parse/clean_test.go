package parse

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A log line can carry escape sequences. The renderer writes message text
// straight into the frame, so they have to be gone by the time parsing is done
// or whatever writes the log controls the operator's terminal.
func TestControlSequencesAreStripped(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string // expected Message
	}{
		{"erase display", "2026-09-16T10:00:00Z INFO \x1b[2Jwiped", "wiped"},
		{"cursor jump", "2026-09-16T10:00:00Z INFO \x1b[5;30Hmoved", "moved"},
		{"sgr colour", "2026-09-16T10:00:00Z INFO \x1b[41;97mred\x1b[0m ok", "red ok"},
		{"osc title", "2026-09-16T10:00:00Z INFO \x1b]0;pwned\x07title", "title"},
		{"bel and backspace", "2026-09-16T10:00:00Z INFO a\x07b\x08c", "abc"},
		{"delete", "2026-09-16T10:00:00Z INFO a\x7fb", "ab"},
		{"c1 csi", "2026-09-16T10:00:00Z INFO amb", "amb"},
		{"tab becomes a space", "2026-09-16T10:00:00Z INFO a\tb", "a b"},
		// A JSON line can smuggle an escape past the raw scan as .
		{"json escape", `{"ts":"2026-09-16T10:00:00Z","level":"info","msg":"x[2Jy"}`, "xy"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Parse(c.line, ref)
			if r.Message != c.want {
				t.Errorf("Message = %q, want %q", r.Message, c.want)
			}
			for _, s := range []string{r.Raw, r.Message} {
				for i := 0; i < len(s); i++ {
					if b := s[i]; b < 0x20 || b == 0x7f {
						t.Errorf("control byte %#x survived in %q", b, s)
					}
				}
			}
		})
	}
}

func TestInvalidUTF8IsReplaced(t *testing.T) {
	r := Parse("2026-09-16T10:00:00Z INFO bad \xff\xfe bytes", ref)
	if !utf8.ValidString(r.Raw) || !utf8.ValidString(r.Message) {
		t.Fatalf("invalid UTF-8 survived: raw=%q msg=%q", r.Raw, r.Message)
	}
	if !strings.Contains(r.Message, "bad") || !strings.Contains(r.Message, "bytes") {
		t.Errorf("Message lost its text: %q", r.Message)
	}
}

// The fast path must not disturb ordinary lines, including non-ASCII ones.
func TestCleanLeavesOrdinaryTextAlone(t *testing.T) {
	for _, s := range []string{
		"",
		"plain",
		"request method=GET path=/api/orders status=200 latency=40ms",
		"café © 2026",
		"héllo wörld — ✓",
	} {
		if got := Clean(s); got != s {
			t.Errorf("Clean(%q) = %q, want it unchanged", s, got)
		}
	}
}
