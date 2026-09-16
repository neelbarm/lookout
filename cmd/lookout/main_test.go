package main

import (
	"flag"
	"strings"
	"testing"
	"time"
)

func parse(t *testing.T, args ...string) (*opts, []string) {
	t.Helper()
	fs := flag.NewFlagSet("lookout", flag.ContinueOnError)
	fs.SetOutput(nopWriter{})
	var o opts
	registerFlags(fs, &o)
	rest, err := parseArgs(fs, args)
	if err != nil {
		t.Fatalf("parseArgs(%v): %v", args, err)
	}
	return &o, rest
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// Go's flag package stops at the first non-flag argument, so without
// parseArgs "lookout replay app.log --speed 50" would silently replay at 1x.
func TestFlagsAfterOperandsAreParsed(t *testing.T) {
	o, rest := parse(t, "app.log", "--speed", "50", "--json")
	if len(rest) != 1 || rest[0] != "app.log" {
		t.Fatalf("operands = %v", rest)
	}
	if o.speed != 50 {
		t.Errorf("speed = %v, want 50", o.speed)
	}
	if !o.jsonOut {
		t.Error("--json after the operand was ignored")
	}
}

func TestFlagsBeforeOperandsStillWork(t *testing.T) {
	o, rest := parse(t, "--speed", "7", "--no-color", "app.log")
	if len(rest) != 1 || rest[0] != "app.log" {
		t.Fatalf("operands = %v", rest)
	}
	if o.speed != 7 || !o.noColor {
		t.Errorf("opts = %+v", o)
	}
}

func TestMixedAndRepeatedOperands(t *testing.T) {
	o, rest := parse(t, "--top", "3", "a.log", "--max-anomalies=2", "b.log", "--quiet")
	if strings.Join(rest, ",") != "a.log,b.log" {
		t.Fatalf("operands = %v", rest)
	}
	if o.top != 3 || o.maxAnom != 2 || !o.quiet {
		t.Errorf("opts = %+v", o)
	}
}

func TestEngineConfigMapsEveryFlag(t *testing.T) {
	o, _ := parse(t,
		"x.log",
		"--warmup-lines", "111", "--warmup-secs", "2.5",
		"--z", "9", "--burst-z", "8", "--sim", "0.6", "--depth", "5",
		"--silence", "42", "--param-sigma", "6", "--max-new-per-min", "1",
		"--cooldown", "30s")
	cfg := o.engineConfig()
	if cfg.Detect.WarmupLines != 111 {
		t.Errorf("WarmupLines = %d", cfg.Detect.WarmupLines)
	}
	if cfg.Detect.WarmupDur != 2500*time.Millisecond {
		t.Errorf("WarmupDur = %v", cfg.Detect.WarmupDur)
	}
	if cfg.Detect.Z != 9 || cfg.Detect.BurstZ != 8 {
		t.Errorf("z thresholds = %v %v", cfg.Detect.Z, cfg.Detect.BurstZ)
	}
	if cfg.Drain.SimThreshold != 0.6 || cfg.Drain.Depth != 5 {
		t.Errorf("drain = %+v", cfg.Drain)
	}
	if cfg.Detect.SilenceSecs != 42 || cfg.Detect.ParamSigma != 6 {
		t.Errorf("detect = %+v", cfg.Detect)
	}
	if cfg.Detect.MaxNewPerMinute != 1 || cfg.Detect.Cooldown != 30*time.Second {
		t.Errorf("detect = %+v", cfg.Detect)
	}
}

func TestDefaultsAreTheDocumentedOnes(t *testing.T) {
	o, rest := parse(t)
	if len(rest) != 0 {
		t.Fatalf("operands = %v", rest)
	}
	cfg := o.engineConfig()
	if cfg.Detect.WarmupLines != 500 || cfg.Detect.WarmupDur != 10*time.Second {
		t.Errorf("warm-up defaults = %d/%v", cfg.Detect.WarmupLines, cfg.Detect.WarmupDur)
	}
	if cfg.Detect.Z != 3.5 || cfg.Drain.SimThreshold != 0.4 || cfg.Detect.Cooldown != 8*time.Second {
		t.Errorf("defaults drifted from the README: %+v %+v", cfg.Detect, cfg.Drain)
	}
}
