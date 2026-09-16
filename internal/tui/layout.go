// Package tui renders lookout's interactive terminal UI and its designed
// report output using raw ANSI escapes only. The functions in this file are
// pure layout primitives and are covered by tests.
package tui

import (
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// sparkRunes are the eight one-eighth block heights.
var sparkRunes = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// barRunes are the eight one-eighth horizontal fills.
var barRunes = []rune{'▏', '▎', '▍', '▌', '▋', '▊', '▉', '█'}

// StripANSI removes CSI escape sequences from s.
func StripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
				j++
			}
			i = j + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// VisWidth returns the number of visible cells a styled string occupies.
func VisWidth(s string) int { return utf8.RuneCountInString(StripANSI(s)) }

// Truncate shortens plain text to at most w cells, adding an ellipsis when it
// had to cut. It never returns more than w cells.
func Truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	r := []rune(s)
	return string(r[:w-1]) + "…"
}

// PadTo right-pads a styled string to exactly w visible cells, truncating
// plain text if it overflows.
func PadTo(s string, w int) string {
	n := VisWidth(s)
	if n == w {
		return s
	}
	if n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return Truncate(s, w)
}

// PadLeft left-pads plain text to w cells.
func PadLeft(s string, w int) string {
	n := utf8.RuneCountInString(s)
	if n >= w {
		return Truncate(s, w)
	}
	return strings.Repeat(" ", w-n) + s
}

// Sparkline renders values as unicode block characters. The series is right
// aligned into width cells: the newest sample is on the right. max fixes the
// vertical scale; pass 0 to auto-scale.
func Sparkline(vals []float64, width int, max float64) string {
	if width <= 0 {
		return ""
	}
	if len(vals) > width {
		vals = vals[len(vals)-width:]
	}
	if max <= 0 {
		for _, v := range vals {
			if v > max {
				max = v
			}
		}
	}
	var b strings.Builder
	for i := 0; i < width-len(vals); i++ {
		b.WriteRune(' ')
	}
	for _, v := range vals {
		if max <= 0 || v <= 0 {
			b.WriteRune('▁')
			continue
		}
		idx := int(math.Round(v / max * float64(len(sparkRunes)-1)))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sparkRunes) {
			idx = len(sparkRunes) - 1
		}
		b.WriteRune(sparkRunes[idx])
	}
	return b.String()
}

// Bar renders a fractional progress bar with eighth-block precision.
func Bar(frac float64, width int) string {
	if width <= 0 {
		return ""
	}
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	total := frac * float64(width)
	full := int(total)
	rem := total - float64(full)
	var b strings.Builder
	for i := 0; i < full && i < width; i++ {
		b.WriteRune('█')
	}
	n := full
	if n < width && rem > 0.06 {
		idx := int(rem * 8)
		if idx > 7 {
			idx = 7
		}
		b.WriteRune(barRunes[idx])
		n++
	}
	for ; n < width; n++ {
		b.WriteRune('░')
	}
	return b.String()
}

// HumanCount abbreviates large counts.
func HumanCount(v float64) string {
	a := math.Abs(v)
	switch {
	case a >= 1e9:
		return trimZero(v/1e9) + "B"
	case a >= 1e6:
		return trimZero(v/1e6) + "M"
	case a >= 1e4:
		return trimZero(v/1e3) + "k"
	default:
		return trimZero(v)
	}
}

func trimZero(v float64) string {
	s := formatFloat(v, 1)
	s = strings.TrimSuffix(s, ".0")
	return s
}

func formatFloat(v float64, prec int) string {
	neg := v < 0
	if neg {
		v = -v
	}
	mult := math.Pow(10, float64(prec))
	n := int64(math.Round(v * mult))
	whole := n / int64(mult)
	frac := n % int64(mult)
	s := itoa(whole)
	if prec > 0 {
		f := itoa(frac)
		for len(f) < prec {
			f = "0" + f
		}
		s += "." + f
	}
	if neg {
		s = "-" + s
	}
	return s
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// HumanDur renders an uptime-style duration.
func HumanDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d.Seconds())
	h, m, sec := s/3600, (s%3600)/60, s%60
	if h > 0 {
		return itoa(int64(h)) + "h" + pad2(m) + "m" + pad2(sec) + "s"
	}
	if m > 0 {
		return itoa(int64(m)) + "m" + pad2(sec) + "s"
	}
	return itoa(int64(sec)) + "s"
}

func pad2(n int) string {
	if n < 10 {
		return "0" + itoa(int64(n))
	}
	return itoa(int64(n))
}

// RelTime renders a compact "time since" label.
func RelTime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Second:
		return "now"
	case d < time.Minute:
		return itoa(int64(d.Seconds())) + "s ago"
	case d < time.Hour:
		return itoa(int64(d.Minutes())) + "m ago"
	default:
		return itoa(int64(d.Hours())) + "h ago"
	}
}

// Ease moves cur toward target by factor k (0..1), snapping when very close.
func Ease(cur, target, k float64) float64 {
	next := cur + (target-cur)*k
	if math.Abs(target-next) < math.Abs(target)*0.001 || math.Abs(target-next) < 0.01 {
		return target
	}
	return next
}
