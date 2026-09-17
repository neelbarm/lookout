package main

import (
	"math"
	"strings"
	"testing"

	"github.com/neelbarmecha/lookout/internal/detect"
)

// "lookout --version" and "lookout --help" used to reach the flag parser and
// exit 2 with "flag provided but not defined".
func TestEarlyExitHandlesDashedSpellings(t *testing.T) {
	for _, a := range []string{"version", "-version", "--version"} {
		got, ok := earlyExit([]string{a})
		if !ok || !strings.HasPrefix(got, "lookout 1.0.0") {
			t.Errorf("earlyExit(%q) = %q, %v; want the version", a, got, ok)
		}
	}
	for _, a := range []string{"help", "-h", "-help", "--help"} {
		got, ok := earlyExit([]string{a})
		if !ok || !strings.Contains(got, "usage:") {
			t.Errorf("earlyExit(%q) = %q, %v; want the usage", a, got, ok)
		}
	}
	for _, a := range [][]string{nil, {}, {"report"}, {"--json"}, {"tail", "app.log"}} {
		if got, ok := earlyExit(a); ok {
			t.Errorf("earlyExit(%v) = %q, true; want no early exit", a, got)
		}
	}
}

// encoding/json refuses Inf and NaN and writes nothing when it does, so an
// unrepresentable score would drop the whole finding instead of degrading it.
func TestJSONSafeReplacesNonFiniteValues(t *testing.T) {
	for _, v := range []float64{math.Inf(1), math.Inf(-1), math.NaN()} {
		got := jsonSafe(detect.Anomaly{Kind: detect.KindParam, Score: v, Value: v})
		if got.Score != 0 || got.Value != 0 {
			t.Errorf("jsonSafe(%v) left score=%v value=%v", v, got.Score, got.Value)
		}
	}
	in := detect.Anomaly{Kind: detect.KindParam, Score: 17.5, Value: 1094}
	if got := jsonSafe(in); got != in {
		t.Errorf("jsonSafe changed a finite anomaly: %+v", got)
	}
}
