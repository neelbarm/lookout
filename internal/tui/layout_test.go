package tui

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSparkline(t *testing.T) {
	if got := Sparkline([]float64{0, 1, 2, 3, 4, 5, 6, 7}, 8, 7); got != "▁▂▃▄▅▆▇█" {
		t.Errorf("Sparkline ramp = %q", got)
	}
	if got := Sparkline([]float64{5, 5, 5}, 3, 5); got != "███" {
		t.Errorf("Sparkline flat = %q", got)
	}
	if got := Sparkline([]float64{0, 0, 0}, 3, 0); got != "▁▁▁" {
		t.Errorf("Sparkline zeros = %q", got)
	}
	// Right aligned: a short series pads on the left.
	if got := Sparkline([]float64{7}, 4, 7); got != "   █" {
		t.Errorf("Sparkline padding = %q", got)
	}
	// A long series keeps the newest samples.
	if got := Sparkline([]float64{0, 0, 0, 7, 7}, 2, 7); got != "██" {
		t.Errorf("Sparkline truncation = %q", got)
	}
	// Every result is exactly `width` cells wide.
	for _, w := range []int{1, 5, 17, 60} {
		got := Sparkline([]float64{1, 9, 3, 4}, w, 0)
		if n := utf8.RuneCountInString(got); n != w {
			t.Errorf("Sparkline width %d produced %d cells", w, n)
		}
	}
	if Sparkline(nil, 0, 1) != "" {
		t.Error("zero width should render nothing")
	}
	if Sparkline(nil, 3, 1) != "   " {
		t.Error("empty series should render blanks")
	}
	// Auto-scaling uses the series maximum.
	if got := Sparkline([]float64{0, 10}, 2, 0); got != "▁█" {
		t.Errorf("auto-scale = %q", got)
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		w    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 4, "hel…"},
		{"hello", 1, "…"},
		{"hello", 0, ""},
		{"hello", -3, ""},
		{"héllo wörld", 6, "héllo…"},
		{"日本語テキスト", 4, "日本語…"},
	}
	for _, c := range cases {
		got := Truncate(c.in, c.w)
		if got != c.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", c.in, c.w, got, c.want)
		}
		if n := utf8.RuneCountInString(got); c.w > 0 && n > c.w {
			t.Errorf("Truncate(%q, %d) returned %d runes", c.in, c.w, n)
		}
	}
}

func TestVisWidthIgnoresEscapes(t *testing.T) {
	s := Styler{Mode: TrueColor}
	styled := s.B(ColSev3, "boom") + s.S(ColDim, "!")
	if got := VisWidth(styled); got != 5 {
		t.Errorf("VisWidth = %d, want 5 (got %q)", got, StripANSI(styled))
	}
	if got := StripANSI(styled); got != "boom!" {
		t.Errorf("StripANSI = %q", got)
	}
}

func TestPadTo(t *testing.T) {
	s := Styler{Mode: TrueColor}
	styled := s.S(ColText, "abc")
	out := PadTo(styled, 8)
	if VisWidth(out) != 8 {
		t.Errorf("PadTo width = %d, want 8", VisWidth(out))
	}
	if !strings.HasSuffix(out, "     ") {
		t.Errorf("PadTo should right-pad, got %q", StripANSI(out))
	}
	// Overflow truncates rather than overrunning the column.
	if got := PadTo("abcdefgh", 4); VisWidth(got) != 4 {
		t.Errorf("PadTo overflow width = %d", VisWidth(got))
	}
	if got := PadLeft("7", 4); got != "   7" {
		t.Errorf("PadLeft = %q", got)
	}
}

func TestBar(t *testing.T) {
	if got := Bar(1, 4); got != "████" {
		t.Errorf("Bar(1) = %q", got)
	}
	if got := Bar(0, 4); got != "░░░░" {
		t.Errorf("Bar(0) = %q", got)
	}
	if got := Bar(0.5, 4); got != "██░░" {
		t.Errorf("Bar(0.5) = %q", got)
	}
	// Clamping.
	if got := Bar(5, 3); got != "███" {
		t.Errorf("Bar(5) = %q", got)
	}
	if got := Bar(-1, 3); got != "░░░" {
		t.Errorf("Bar(-1) = %q", got)
	}
	for _, w := range []int{1, 3, 8, 24} {
		for _, f := range []float64{0, 0.13, 0.5, 0.77, 1} {
			if n := utf8.RuneCountInString(Bar(f, w)); n != w {
				t.Errorf("Bar(%v, %d) produced %d cells", f, w, n)
			}
		}
	}
}

func TestHumanCount(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{7, "7"},
		{999, "999"},
		{5420, "5420"},
		{12300, "12.3k"},
		{1_500_000, "1.5M"},
		{2_000_000_000, "2B"},
	}
	for _, c := range cases {
		if got := HumanCount(c.in); got != c.want {
			t.Errorf("HumanCount(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHumanDurAndRelTime(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{9 * time.Second, "9s"},
		{90 * time.Second, "1m30s"},
		{3661 * time.Second, "1h01m01s"},
		{-5 * time.Second, "0s"},
	}
	for _, c := range cases {
		if got := HumanDur(c.d); got != c.want {
			t.Errorf("HumanDur(%v) = %q, want %q", c.d, got, c.want)
		}
	}
	rel := []struct {
		d    time.Duration
		want string
	}{
		{100 * time.Millisecond, "now"},
		{3 * time.Second, "3s ago"},
		{2 * time.Minute, "2m ago"},
		{5 * time.Hour, "5h ago"},
	}
	for _, c := range rel {
		if got := RelTime(c.d); got != c.want {
			t.Errorf("RelTime(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestEaseConverges(t *testing.T) {
	v := 0.0
	for i := 0; i < 200; i++ {
		v = Ease(v, 42, 0.2)
	}
	if v != 42 {
		t.Errorf("Ease did not snap to the target, got %v", v)
	}
	// It must move monotonically toward the target, never past it.
	v = 0
	for i := 0; i < 5; i++ {
		next := Ease(v, 10, 0.3)
		if next < v || next > 10 {
			t.Fatalf("Ease overshot: %v -> %v", v, next)
		}
		v = next
	}
}

func TestColorFallbacks(t *testing.T) {
	if got := (Styler{Mode: NoColor}).S(ColSev3, "x"); got != "x" {
		t.Errorf("NoColor should not emit escapes, got %q", got)
	}
	tc := (Styler{Mode: TrueColor}).Fg(RGB{1, 2, 3})
	if tc != "\x1b[38;2;1;2;3m" {
		t.Errorf("truecolor escape = %q", tc)
	}
	c256 := (Styler{Mode: Color256}).Fg(RGB{0xff, 0x00, 0x00})
	if !strings.HasPrefix(c256, "\x1b[38;5;") {
		t.Errorf("256-colour escape = %q", c256)
	}
	// Greys route to the greyscale ramp, which is smoother than the cube.
	idx := cube256(RGB{0x80, 0x80, 0x80})
	if idx < 232 || idx > 255 {
		t.Errorf("grey mapped to %d, want the 232..255 ramp", idx)
	}
	// Colours stay inside the 6x6x6 cube.
	for _, c := range []RGB{ColSev1, ColSev2, ColSev3, ColAccent, ColOK} {
		if i := cube256(c); i < 16 || i > 255 {
			t.Errorf("cube256(%v) = %d out of range", c, i)
		}
	}
}

func TestMixAndSevColor(t *testing.T) {
	a, b := RGB{0, 0, 0}, RGB{100, 200, 250}
	if got := Mix(a, b, 0); got != a {
		t.Errorf("Mix at 0 = %v", got)
	}
	if got := Mix(a, b, 1); got != b {
		t.Errorf("Mix at 1 = %v", got)
	}
	if got := Mix(a, b, 0.5); got.G != 100 {
		t.Errorf("Mix midpoint = %v", got)
	}
	// Out-of-range factors clamp.
	if got := Mix(a, b, 4); got != b {
		t.Errorf("Mix clamp high = %v", got)
	}
	// Higher severity is closer to the coral end.
	if SevColor(3, 1) == SevColor(1, 1) {
		t.Error("severities should be visually distinct")
	}
	// Intensity brightens rather than changing hue family.
	dim, bright := SevColor(3, 0), SevColor(3, 1)
	if dim == bright {
		t.Error("intensity should change the colour")
	}
	for _, k := range []string{"novel", "spike", "burst", "param", "silence", "other"} {
		if KindGlyph(k) == "" {
			t.Errorf("no glyph for %q", k)
		}
	}
}
