package tui

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/neelbarmecha/lookout/internal/engine"
)

// buildApp returns a dashboard backed by a stream that contains a scripted
// incident, without touching a terminal.
func buildApp(t *testing.T) *App {
	t.Helper()
	eng := engine.New(engine.DefaultConfig())
	base := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	for s := 0; s < 45; s++ {
		at := base.Add(time.Duration(s) * time.Second)
		for i := 0; i < 20; i++ {
			eng.Feed(fmt.Sprintf("%s INFO request method=GET path=/api/orders status=200 latency=%dms user=u%d",
				at.Format(time.RFC3339), 38+i, 1000+i), at)
		}
	}
	at := base.Add(46 * time.Second)
	eng.Feed(at.Format(time.RFC3339)+" ERROR connection pool exhausted active=64 max=64 waiters=37", at)
	eng.Tick(base.Add(47 * time.Second))

	a := NewApp(eng, nil, "test.log")
	a.st = Styler{Mode: TrueColor}
	return a
}

// checkFrame asserts the invariant the diff renderer depends on: the frame is
// exactly h rows and every row occupies exactly w visible cells.
func checkFrame(t *testing.T, a *App, w, h int) []string {
	t.Helper()
	rows := a.render(w, h)
	if len(rows) != h {
		t.Fatalf("%dx%d: frame has %d rows", w, h, len(rows))
	}
	for i, r := range rows {
		if got := VisWidth(r); got != w {
			t.Errorf("%dx%d: row %d is %d cells: %q", w, h, i, got, StripANSI(r))
		}
		if strings.ContainsAny(StripANSI(r), "\n\r\t") {
			t.Errorf("%dx%d: row %d contains a control character", w, h, i)
		}
	}
	return rows
}

func TestFrameGeometry(t *testing.T) {
	a := buildApp(t)
	sizes := [][2]int{
		{80, 24}, {120, 40}, {200, 60}, {100, 30}, {99, 20}, {60, 15}, {45, 12}, {300, 100},
	}
	for _, s := range sizes {
		checkFrame(t, a, s[0], s[1])
	}
}

func TestFrameGeometryAcrossToggles(t *testing.T) {
	a := buildApp(t)
	for _, tmpl := range []bool{true, false} {
		for _, anom := range []bool{true, false} {
			for _, filter := range []string{"", "checkout", "pool"} {
				a.showTmpl, a.anomOnly, a.filter = tmpl, anom, filter
				checkFrame(t, a, 120, 36)
				checkFrame(t, a, 80, 24)
			}
		}
	}
	a.paused = true
	a.frozen = a.eng.Recent(50, "", false)
	checkFrame(t, a, 120, 36)
	a.editing = true
	a.filter = "some long filter text that should not overflow the status bar"
	checkFrame(t, a, 80, 24)
}

func TestFrameIsStableBetweenIdenticalFrames(t *testing.T) {
	a := buildApp(t)
	a.paused = true
	a.frozen = a.eng.Recent(50, "", false)
	// Let the eased values settle so that only the animated chrome differs.
	for i := 0; i < 80; i++ {
		a.render(120, 36)
	}
	first := a.render(120, 36)
	second := a.render(120, 36)
	changed := 0
	for i := range first {
		if first[i] != second[i] {
			changed++
		}
	}
	// Only the header (pulse, spinner, uptime) may differ frame to frame; if
	// the whole screen changed every tick the diff renderer would be useless.
	if changed > 3 {
		t.Errorf("%d of %d rows changed between identical frames", changed, len(first))
	}
}

func TestNoColorModeEmitsNoEscapes(t *testing.T) {
	a := buildApp(t)
	a.st = Styler{Mode: NoColor}
	rows := a.render(100, 30)
	for i, r := range rows {
		if strings.Contains(r, "\x1b") {
			t.Fatalf("row %d contains an escape sequence in NoColor mode: %q", i, r)
		}
	}
}

func TestBoxDrawsTitledBorders(t *testing.T) {
	a := buildApp(t)
	rows := a.box("hello", 20, 4, []string{"body"}, ColBorder)
	if len(rows) != 4 {
		t.Fatalf("box returned %d rows", len(rows))
	}
	top := StripANSI(rows[0])
	if !strings.HasPrefix(top, "╭─ hello ") || !strings.HasSuffix(top, "╮") {
		t.Errorf("top border = %q", top)
	}
	bottom := StripANSI(rows[3])
	if !strings.HasPrefix(bottom, "╰") || !strings.HasSuffix(bottom, "╯") {
		t.Errorf("bottom border = %q", bottom)
	}
	for i, r := range rows {
		if VisWidth(r) != 20 {
			t.Errorf("box row %d is %d cells", i, VisWidth(r))
		}
	}
	// A title longer than the box must not break the geometry.
	for _, r := range a.box(strings.Repeat("x", 100), 20, 3, nil, ColBorder) {
		if VisWidth(r) != 20 {
			t.Errorf("long title broke the box: %q", StripANSI(r))
		}
	}
}

func TestJoinLR(t *testing.T) {
	got := StripANSI(joinLR("left", "right", 20))
	if len(got) != 20 || !strings.HasPrefix(got, "left") || !strings.HasSuffix(got, "right") {
		t.Errorf("joinLR = %q", got)
	}
	// When the two sides do not fit, the right side wins and the width holds.
	for _, w := range []int{4, 6, 9, 12} {
		if n := VisWidth(joinLR("a very long left side", "right", w)); n != w {
			t.Errorf("joinLR width %d = %d", w, n)
		}
	}
}

func TestKeyHandling(t *testing.T) {
	a := buildApp(t)
	if a.key('q') != true {
		t.Error("q should quit")
	}
	if a.key(3) != true {
		t.Error("ctrl-c should quit")
	}
	a.key('p')
	if !a.paused || a.frozen == nil {
		t.Error("p should pause and freeze the view")
	}
	a.key('p')
	if a.paused || a.frozen != nil {
		t.Error("p should resume")
	}
	a.key('a')
	if !a.anomOnly {
		t.Error("a should toggle anomalies-only")
	}
	a.key('t')
	if a.showTmpl {
		t.Error("t should toggle the template pane")
	}
	a.key('/')
	if !a.editing {
		t.Error("/ should start filter editing")
	}
	for _, b := range []byte("pool") {
		a.key(b)
	}
	if a.filter != "pool" {
		t.Errorf("filter = %q", a.filter)
	}
	a.key(127)
	if a.filter != "poo" {
		t.Errorf("backspace gave %q", a.filter)
	}
	a.key(13)
	if a.editing {
		t.Error("enter should end filter editing")
	}
	a.key('c')
	if a.filter != "" {
		t.Error("c should clear the filter")
	}
}

// TestDumpFrame is a development aid: run it with LOOKOUT_DUMP=1 to print a
// real frame to the terminal and eyeball the design without a live stream.
//
//	LOOKOUT_DUMP=1 go test ./internal/tui -run TestDumpFrame -v
func TestDumpFrame(t *testing.T) {
	if os.Getenv("LOOKOUT_DUMP") == "" {
		t.Skip("set LOOKOUT_DUMP=1 to print a frame")
	}
	a := buildApp(t)
	for i := 0; i < 40; i++ {
		a.render(118, 30)
	}
	for _, r := range a.render(118, 30) {
		os.Stdout.WriteString(r + "\x1b[0m\n")
	}
}
