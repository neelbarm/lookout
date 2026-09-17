package tui

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/neelbarmecha/lookout/internal/detect"
	"github.com/neelbarmecha/lookout/internal/engine"
)

const (
	fps       = 12
	frameDur  = time.Second / fps
	flashLife = 1100 * time.Millisecond
)

var spinner = []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}

// App is the interactive dashboard.
type App struct {
	eng  *engine.Engine
	st   Styler
	term *Term
	src  <-chan engine.Event
	name string

	paused   bool
	anomOnly bool
	showTmpl bool
	filter   string
	editing  bool
	done     bool

	frame     int
	startWall time.Time

	eLines, eLPS, eTmpl, eAnom float64
	gHist                      []float64
	tHist                      map[int][]float64

	lastCount int
	lastAt    time.Time
	lps       float64

	frozen []engine.Line

	prev []string
	w, h int
}

// NewApp builds the dashboard around an engine and a source.
func NewApp(eng *engine.Engine, src <-chan engine.Event, name string) *App {
	return &App{
		eng: eng, src: src, name: name,
		st:        Styler{Mode: DetectColor()},
		showTmpl:  true,
		startWall: time.Now(),
		tHist:     map[int][]float64{},
		lastAt:    time.Now(),
	}
}

// Run drives the UI until the user quits or ctx is cancelled. The terminal is
// always restored, including on panic.
func (a *App) Run(ctx context.Context) error {
	term, err := OpenTerm()
	if err != nil {
		return err
	}
	a.term = term
	defer term.Close()

	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)

	keys := term.Keys()
	tick := time.NewTicker(frameDur)
	defer tick.Stop()

	a.w, a.h = term.Size()
	a.draw()

	for {
		select {
		case <-ctx.Done():
			return nil

		case s := <-sig:
			switch s {
			case syscall.SIGWINCH:
				a.w, a.h = term.Size()
				a.prev = nil
				term.Write("\x1b[2J")
			default:
				return nil
			}

		case b, ok := <-keys:
			if !ok {
				return nil
			}
			if a.key(b) {
				return nil
			}

		case ev, ok := <-a.src:
			if !ok {
				a.done = true
				a.src = nil
				continue
			}
			a.eng.Feed(ev.Text, ev.At)
			// Drain whatever else is queued so that a burst does not starve
			// the render loop.
			for i := 0; i < 4096; i++ {
				select {
				case ev2, ok2 := <-a.src:
					if !ok2 {
						a.done = true
						a.src = nil
						i = 4096
						continue
					}
					a.eng.Feed(ev2.Text, ev2.At)
				default:
					i = 4096
				}
			}

		case <-tick.C:
			a.advanceClock()
			a.draw()
		}
	}
}

// advanceClock keeps the detector's second buckets moving when the stream is
// idle. When the stream carries historical timestamps we follow those instead
// of wall time so replays behave.
func (a *App) advanceClock() {
	now := time.Now()
	_, last := a.eng.Span()
	if !last.IsZero() {
		if d := now.Sub(last); d < 0 || d > 5*time.Second {
			now = last
		}
	}
	a.eng.Tick(now)
}

func (a *App) key(b byte) bool {
	if a.editing {
		switch b {
		case 13, 10, 27: // enter, esc
			a.editing = false
		case 127, 8:
			if n := len(a.filter); n > 0 {
				a.filter = a.filter[:n-1]
			}
		default:
			if b >= 32 && b < 127 {
				a.filter += string(rune(b))
			}
		}
		return false
	}
	switch b {
	case 'q', 'Q', 3: // q or Ctrl-C
		return true
	case 'p', 'P', ' ':
		a.paused = !a.paused
		if a.paused {
			a.frozen = a.eng.Recent(400, a.filter, a.anomOnly)
		} else {
			a.frozen = nil
		}
	case 'a', 'A':
		a.anomOnly = !a.anomOnly
	case 't', 'T':
		a.showTmpl = !a.showTmpl
		a.prev = nil
	case '/':
		a.editing = true
	case 'c', 'C':
		a.filter = ""
	}
	return false
}

// draw renders one frame and writes only the rows that changed.
func (a *App) draw() {
	a.frame++
	if a.w < 40 || a.h < 10 {
		a.drawTooSmall()
		return
	}
	rows := a.render(a.w, a.h)
	var b strings.Builder
	b.Grow(len(rows) * a.w * 2)
	for i, r := range rows {
		if i < len(a.prev) && a.prev[i] == r {
			continue
		}
		fmt.Fprintf(&b, "\x1b[%d;1H", i+1)
		b.WriteString(r)
		b.WriteString("\x1b[0m\x1b[K")
	}
	b.WriteString("\x1b[H")
	a.term.Write(b.String())
	a.term.Flush()
	a.prev = rows
}

// drawTooSmall says why the dashboard is blank, instead of leaving the user
// looking at an empty alternate screen with no way to tell what went wrong.
func (a *App) drawTooSmall() {
	msg := Truncate(fmt.Sprintf("terminal is %dx%d; lookout needs at least 40x10", a.w, a.h), a.w)
	if len(a.prev) == 1 && a.prev[0] == msg {
		return
	}
	a.term.Write("\x1b[2J\x1b[1;1H" + msg)
	a.term.Flush()
	a.prev = []string{msg}
}

func (a *App) render(w, h int) []string {
	a.tickEase()

	out := make([]string, 0, h)
	head := a.header(w)
	out = append(out, head...)

	bodyH := h - len(head) - 1
	if bodyH < 3 {
		bodyH = 3
	}
	rightW := 0
	if w >= 100 {
		rightW = w * 38 / 100
		if rightW > 52 {
			rightW = 52
		}
		if rightW < 34 {
			rightW = 34
		}
	}
	leftW := w - rightW

	left := a.streamPane(leftW, bodyH)
	if rightW > 0 {
		var right []string
		if a.showTmpl {
			tH := bodyH / 2
			if tH < 5 {
				tH = 5
			}
			if tH > bodyH-5 {
				tH = bodyH - 5
			}
			right = append(right, a.templatePane(rightW, tH)...)
			right = append(right, a.anomalyPane(rightW, bodyH-tH)...)
		} else {
			right = a.anomalyPane(rightW, bodyH)
		}
		for i := 0; i < bodyH; i++ {
			l, r := "", ""
			if i < len(left) {
				l = left[i]
			}
			if i < len(right) {
				r = right[i]
			}
			out = append(out, PadTo(l, leftW)+PadTo(r, rightW))
		}
	} else {
		out = append(out, left...)
	}
	out = append(out, a.statusBar(w))

	for len(out) < h {
		out = append(out, strings.Repeat(" ", w))
	}
	return out[:h]
}

func (a *App) tickEase() {
	det := a.eng.Det
	now := time.Now()
	if d := now.Sub(a.lastAt); d >= 400*time.Millisecond {
		n := a.eng.Total()
		a.lps = float64(n-a.lastCount) / d.Seconds()
		a.lastCount, a.lastAt = n, now
	}
	k := 0.22
	a.eLines = Ease(a.eLines, float64(a.eng.Total()), k)
	a.eLPS = Ease(a.eLPS, a.lps, 0.15)
	a.eTmpl = Ease(a.eTmpl, float64(len(a.eng.Miner.Clusters())), k)
	a.eAnom = Ease(a.eAnom, float64(det.Anomalies()), k)

	g := det.GlobalHistory()
	if len(a.gHist) != len(g) {
		a.gHist = append([]float64(nil), g...)
	} else {
		for i := range g {
			a.gHist[i] = Ease(a.gHist[i], g[i], 0.3)
		}
	}
}

// ---------- header ----------

func (a *App) header(w int) []string {
	s := a.st
	det := a.eng.Det
	inner := w - 4

	// Two-tone brand block plus wordmark.
	brand := s.Fg(ColBrandA) + "▄" + s.Fg(ColBrandB) + "▀" + s.off()
	left := brand + " " + s.B(ColTitle, "LOOKOUT") + " " + s.S(ColFaint, "│ ") +
		s.S(ColDim, "real-time log anomaly detection")

	// Pulsing LIVE indicator.
	pulse := 0.5 + 0.5*math.Sin(float64(a.frame)/float64(fps)*2.2)
	dotCol := Mix(ColFaint, ColOK, pulse)
	state := s.Fg(dotCol) + "●" + s.off() + " " + s.S(ColDim, "LIVE")
	if a.paused {
		state = s.Fg(ColSev1) + "‖" + s.off() + " " + s.S(ColSev1, "PAUSED")
	} else if a.done {
		state = s.Fg(ColFaint) + "◼" + s.off() + " " + s.S(ColDim, "EOF")
	}
	up := s.S(ColFaint, HumanDur(time.Since(a.startWall)))
	right := state + "  " + up
	row1 := joinLR(left, right, inner)

	metrics := []string{
		s.S(ColDim, "lines ") + s.B(ColText, HumanCount(a.eLines)),
		s.B(ColAccent, fmt.Sprintf("%.1f", a.eLPS)) + s.S(ColDim, "/s"),
		s.S(ColDim, "templates ") + s.B(ColText, fmt.Sprintf("%d", int(math.Round(a.eTmpl)))),
		s.S(ColDim, "anomalies ") + a.anomCount(),
	}
	mrow := strings.Join(metrics, s.S(ColFaint, "  ·  "))

	var tail string
	if !det.Warm() {
		sp := string(spinner[a.frame%len(spinner)])
		p := det.WarmProgress()
		tail = s.S(ColInfo, sp) + " " + s.S(ColDim, "learning baseline ") +
			s.Fg(ColInfo) + Bar(p, 14) + s.off() + s.S(ColFaint, fmt.Sprintf(" %3.0f%%", p*100))
	} else {
		mx := 0.0
		for _, v := range a.gHist {
			if v > mx {
				mx = v
			}
		}
		spark := Sparkline(a.gHist, 24, mx)
		tail = s.S(ColFaint, "60s ") + s.Fg(ColAccent) + spark + s.off() +
			s.S(ColFaint, fmt.Sprintf(" %.0f", mx)) + s.S(ColFaint, "/s")
	}
	row2 := joinLR(mrow, tail, inner)

	return a.box(a.name, w, 4, []string{row1, row2}, ColBorder)
}

func (a *App) anomCount() string {
	s := a.st
	n := int(math.Round(a.eAnom))
	c := ColText
	if n > 0 {
		c = ColSev2
	}
	return s.B(c, fmt.Sprintf("%d", n))
}

// ---------- stream pane ----------

func (a *App) streamPane(w, h int) []string {
	inner := w - 4
	rowsN := h - 2
	if rowsN < 1 {
		rowsN = 1
	}

	lines := a.frozen
	if lines == nil {
		lines = a.eng.Recent(rowsN, a.filter, a.anomOnly)
	}
	if len(lines) > rowsN {
		lines = lines[len(lines)-rowsN:]
	}

	body := make([]string, 0, rowsN)
	for _, l := range lines {
		body = append(body, a.streamRow(l, inner))
	}
	for len(body) < rowsN {
		body = append(body, "")
	}

	title := "stream"
	if a.anomOnly {
		title = "stream · anomalies only"
	}
	if a.filter != "" {
		title += " · /" + a.filter
	}
	return a.box(title, w, h, body, ColBorder)
}

func (a *App) streamRow(l engine.Line, w int) string {
	s := a.st
	sev := l.Severity()
	ts := l.At.Format("15:04:05")

	var tag string
	if top, ok := l.Top(); ok {
		t := top.Tag
		if len(l.Anoms) > 1 {
			t = fmt.Sprintf("%s +%d", t, len(l.Anoms)-1)
		}
		tag = "[" + Truncate(t, 18) + "] "
	}

	textW := w - 9 - len([]rune(tag))
	if textW < 8 {
		textW = 8
	}
	body := l.Msg
	if body == "" {
		body = l.Raw
	}
	text := Truncate(collapseWS(body), textW)

	tsCol, txtCol := ColFaint, ColText
	switch l.Level {
	case "ERROR", "FATAL":
		txtCol = Mix(ColText, ColSev3, 0.6)
	case "WARN":
		txtCol = Mix(ColText, ColSev1, 0.55)
	case "DEBUG", "TRACE":
		txtCol = ColDim
	}

	row := s.S(tsCol, ts) + " "
	if tag != "" {
		age := time.Since(l.Wall)
		intensity := 1 - float64(age)/float64(flashLife)
		row += s.B(SevColor(sev, intensity), tag)
		row += s.Fg(SevColor(sev, math.Max(intensity, 0.35))) + text + s.off()
	} else {
		row += s.S(txtCol, text)
	}

	// Flash: a brief tinted background that decays over about a second.
	if sev > 0 && s.Mode != NoColor {
		age := time.Since(l.Wall)
		if age < flashLife {
			t := 1 - float64(age)/float64(flashLife)
			bg := Mix(RGB{0x15, 0x16, 0x1e}, SevBg(sev), t)
			return s.Bg(bg) + PadTo(row, w) + s.bgOff()
		}
	}
	return row
}

func collapseWS(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.TrimRight(s, " ")
}

// ---------- template pane ----------

func (a *App) templatePane(w, h int) []string {
	s := a.st
	inner := w - 4
	rowsN := h - 2
	stats := a.eng.Det.Snapshot(rowsN)

	body := make([]string, 0, rowsN)
	for _, t := range stats {
		hist := a.tHist[t.ID]
		if len(hist) != len(t.Hist) {
			hist = append([]float64(nil), t.Hist...)
		} else {
			for i := range hist {
				hist[i] = Ease(hist[i], t.Hist[i], 0.3)
			}
		}
		a.tHist[t.ID] = hist

		mx := 0.0
		for _, v := range hist {
			if v > mx {
				mx = v
			}
		}
		const sparkW = 12
		rate := PadLeft(fmt.Sprintf("%.1f/s", t.PerSec), 8)
		textW := inner - sparkW - 10
		if textW < 6 {
			textW = 6
		}
		txt := Truncate(t.Template, textW)

		col := ColDim
		if t.Anomalies > 0 {
			col = Mix(ColText, ColSev2, 0.5)
		}
		sparkCol := Mix(ColFaint, ColAccent, clamp01(t.PerSec/8))
		body = append(body,
			s.S(ColInfo, rate)+" "+
				s.Fg(sparkCol)+Sparkline(hist, sparkW, mx)+s.off()+" "+
				s.S(col, txt))
	}
	for len(body) < rowsN {
		body = append(body, "")
	}
	return a.box(fmt.Sprintf("templates · %d", len(a.eng.Miner.Clusters())), w, h, body, ColBorder)
}

// ---------- anomaly pane ----------

func (a *App) anomalyPane(w, h int) []string {
	s := a.st
	inner := w - 4
	rowsN := h - 2
	cards := rowsN / 4
	if cards < 1 {
		cards = 1
	}
	list := a.eng.LatestAnomalies(cards)

	body := make([]string, 0, rowsN)
	for _, an := range list {
		col := SevColor(an.Severity, 1)
		age := time.Since(a.wallOf(an))
		head := s.Fg(col) + KindGlyph(string(an.Kind)) + " " + s.Bold() + Truncate(an.Tag, 22) + s.Reset() +
			s.S(ColFaint, fmt.Sprintf("  #%d", an.TemplateID))
		body = append(body, joinLR(head, s.S(ColFaint, RelTime(age)), inner))
		for _, line := range wrap(an.Reason, inner-2, 2) {
			body = append(body, "  "+s.S(ColDim, line))
		}
		if len(body) < rowsN {
			body = append(body, "")
		}
	}
	if len(list) == 0 {
		msg := "nothing unusual yet"
		if !a.eng.Det.Warm() {
			msg = "learning what normal looks like…"
		}
		body = append(body, s.S(ColFaint, msg))
	}
	for len(body) < rowsN {
		body = append(body, "")
	}
	if len(body) > rowsN {
		body = body[:rowsN]
	}
	return a.box("anomalies", w, h, body, ColBorder)
}

// wallOf maps a stream timestamp onto wall time for relative-time display.
func (a *App) wallOf(an detect.Anomaly) time.Time {
	_, last := a.eng.Span()
	if last.IsZero() {
		return time.Now()
	}
	d := last.Sub(an.Time)
	if d < 0 {
		d = 0
	}
	return time.Now().Add(-d)
}

// ---------- status bar ----------

func (a *App) statusBar(w int) string {
	s := a.st
	key := func(k, label string, on bool) string {
		c := ColDim
		if on {
			c = ColAccent
		}
		return s.B(c, k) + s.S(ColFaint, " "+label)
	}
	left := "  " + strings.Join([]string{
		key("q", "quit", false),
		key("p", "pause", a.paused),
		key("a", "anomalies", a.anomOnly),
		key("t", "templates", a.showTmpl),
		key("/", "filter", a.filter != ""),
		key("c", "clear", false),
	}, s.S(ColFaint, "  ·  "))

	right := ""
	if a.editing {
		right = s.S(ColAccent, "/"+a.filter+"▏")
	} else if a.filter != "" {
		right = s.S(ColDim, "filter: ") + s.S(ColAccent, a.filter)
	}
	right += "  "
	return joinLR(left, right, w)
}

// ---------- box ----------

func (a *App) box(title string, w, h int, body []string, border RGB) []string {
	s := a.st
	if w < 6 || h < 3 {
		return make([]string, h)
	}
	inner := w - 4
	bc := s.Fg(border)
	rows := make([]string, 0, h)

	// The header row is "╭─ ", the title, " ", at least one rule and "╮", so a
	// title can never exceed w-6 cells without breaking the geometry.
	titleMax := w - 6
	if titleMax < 0 {
		titleMax = 0
	}
	t := Truncate(title, titleMax)
	fill := w - 5 - VisWidth(t)
	if fill < 1 {
		fill = 1
	}
	rows = append(rows, bc+"╭─ "+s.off()+s.S(ColTitle, t)+" "+bc+strings.Repeat("─", fill)+"╮"+s.off())

	for i := 0; i < h-2; i++ {
		content := ""
		if i < len(body) {
			content = body[i]
		}
		rows = append(rows, bc+"│"+s.off()+" "+PadTo(content, inner)+" "+bc+"│"+s.off())
	}
	rows = append(rows, bc+"╰"+strings.Repeat("─", w-2)+"╯"+s.off())
	return rows
}

// wrap breaks plain text onto at most maxLines lines of width w, ellipsizing
// whatever does not fit.
func wrap(text string, w, maxLines int) []string {
	if w <= 0 || maxLines <= 0 {
		return nil
	}
	words := strings.Fields(text)
	var out []string
	cur := ""
	for _, word := range words {
		switch {
		case cur == "":
			cur = word
		case len([]rune(cur))+1+len([]rune(word)) <= w:
			cur += " " + word
		default:
			out = append(out, cur)
			cur = word
			if len(out) == maxLines {
				break
			}
		}
	}
	if cur != "" && len(out) < maxLines {
		out = append(out, cur)
	}
	if len(out) > maxLines {
		out = out[:maxLines]
	}
	for i := range out {
		out[i] = Truncate(out[i], w)
	}
	// Mark the last line when the text was cut short.
	if len(out) == maxLines {
		joined := 0
		for _, l := range out {
			joined += len([]rune(l)) + 1
		}
		if joined-1 < len([]rune(text)) {
			out[maxLines-1] = Truncate(out[maxLines-1], w-1) + "\u2026"
		}
	}
	for len(out) < maxLines {
		out = append(out, "")
	}
	return out
}

// joinLR places left and right on one row of exactly w cells.
func joinLR(left, right string, w int) string {
	lw, rw := VisWidth(left), VisWidth(right)
	if lw+rw+1 > w {
		if rw+1 >= w {
			return PadTo(right, w)
		}
		return PadTo(Truncate(StripANSI(left), w-rw-1), w-rw) + right
	}
	return left + strings.Repeat(" ", w-lw-rw) + right
}
