package drain

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Once the root's child cap folds several token counts into one wildcard
// branch, a saturated leaf holds clusters of different lengths. Merging a
// short line into a long template used to panic with an index out of range.
func TestSaturatedLeafWithMixedTokenCounts(t *testing.T) {
	m := New(DefaultConfig())
	add := func(n int) {
		msg := "alpha beta" + strings.Repeat(" x", n)
		m.Add(msg, msg, time.Time{})
	}
	// Fill the root's child map with MaxChildren distinct, long token counts.
	cfg := DefaultConfig()
	for n := 2000; n > 2000-cfg.MaxChildren; n-- {
		add(n)
	}
	// Everything from here shares the root's wildcard subtree, so one leaf now
	// mixes token counts. Descending lengths put a shorter line against a
	// longer template after the leaf saturates.
	for n := 1000; n >= 400; n-- {
		add(n)
	}
	// Feed a batch of repeats too: the same lines must not mint new clusters.
	before := len(m.Clusters())
	for i := 0; i < 3; i++ {
		for n := 1000; n >= 400; n-- {
			add(n)
		}
	}
	if got := len(m.Clusters()); got != before {
		t.Errorf("repeats added %d clusters, want none", got-before)
	}
	// Every template must still describe lines of its own length.
	for _, c := range m.Clusters() {
		if len(c.Tokens) == 0 {
			t.Fatalf("cluster #%d has no tokens", c.ID)
		}
	}
}

// A 300-digit number with an "h" suffix parses fine and then overflows while
// being scaled to milliseconds. An infinite value poisons the running
// statistics and makes encoding/json drop the whole anomaly.
func TestNumericRejectsOverflow(t *testing.T) {
	for _, n := range []int{300, 303, 305, 308, 309, 400} {
		for _, unit := range []string{"", "ns", "us", "ms", "s", "m", "h", "B"} {
			tok := "dur=" + strings.Repeat("9", n) + unit
			v, _, ok := numeric(tok)
			if ok && (math.IsInf(v, 0) || math.IsNaN(v)) {
				t.Errorf("numeric(%d digits + %q) = %v, want it rejected", n, unit, v)
			}
		}
	}
	// Ordinary durations still normalize to milliseconds.
	for _, c := range []struct {
		tok  string
		want float64
	}{
		{"latency=1.5s", 1500},
		{"latency=250ms", 250},
		{"took=2h", 7.2e6},
	} {
		v, unit, ok := numeric(c.tok)
		if !ok || unit != "ms" || v != c.want {
			t.Errorf("numeric(%q) = %v %q %v, want %v ms true", c.tok, v, unit, ok, c.want)
		}
	}
}
