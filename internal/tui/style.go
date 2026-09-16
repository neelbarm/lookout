package tui

import (
	"os"
	"strconv"
	"strings"
)

// ColorMode is the colour depth lookout will use.
type ColorMode int

const (
	NoColor ColorMode = iota
	Color256
	TrueColor
)

// RGB is a 24-bit colour.
type RGB struct{ R, G, B uint8 }

// The palette is deliberately muted: desaturated blues and greys for chrome,
// warm amber through coral for severity. Nothing neon.
var (
	ColBrandA   = RGB{0x6f, 0x8f, 0xd6} // brand block, cool
	ColBrandB   = RGB{0x9a, 0x86, 0xc8} // brand block, warm
	ColBorder   = RGB{0x44, 0x4c, 0x60}
	ColTitle    = RGB{0xc6, 0xcd, 0xdd}
	ColText     = RGB{0xb4, 0xbc, 0xcc}
	ColDim      = RGB{0x6b, 0x73, 0x88}
	ColFaint    = RGB{0x50, 0x58, 0x6c}
	ColOK       = RGB{0x8a, 0xb6, 0x74}
	ColInfo     = RGB{0x72, 0xa8, 0xc4}
	ColAccent   = RGB{0x7f, 0x9c, 0xd8}
	ColSev1     = RGB{0xc9, 0xa2, 0x62} // amber
	ColSev2     = RGB{0xd2, 0x8c, 0x5e} // orange
	ColSev3     = RGB{0xcf, 0x6f, 0x7f} // coral
	ColMagenta  = RGB{0xa6, 0x8a, 0xc4}
	ColFlashBg1 = RGB{0x3a, 0x30, 0x22}
	ColFlashBg2 = RGB{0x44, 0x2e, 0x22}
	ColFlashBg3 = RGB{0x48, 0x28, 0x30}
)

// Styler emits escape sequences at the detected colour depth.
type Styler struct{ Mode ColorMode }

// DetectColor inspects the environment for colour support.
func DetectColor() ColorMode {
	if os.Getenv("NO_COLOR") != "" {
		return NoColor
	}
	term := os.Getenv("TERM")
	if term == "dumb" || term == "" {
		return NoColor
	}
	ct := strings.ToLower(os.Getenv("COLORTERM"))
	if strings.Contains(ct, "truecolor") || strings.Contains(ct, "24bit") {
		return TrueColor
	}
	if strings.Contains(term, "256color") || strings.Contains(term, "kitty") || strings.Contains(term, "alacritty") {
		return Color256
	}
	return Color256
}

// cube256 maps an RGB colour onto the xterm 256 palette.
func cube256(c RGB) int {
	q := func(v uint8) int {
		switch {
		case v < 48:
			return 0
		case v < 115:
			return 1
		default:
			return int((int(v) - 35) / 40)
		}
	}
	r, g, b := q(c.R), q(c.G), q(c.B)
	if r > 5 {
		r = 5
	}
	if g > 5 {
		g = 5
	}
	if b > 5 {
		b = 5
	}
	// Prefer the greyscale ramp for near-grey colours: it is much smoother.
	mx, mn := max3(c.R, c.G, c.B), min3(c.R, c.G, c.B)
	if int(mx)-int(mn) < 16 {
		g0 := (int(mx) + int(mn)) / 2
		idx := (g0 - 8) / 10
		if idx < 0 {
			idx = 0
		}
		if idx > 23 {
			idx = 23
		}
		return 232 + idx
	}
	return 16 + 36*r + 6*g + b
}

func max3(a, b, c uint8) uint8 {
	if b > a {
		a = b
	}
	if c > a {
		a = c
	}
	return a
}

func min3(a, b, c uint8) uint8 {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// Fg returns a foreground colour escape.
func (s Styler) Fg(c RGB) string {
	switch s.Mode {
	case TrueColor:
		return "\x1b[38;2;" + strconv.Itoa(int(c.R)) + ";" + strconv.Itoa(int(c.G)) + ";" + strconv.Itoa(int(c.B)) + "m"
	case Color256:
		return "\x1b[38;5;" + strconv.Itoa(cube256(c)) + "m"
	}
	return ""
}

// Bg returns a background colour escape.
func (s Styler) Bg(c RGB) string {
	switch s.Mode {
	case TrueColor:
		return "\x1b[48;2;" + strconv.Itoa(int(c.R)) + ";" + strconv.Itoa(int(c.G)) + ";" + strconv.Itoa(int(c.B)) + "m"
	case Color256:
		return "\x1b[48;5;" + strconv.Itoa(cube256(c)) + "m"
	}
	return ""
}

// off restores the default foreground colour.
func (s Styler) off() string {
	if s.Mode == NoColor {
		return ""
	}
	return "\x1b[39m"
}

// bgOff restores the default background colour.
func (s Styler) bgOff() string {
	if s.Mode == NoColor {
		return ""
	}
	return "\x1b[49m"
}

// Reset clears all attributes.
func (s Styler) Reset() string {
	if s.Mode == NoColor {
		return ""
	}
	return "\x1b[0m"
}

// Bold turns on bold.
func (s Styler) Bold() string {
	if s.Mode == NoColor {
		return ""
	}
	return "\x1b[1m"
}

// S wraps text in a foreground colour.
func (s Styler) S(c RGB, text string) string {
	if s.Mode == NoColor {
		return text
	}
	return s.Fg(c) + text + "\x1b[39m"
}

// B wraps text in bold plus a foreground colour.
func (s Styler) B(c RGB, text string) string {
	if s.Mode == NoColor {
		return text
	}
	return "\x1b[1m" + s.Fg(c) + text + "\x1b[0m"
}

// Mix linearly blends two colours; t=0 returns a, t=1 returns b.
func Mix(a, b RGB, t float64) RGB {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	f := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t) }
	return RGB{f(a.R, b.R), f(a.G, b.G), f(a.B, b.B)}
}

// SevColor returns the palette entry for a severity, brightened by intensity.
func SevColor(sev int, intensity float64) RGB {
	var base RGB
	switch {
	case sev >= 3:
		base = ColSev3
	case sev == 2:
		base = ColSev2
	default:
		base = ColSev1
	}
	return Mix(ColDim, base, 0.55+0.45*clamp01(intensity))
}

// SevBg returns the flash background for a severity.
func SevBg(sev int) RGB {
	switch {
	case sev >= 3:
		return ColFlashBg3
	case sev == 2:
		return ColFlashBg2
	default:
		return ColFlashBg1
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// KindGlyph is the icon-like glyph shown for each anomaly kind.
func KindGlyph(kind string) string {
	switch kind {
	case "novel":
		return "✦"
	case "spike":
		return "▲"
	case "burst":
		return "◆"
	case "param":
		return "◉"
	case "silence":
		return "○"
	}
	return "•"
}
