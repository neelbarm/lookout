package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRingIsBoundedAndOrdered(t *testing.T) {
	r := newRing[int](4)
	for i := 1; i <= 10; i++ {
		r.push(i)
	}
	if r.len() != 4 {
		t.Fatalf("len = %d, want 4", r.len())
	}
	var fwd, back []int
	r.eachForward(func(v int) bool { fwd = append(fwd, v); return true })
	r.eachBackward(func(v int) bool { back = append(back, v); return true })
	if fmt.Sprint(fwd) != "[7 8 9 10]" {
		t.Errorf("forward = %v, want [7 8 9 10]", fwd)
	}
	if fmt.Sprint(back) != "[10 9 8 7]" {
		t.Errorf("backward = %v, want [10 9 8 7]", back)
	}
	// Early exit stops the walk.
	n := 0
	r.eachBackward(func(int) bool { n++; return n < 2 })
	if n != 2 {
		t.Errorf("early exit visited %d", n)
	}
}

func TestRingPartiallyFilled(t *testing.T) {
	r := newRing[int](5)
	r.push(1)
	r.push(2)
	var fwd []int
	r.eachForward(func(v int) bool { fwd = append(fwd, v); return true })
	if fmt.Sprint(fwd) != "[1 2]" {
		t.Errorf("forward = %v", fwd)
	}
}

func TestRecentFilters(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxRecent = 100
	e := New(cfg)
	base := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 50; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		e.Feed(fmt.Sprintf("%s INFO request path=/api/orders n=%d", at.Format(time.RFC3339), i), at)
	}
	at := base.Add(60 * time.Second)
	e.Feed(at.Format(time.RFC3339)+" ERROR checkout failed reason=timeout", at)

	if got := len(e.Recent(10, "", false)); got != 10 {
		t.Errorf("Recent(10) = %d lines", got)
	}
	filtered := e.Recent(50, "checkout", false)
	if len(filtered) != 1 {
		t.Fatalf("filter returned %d lines", len(filtered))
	}
	// Results come back oldest first.
	lines := e.Recent(5, "", false)
	for i := 1; i < len(lines); i++ {
		if lines[i].Seq < lines[i-1].Seq {
			t.Fatal("Recent is not in chronological order")
		}
	}
	if e.Total() != 51 {
		t.Errorf("Total = %d, want 51", e.Total())
	}
	first, last := e.Span()
	if !first.Equal(base) || !last.Equal(at) {
		t.Errorf("span = %v..%v", first, last)
	}
}

func TestSpanIgnoresOutOfOrderHeaderLines(t *testing.T) {
	e := New(DefaultConfig())
	now := time.Date(2026, 9, 16, 20, 0, 0, 0, time.UTC)
	// A header with no timestamp takes the arrival time, which is later than
	// every real line in the file.
	e.Feed("Filtering the log data using ...", now)
	e.Feed("2026-09-16T18:00:00Z INFO first", now)
	e.Feed("2026-09-16T18:10:00Z INFO last", now)
	first, last := e.Span()
	if last.Sub(first) <= 0 {
		t.Fatalf("span should be positive, got %v..%v", first, last)
	}
}

func TestDrainQueue(t *testing.T) {
	e := New(DefaultConfig())
	base := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 600; i++ {
		at := base.Add(time.Duration(i/20) * time.Second)
		e.Feed(fmt.Sprintf("%s INFO request path=/api/orders n=%d", at.Format(time.RFC3339), i), at)
	}
	e.Drain() // clear anything from warm-up
	at := base.Add(40 * time.Second)
	e.Feed(at.Format(time.RFC3339)+" ERROR connection pool exhausted active=64 max=64", at)
	got := e.Drain()
	if len(got) == 0 {
		t.Fatal("expected the novel template to be queued")
	}
	if len(e.Drain()) != 0 {
		t.Error("Drain should empty the queue")
	}
}

func TestTailFollowsRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src, err := TailSource(ctx, path, true)
	if err != nil {
		t.Fatal(err)
	}

	read := func() string {
		select {
		case ev := <-src:
			return ev.Text
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for a line")
			return ""
		}
	}
	if got := read(); got != "one" {
		t.Fatalf("first line = %q", got)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("two\n")
	f.Close()
	if got := read(); got != "two" {
		t.Fatalf("appended line = %q", got)
	}

	// Rotate: move the file aside and start a fresh one.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("three\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "three" {
		t.Fatalf("post-rotation line = %q", got)
	}
}

func TestReaderSourceStopsAtEOF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	os.WriteFile(path, []byte("a\nb\nc\n"), 0o644)
	f, _ := os.Open(path)
	defer f.Close()
	ctx := context.Background()
	n := 0
	for range ReaderSource(ctx, f) {
		n++
	}
	if n != 3 {
		t.Errorf("read %d lines, want 3", n)
	}
}

func TestForEachLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	os.WriteFile(path, []byte("a\nb\n"), 0o644)
	var got []string
	if err := ForEachLine(path, func(text string, _ time.Time) { got = append(got, text) }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[a b]" {
		t.Errorf("got %v", got)
	}
	if err := ForEachLine(filepath.Join(dir, "missing.log"), func(string, time.Time) {}); err == nil {
		t.Error("expected an error for a missing file")
	}
}

func TestBlankLinesAreIgnored(t *testing.T) {
	e := New(DefaultConfig())
	now := time.Now()
	e.Feed("   ", now)
	e.Feed("", now)
	if e.Total() != 0 {
		t.Errorf("blank lines counted: %d", e.Total())
	}
}
