package detect

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/neelbarmecha/lookout/internal/drain"
	"github.com/neelbarmecha/lookout/internal/parse"
)

// harness feeds synthetic streams through the real miner and detector.
type harness struct {
	m   *drain.Miner
	d   *Detector
	got []Anomaly
}

func newHarness(cfg Config) *harness {
	return &harness{m: drain.New(drain.DefaultConfig()), d: New(cfg)}
}

func (h *harness) feed(line string, at time.Time) {
	rec := parse.Parse(line, at)
	rec.Time = at
	cl, isNew, params := h.m.Add(rec.Message, rec.Raw, at)
	h.got = append(h.got, Filter(h.d.Observe(rec, cl, isNew, params))...)
}

func (h *harness) tick(at time.Time) {
	h.got = append(h.got, Filter(h.d.Tick(at))...)
}

func (h *harness) count(k Kind) int {
	n := 0
	for _, a := range h.got {
		if a.Kind == k {
			n++
		}
	}
	return n
}

func (h *harness) after(t0 time.Time, k Kind) int {
	n := 0
	for _, a := range h.got {
		if a.Kind == k && !a.Time.Before(t0) {
			n++
		}
	}
	return n
}

func testConfig() Config {
	c := DefaultConfig()
	c.WarmupLines = 400
	c.WarmupDur = 5 * time.Second
	return c
}

var base = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

// steady emits a boring, stationary service log: three templates at fixed
// rates with log-normal latencies and a low background error rate.
func steady(h *harness, rng *rand.Rand, from, to int) {
	for s := from; s < to; s++ {
		at := base.Add(time.Duration(s) * time.Second)
		for i := 0; i < 20; i++ {
			lat := math.Exp(math.Log(40) + 0.5*rng.NormFloat64())
			lvl := "INFO"
			if rng.Float64() < 0.02 {
				lvl = "WARN"
			}
			h.feed(fmt.Sprintf("%s %s request method=GET path=/api/orders status=200 latency=%.0fms user=u%d",
				at.Add(time.Duration(i)*50*time.Millisecond).Format(time.RFC3339), lvl, lat, 1000+rng.Intn(9000)),
				at.Add(time.Duration(i)*50*time.Millisecond))
		}
		for i := 0; i < 4; i++ {
			h.feed(fmt.Sprintf("%s DEBUG cache lookup key=session:%d hit=true size=%dB",
				at.Format(time.RFC3339), 1000+rng.Intn(9000), 100+rng.Intn(800)), at)
		}
		for i := 0; i < 3; i++ {
			h.feed(fmt.Sprintf("%s INFO auth token refreshed user=u%d ttl=3600s",
				at.Format(time.RFC3339), 1000+rng.Intn(9000)), at)
		}
	}
}

func TestSteadyStreamIsQuiet(t *testing.T) {
	h := newHarness(testConfig())
	rng := rand.New(rand.NewSource(1))
	steady(h, rng, 0, 180)
	h.tick(base.Add(181 * time.Second))

	lines := 180 * 27
	if got := len(h.got); got > lines/100 {
		for _, a := range h.got {
			t.Logf("  %s %s: %s", a.Time.Format("15:04:05"), a.Kind, a.Reason)
		}
		t.Fatalf("false positive rate %d/%d exceeds 1%%", got, lines)
	}
	t.Logf("steady stream: %d anomalies in %d lines (%.3f%%)",
		len(h.got), lines, 100*float64(len(h.got))/float64(lines))
}

func TestNovelTemplateIsFlagged(t *testing.T) {
	h := newHarness(testConfig())
	rng := rand.New(rand.NewSource(2))
	steady(h, rng, 0, 60)

	mark := base.Add(60 * time.Second)
	for i := 0; i < 5; i++ {
		at := mark.Add(time.Duration(i) * 100 * time.Millisecond)
		h.feed(at.Format(time.RFC3339)+" ERROR connection pool exhausted active=64 max=64 waiters=37 pool=pg-primary", at)
	}
	steady(h, rng, 61, 70)

	if n := h.after(mark, KindNovel); n != 1 {
		for _, a := range h.got {
			t.Logf("  %s %s: %s", a.Time.Format("15:04:05"), a.Kind, a.Reason)
		}
		t.Fatalf("expected exactly 1 novel-template anomaly, got %d", n)
	}
}

func TestNovelTemplateNotFlaggedDuringWarmup(t *testing.T) {
	h := newHarness(testConfig())
	rng := rand.New(rand.NewSource(3))
	// A brand new template inside the warm-up window is part of the baseline.
	at := base.Add(time.Second)
	h.feed(at.Format(time.RFC3339)+" ERROR connection pool exhausted active=64 max=64 waiters=37 pool=pg-primary", at)
	steady(h, rng, 0, 10)
	if n := h.count(KindNovel); n != 0 {
		t.Fatalf("novel fired during warm-up: %d", n)
	}
}

func TestRateSpikeIsFlagged(t *testing.T) {
	h := newHarness(testConfig())
	rng := rand.New(rand.NewSource(4))
	steady(h, rng, 0, 60)

	mark := base.Add(60 * time.Second)
	// The auth template jumps from 3/s to 30/s for four seconds.
	for s := 60; s < 64; s++ {
		at := base.Add(time.Duration(s) * time.Second)
		for i := 0; i < 30; i++ {
			h.feed(fmt.Sprintf("%s INFO auth token refreshed user=u%d ttl=3600s",
				at.Format(time.RFC3339), 1000+rng.Intn(9000)), at)
		}
	}
	steady(h, rng, 64, 70)

	if n := h.after(mark, KindSpike); n < 1 {
		for _, a := range h.got {
			t.Logf("  %s %s: %s", a.Time.Format("15:04:05"), a.Kind, a.Reason)
		}
		t.Fatal("rate spike not detected")
	}
}

func TestParameterOutlierIsFlagged(t *testing.T) {
	h := newHarness(testConfig())
	rng := rand.New(rand.NewSource(5))
	steady(h, rng, 0, 60)

	mark := base.Add(60 * time.Second)
	at := mark
	h.feed(at.Format(time.RFC3339)+" INFO request method=GET path=/api/orders status=200 latency=3120ms user=u4242", at)
	steady(h, rng, 61, 65)

	if n := h.after(mark, KindParam); n < 1 {
		t.Fatal("parameter outlier not detected")
	}
	for _, a := range h.got {
		if a.Kind == KindParam && !a.Time.Before(mark) {
			if a.Param != "latency" {
				t.Errorf("flagged parameter %q, want latency", a.Param)
			}
			if a.Value != 3120 {
				t.Errorf("flagged value %v, want 3120", a.Value)
			}
			return
		}
	}
}

func TestIdentifierParametersAreNotScored(t *testing.T) {
	h := newHarness(testConfig())
	rng := rand.New(rand.NewSource(6))
	// req_id is hex: sometimes all digits, sometimes not. It must never be
	// treated as a measurement.
	for s := 0; s < 90; s++ {
		at := base.Add(time.Duration(s) * time.Second)
		for i := 0; i < 20; i++ {
			id := fmt.Sprintf("%08x", rng.Uint32())
			h.feed(fmt.Sprintf("%s INFO request id=%s status=200", at.Format(time.RFC3339), id), at)
		}
	}
	at := base.Add(91 * time.Second)
	h.feed(at.Format(time.RFC3339)+" INFO request id=99999999 status=200", at)
	for _, a := range h.got {
		if a.Kind == KindParam && a.Param == "id" {
			t.Fatalf("identifier scored as a measurement: %s", a.Reason)
		}
	}
}

func TestErrorBurstIsFlagged(t *testing.T) {
	h := newHarness(testConfig())
	rng := rand.New(rand.NewSource(7))
	steady(h, rng, 0, 60)

	mark := base.Add(60 * time.Second)
	for s := 60; s < 66; s++ {
		at := base.Add(time.Duration(s) * time.Second)
		for i := 0; i < 15; i++ {
			h.feed(fmt.Sprintf("%s ERROR request method=GET path=/api/orders status=500 latency=%dms user=u%d",
				at.Format(time.RFC3339), 40+rng.Intn(30), 1000+rng.Intn(9000)), at)
		}
	}
	h.tick(base.Add(67 * time.Second))

	if n := h.after(mark, KindBurst); n < 1 {
		for _, a := range h.got {
			t.Logf("  %s %s: %s", a.Time.Format("15:04:05"), a.Kind, a.Reason)
		}
		t.Fatal("error burst not detected")
	}
}

func TestSilenceIsFlagged(t *testing.T) {
	cfg := testConfig()
	cfg.SilenceSecs = 8
	cfg.MinActiveSecs = 20
	h := newHarness(cfg)
	rng := rand.New(rand.NewSource(8))
	steady(h, rng, 0, 60)

	mark := base.Add(60 * time.Second)
	// The cache template stops while everything else keeps flowing.
	for s := 60; s < 80; s++ {
		at := base.Add(time.Duration(s) * time.Second)
		for i := 0; i < 20; i++ {
			h.feed(fmt.Sprintf("%s INFO request method=GET path=/api/orders status=200 latency=41ms user=u%d",
				at.Format(time.RFC3339), 1000+rng.Intn(9000)), at)
		}
	}
	if n := h.after(mark, KindSilence); n < 1 {
		for _, a := range h.got {
			t.Logf("  %s %s: %s", a.Time.Format("15:04:05"), a.Kind, a.Reason)
		}
		t.Fatal("silence not detected")
	}
}

func TestSilenceNotFlaggedWhenEverythingStops(t *testing.T) {
	cfg := testConfig()
	cfg.SilenceSecs = 8
	cfg.MinActiveSecs = 20
	h := newHarness(cfg)
	rng := rand.New(rand.NewSource(9))
	steady(h, rng, 0, 60)
	// The whole stream goes quiet: that is not any one template's story.
	h.tick(base.Add(120 * time.Second))
	if n := h.after(base.Add(60*time.Second), KindSilence); n != 0 {
		t.Fatalf("silence fired for a whole-stream stall: %d", n)
	}
}

func TestHysteresisSuppressesRepeats(t *testing.T) {
	cfg := testConfig()
	cfg.Cooldown = 30 * time.Second
	h := newHarness(cfg)
	rng := rand.New(rand.NewSource(10))
	steady(h, rng, 0, 60)

	mark := base.Add(60 * time.Second)
	// Ten consecutive outliers on the same template within the cooldown.
	for i := 0; i < 10; i++ {
		at := mark.Add(time.Duration(i) * 200 * time.Millisecond)
		h.feed(fmt.Sprintf("%s INFO request method=GET path=/api/orders status=200 latency=%dms user=u1",
			at.Format(time.RFC3339), 3000+i), at)
	}
	if n := h.after(mark, KindParam); n != 1 {
		t.Fatalf("hysteresis failed: %d parameter anomalies inside one cooldown", n)
	}
}

func TestWelfordAndQuantiles(t *testing.T) {
	p := &pstat{rng: rand.New(rand.NewSource(11))}
	for i := 1; i <= 1000; i++ {
		p.push(float64(i))
	}
	if math.Abs(p.mean-500.5) > 1e-6 {
		t.Errorf("mean = %v, want 500.5", p.mean)
	}
	want := math.Sqrt(1000 * 1001 / 12.0) // sd of 1..1000
	if math.Abs(p.sd()-want) > 1 {
		t.Errorf("sd = %v, want about %v", p.sd(), want)
	}
	if q := p.quantile(0.5); q < 300 || q > 700 {
		t.Errorf("median estimate = %v, want near 500", q)
	}
	if n := p.distinct(); n < 100 {
		t.Errorf("distinct = %d, want a large reservoir spread", n)
	}
}

func TestEMAWinsorization(t *testing.T) {
	mean, varr := 5.0, 1.0
	for i := 0; i < 50; i++ {
		emaUpdate(&mean, &varr, 5, 0.2)
	}
	before := mean
	emaUpdate(&mean, &varr, 500, 0.2) // a wild outlier
	if mean > before+3 {
		t.Errorf("one outlier moved the baseline from %v to %v", before, mean)
	}
	if varr > 25 {
		t.Errorf("one outlier inflated the variance to %v", varr)
	}
}

func TestSnapshotOrdering(t *testing.T) {
	h := newHarness(testConfig())
	rng := rand.New(rand.NewSource(12))
	steady(h, rng, 0, 40)
	snap := h.d.Snapshot(0)
	if len(snap) < 3 {
		t.Fatalf("expected at least 3 templates, got %d", len(snap))
	}
	for i := 1; i < len(snap); i++ {
		if snap[i-1].PerSec < snap[i].PerSec {
			t.Fatal("snapshot is not ordered by rate")
		}
	}
	for _, s := range snap {
		if len(s.Hist) != histLen {
			t.Fatalf("history length = %d, want %d", len(s.Hist), histLen)
		}
	}
}
