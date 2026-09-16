package tui

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/neelbarmecha/lookout/internal/detect"
	"github.com/neelbarmecha/lookout/internal/engine"
)

// ReportOptions controls the offline report renderer.
type ReportOptions struct {
	Width      int
	Color      bool
	TopN       int
	MaxAnomaly int
	Source     string
}

// Report renders the designed offline report for a processed stream.
func Report(w io.Writer, eng *engine.Engine, opt ReportOptions) {
	s := Styler{Mode: NoColor}
	if opt.Color {
		s = Styler{Mode: DetectColor()}
	}
	width := opt.Width
	if width <= 0 {
		width = 96
	}
	if width > 140 {
		width = 140
	}
	if opt.TopN <= 0 {
		opt.TopN = 15
	}
	if opt.MaxAnomaly <= 0 {
		opt.MaxAnomaly = 40
	}

	first, last := eng.Span()
	dur := last.Sub(first)

	rule := func(title string) {
		t := " " + title + " "
		fill := width - 2 - VisWidth(t)
		if fill < 0 {
			fill = 0
		}
		fmt.Fprintln(w, s.Fg(ColBorder)+"╶─"+s.off()+s.B(ColTitle, t)+s.Fg(ColBorder)+strings.Repeat("─", fill)+s.off())
	}

	// Banner
	fmt.Fprintln(w)
	brand := s.Fg(ColBrandA) + "▄" + s.Fg(ColBrandB) + "▀" + s.off()
	fmt.Fprintln(w, "  "+brand+" "+s.B(ColTitle, "LOOKOUT")+"  "+s.S(ColDim, "log anomaly report"))
	if opt.Source != "" {
		fmt.Fprintln(w, "  "+s.S(ColFaint, opt.Source))
	}
	fmt.Fprintln(w)

	stats := eng.Det.Snapshot(0)
	fmt.Fprintln(w, "  "+strings.Join([]string{
		s.S(ColDim, "lines ") + s.B(ColText, HumanCount(float64(eng.Total()))),
		s.S(ColDim, "templates ") + s.B(ColText, fmt.Sprintf("%d", len(eng.Miner.Clusters()))),
		s.S(ColDim, "anomalies ") + s.B(ColSev2, fmt.Sprintf("%d", eng.Det.Anomalies())),
		s.S(ColDim, "span ") + s.B(ColText, HumanDur(dur)),
		s.S(ColDim, "rate ") + s.B(ColText, fmt.Sprintf("%.1f/s", rate(eng.Total(), dur))),
	}, s.S(ColFaint, "  ·  ")))
	fmt.Fprintln(w)

	// Volume histogram over the whole span.
	rule("volume")
	fmt.Fprintln(w)
	histogram(w, s, eng, width, dur)
	fmt.Fprintln(w)

	// Templates table
	rule(fmt.Sprintf("templates · top %d of %d", min(opt.TopN, len(stats)), len(stats)))
	fmt.Fprintln(w)
	templateTable(w, s, stats, eng.Total(), width, opt.TopN)
	fmt.Fprintln(w)

	// Anomalies
	an := eng.Anomalies()
	rule(fmt.Sprintf("anomalies · %d", eng.Det.Anomalies()))
	fmt.Fprintln(w)
	if len(an) == 0 {
		fmt.Fprintln(w, "  "+s.S(ColOK, "✓ nothing unusual — the stream matched its own baseline throughout"))
		fmt.Fprintln(w)
		return
	}
	byKind(w, s, eng, width)
	fmt.Fprintln(w)
	shown := an
	if len(shown) > opt.MaxAnomaly {
		shown = shown[len(shown)-opt.MaxAnomaly:]
	}
	for _, a := range shown {
		col := SevColor(a.Severity, 1)
		head := "  " + s.Fg(col) + KindGlyph(string(a.Kind)) + s.off() + " " +
			s.S(ColFaint, a.Time.Format("15:04:05")) + " " +
			s.B(col, PadTo(a.Tag, 16)) + " " +
			s.S(ColFaint, fmt.Sprintf("#%-4d", a.TemplateID))
		fmt.Fprintln(w, PadTo(head, width))
		fmt.Fprintln(w, "      "+s.S(ColText, Truncate(a.Reason, width-8)))
		if a.Line != "" {
			fmt.Fprintln(w, "      "+s.S(ColFaint, Truncate(collapseWS(a.Line), width-8)))
		}
		fmt.Fprintln(w)
	}
	if len(an) > len(shown) {
		fmt.Fprintln(w, "  "+s.S(ColFaint, fmt.Sprintf("… %d earlier anomalies not shown", len(an)-len(shown))))
		fmt.Fprintln(w)
	}
}

func rate(n int, d time.Duration) float64 {
	if d <= 0 {
		return float64(n)
	}
	return float64(n) / d.Seconds()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// histogram draws a per-bucket volume bar chart across the whole span, with
// anomaly markers underneath.
func histogram(w io.Writer, s Styler, eng *engine.Engine, width int, dur time.Duration) {
	first, last := eng.Span()
	if first.IsZero() || !last.After(first) {
		fmt.Fprintln(w, "  "+s.S(ColFaint, "(single instant — no histogram)"))
		return
	}
	cols := width - 10
	if cols < 20 {
		cols = 20
	}
	buckets := make([]float64, cols)
	marks := make([]int, cols)
	span := last.Sub(first)
	idx := func(t time.Time) int {
		i := int(float64(t.Sub(first)) / float64(span) * float64(cols-1))
		if i < 0 {
			i = 0
		}
		if i >= cols {
			i = cols - 1
		}
		return i
	}
	for sec, n := range eng.Timeline() {
		buckets[idx(time.Unix(sec, 0))] += float64(n)
	}
	for _, a := range eng.Anomalies() {
		i := idx(a.Time)
		if a.Severity > marks[i] {
			marks[i] = a.Severity
		}
	}
	mx := 0.0
	for _, v := range buckets {
		if v > mx {
			mx = v
		}
	}
	rows := 6
	for r := rows; r >= 1; r-- {
		line := "  " + s.S(ColFaint, PadLeft(fmt.Sprintf("%.0f", mx*float64(r)/float64(rows)), 6)) + " "
		var b strings.Builder
		for _, v := range buckets {
			lvl := 0.0
			if mx > 0 {
				lvl = v / mx * float64(rows)
			}
			switch {
			case lvl >= float64(r):
				b.WriteRune('█')
			case lvl >= float64(r)-0.5:
				b.WriteRune('▄')
			default:
				b.WriteRune(' ')
			}
		}
		fmt.Fprintln(w, line+s.Fg(ColAccent)+b.String()+s.off())
	}
	var mline strings.Builder
	for _, m := range marks {
		if m == 0 {
			mline.WriteRune(' ')
			continue
		}
		mline.WriteString(s.Fg(SevColor(m, 1)) + "▲")
	}
	fmt.Fprintln(w, "         "+mline.String()+s.off())
	fmt.Fprintln(w, "         "+s.S(ColFaint, PadTo(first.Format("15:04:05"), cols/2)+PadLeft(last.Format("15:04:05"), cols-cols/2)))
}

func templateTable(w io.Writer, s Styler, stats []detect.TemplateStat, total, width, topN int) {
	sorted := append([]detect.TemplateStat(nil), stats...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Count > sorted[j].Count })
	if len(sorted) > topN {
		sorted = sorted[:topN]
	}
	head := "  " + s.S(ColFaint, PadLeft("id", 5)+"  "+PadLeft("count", 8)+"  "+PadTo("share", 14)+"  template")
	fmt.Fprintln(w, head)
	for _, t := range sorted {
		share := 0.0
		if total > 0 {
			share = float64(t.Count) / float64(total)
		}
		col := ColText
		if t.Anomalies > 0 {
			col = Mix(ColText, ColSev2, 0.55)
		}
		bar := s.Fg(Mix(ColFaint, ColAccent, math.Min(1, share*3))) + Bar(share, 8) + s.off()
		tw := width - 40
		if tw < 20 {
			tw = 20
		}
		fmt.Fprintln(w, "  "+
			s.S(ColInfo, PadLeft(fmt.Sprintf("#%d", t.ID), 5))+"  "+
			s.S(ColText, PadLeft(HumanCount(float64(t.Count)), 8))+"  "+
			bar+s.S(ColFaint, PadLeft(fmt.Sprintf("%4.1f%%", share*100), 6))+"  "+
			s.S(col, Truncate(t.Template, tw)))
		if t.Example != "" && t.Anomalies > 0 {
			fmt.Fprintln(w, "         "+s.S(ColFaint, Truncate("e.g. "+collapseWS(t.Example), width-12)))
		}
	}
}

func byKind(w io.Writer, s Styler, eng *engine.Engine, width int) {
	kinds := []detect.Kind{detect.KindNovel, detect.KindSpike, detect.KindBurst, detect.KindParam, detect.KindSilence}
	counts := eng.ByKind()
	mx := 0
	for _, k := range kinds {
		if counts[k] > mx {
			mx = counts[k]
		}
	}
	for _, k := range kinds {
		n := counts[k]
		frac := 0.0
		if mx > 0 {
			frac = float64(n) / float64(mx)
		}
		col := ColDim
		if n > 0 {
			col = ColText
		}
		fmt.Fprintln(w, "  "+s.S(col, KindGlyph(string(k))+" "+PadTo(detect.KindLabel(k), 18))+
			s.S(ColFaint, PadLeft(fmt.Sprintf("%d", n), 4))+"  "+
			s.Fg(Mix(ColFaint, ColSev2, frac))+Bar(frac, 24)+s.off())
	}
}

// Summary writes the compact end-of-stream summary used in piped mode.
func Summary(w io.Writer, eng *engine.Engine, color bool) {
	s := Styler{Mode: NoColor}
	if color {
		s = Styler{Mode: DetectColor()}
	}
	first, last := eng.Span()
	fmt.Fprintln(w)
	fmt.Fprintln(w, s.B(ColTitle, "lookout summary"))
	fmt.Fprintf(w, "  lines       %d\n", eng.Total())
	fmt.Fprintf(w, "  templates   %d\n", len(eng.Miner.Clusters()))
	fmt.Fprintf(w, "  anomalies   %d\n", eng.Det.Anomalies())
	fmt.Fprintf(w, "  span        %s  (%s → %s)\n", HumanDur(last.Sub(first)),
		first.Format(time.RFC3339), last.Format(time.RFC3339))
	counts := eng.ByKind()
	for _, k := range []detect.Kind{detect.KindNovel, detect.KindSpike, detect.KindBurst, detect.KindParam, detect.KindSilence} {
		fmt.Fprintf(w, "    %-18s %d\n", detect.KindLabel(k), counts[k])
	}
	fmt.Fprintln(w)
	for _, t := range eng.Det.Snapshot(8) {
		fmt.Fprintf(w, "  #%-4d %7d  %s\n", t.ID, t.Count, Truncate(t.Template, 88))
	}
	fmt.Fprintln(w)
}
