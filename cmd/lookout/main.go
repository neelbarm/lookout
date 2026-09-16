// Command lookout watches a log stream and highlights the lines that do not
// look like the rest of the stream, with a reason.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/neelbarmecha/lookout/internal/detect"
	"github.com/neelbarmecha/lookout/internal/engine"
	"github.com/neelbarmecha/lookout/internal/tui"
)

const usage = `lookout - real-time log anomaly detection in the terminal

usage:
  tail -f app.log | lookout          watch a stream on stdin
  lookout tail <file>                follow a file, handling rotation
  lookout replay <file> --speed 50   replay a file using its own timestamps
  lookout report <file>              offline report: templates, anomalies, histogram

common flags:
  --warmup-lines N    lines before detectors arm (default 500)
  --warmup-secs N     seconds before detectors arm (default 10)
  --z F               z-score threshold for rate spikes (default 3.5)
  --burst-z F         z-score threshold for error bursts (default 4)
  --sim F             Drain similarity threshold, 0..1 (default 0.4)
  --depth N           Drain parse-tree depth (default 4)
  --silence N         seconds of silence before alerting (default 10)
  --max-new-per-min N novel alerts only when <=N new templates appeared in the
                      last minute of stream time (default 3)
  --param-sigma F     sigma multiplier for parameter outliers (default 4)
  --cooldown D        per-template hysteresis window (default 8s)
  --json              force JSON-lines output even on a terminal
  --no-color          disable colour
  --quiet             suppress the end-of-stream summary

tail flags:   --from-start        read the whole file before following
replay flags: --speed F           replay speed multiplier (default 1)
report flags: --width N, --top N, --max-anomalies N
`

type opts struct {
	warmLines  int
	warmSecs   float64
	z          float64
	burstZ     float64
	sim        float64
	depth      int
	silence    int
	maxNew     int
	paramSigma float64
	cooldown   time.Duration
	jsonOut    bool
	noColor    bool
	quiet      bool

	fromStart bool
	speed     float64
	width     int
	top       int
	maxAnom   int
}

func registerFlags(fs *flag.FlagSet, o *opts) {
	fs.IntVar(&o.warmLines, "warmup-lines", 500, "")
	fs.Float64Var(&o.warmSecs, "warmup-secs", 10, "")
	fs.Float64Var(&o.z, "z", 3.5, "")
	fs.Float64Var(&o.burstZ, "burst-z", 4, "")
	fs.Float64Var(&o.sim, "sim", 0.4, "")
	fs.IntVar(&o.depth, "depth", 4, "")
	fs.IntVar(&o.silence, "silence", 10, "")
	fs.IntVar(&o.maxNew, "max-new-per-min", 3, "")
	fs.Float64Var(&o.paramSigma, "param-sigma", 4, "")
	fs.DurationVar(&o.cooldown, "cooldown", 8*time.Second, "")
	fs.BoolVar(&o.jsonOut, "json", false, "")
	fs.BoolVar(&o.noColor, "no-color", false, "")
	fs.BoolVar(&o.quiet, "quiet", false, "")
	fs.BoolVar(&o.fromStart, "from-start", false, "")
	fs.Float64Var(&o.speed, "speed", 1, "")
	fs.IntVar(&o.width, "width", 0, "")
	fs.IntVar(&o.top, "top", 15, "")
	fs.IntVar(&o.maxAnom, "max-anomalies", 40, "")
}

func (o *opts) engineConfig() engine.Config {
	cfg := engine.DefaultConfig()
	cfg.Drain.SimThreshold = o.sim
	cfg.Drain.Depth = o.depth
	cfg.Detect.WarmupLines = o.warmLines
	cfg.Detect.WarmupDur = time.Duration(o.warmSecs * float64(time.Second))
	cfg.Detect.Z = o.z
	cfg.Detect.BurstZ = o.burstZ
	cfg.Detect.SilenceSecs = o.silence
	cfg.Detect.MaxNewPerMinute = o.maxNew
	cfg.Detect.ParamSigma = o.paramSigma
	cfg.Detect.Cooldown = o.cooldown
	return cfg
}

func main() {
	args := os.Args[1:]
	cmd := "stream"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return
	case "version", "--version":
		fmt.Println("lookout 1.0.0")
		return
	}

	fs := flag.NewFlagSet("lookout", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	var o opts
	registerFlags(fs, &o)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	rest := fs.Args()

	if o.noColor {
		os.Setenv("NO_COLOR", "1")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigc := make(chan os.Signal, 2)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM, syscall.SIGPIPE)
	go func() {
		<-sigc
		cancel()
	}()

	var err error
	switch cmd {
	case "stream":
		err = runStream(ctx, &o, engine.ReaderSource(ctx, os.Stdin), "stdin")
	case "tail":
		if len(rest) < 1 {
			fatal("lookout tail needs a file")
		}
		var src <-chan engine.Event
		src, err = engine.TailSource(ctx, rest[0], o.fromStart)
		if err == nil {
			err = runStream(ctx, &o, src, rest[0])
		}
	case "replay":
		if len(rest) < 1 {
			fatal("lookout replay needs a file")
		}
		var src <-chan engine.Event
		src, err = engine.ReplaySource(ctx, rest[0], o.speed)
		if err == nil {
			err = runStream(ctx, &o, src, fmt.Sprintf("%s (replay x%.0f)", rest[0], o.speed))
		}
	case "report":
		err = runReport(ctx, &o, rest)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fatal(err.Error())
	}
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "lookout: "+msg)
	os.Exit(1)
}

// runStream drives either the interactive dashboard or the JSON-lines mode.
func runStream(ctx context.Context, o *opts, src <-chan engine.Event, name string) error {
	eng := engine.New(o.engineConfig())
	interactive := tui.IsTTY(os.Stdout) && !o.jsonOut
	if interactive {
		app := tui.NewApp(eng, src, name)
		if err := app.Run(ctx); err == nil {
			return nil
		}
		// No controlling terminal: fall through to the piped mode.
	}
	return runJSON(ctx, o, eng, src)
}

// runJSON writes one JSON object per anomaly to stdout and a summary to stderr.
func runJSON(ctx context.Context, o *opts, eng *engine.Engine, src <-chan engine.Event) error {
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	emit := func(as []detect.Anomaly) {
		for _, a := range as {
			enc.Encode(a)
		}
		if len(as) > 0 {
			out.Flush()
		}
	}

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case ev, ok := <-src:
			if !ok {
				break loop
			}
			eng.Feed(ev.Text, ev.At)
			emit(eng.Drain())
		case <-tick.C:
			now := time.Now()
			_, last := eng.Span()
			if !last.IsZero() {
				if d := now.Sub(last); d < 0 || d > 5*time.Second {
					continue // historical stream: its own timestamps drive the clock
				}
			}
			eng.Tick(now)
			emit(eng.Drain())
		}
	}

	// Close the final partial second so the last bucket is evaluated.
	if _, last := eng.Span(); !last.IsZero() {
		eng.Tick(last.Truncate(time.Second).Add(time.Second))
		emit(eng.Drain())
	}
	out.Flush()
	if !o.quiet {
		tui.Summary(os.Stderr, eng, tui.IsTTY(os.Stderr))
	}
	return nil
}

func runReport(ctx context.Context, o *opts, rest []string) error {
	eng := engine.New(o.engineConfig())
	src := "stdin"
	if len(rest) == 0 || rest[0] == "-" {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		now := time.Now()
		for sc.Scan() {
			eng.Feed(sc.Text(), now)
		}
		if err := sc.Err(); err != nil {
			return err
		}
	} else {
		src = rest[0]
		if err := engine.ForEachLine(rest[0], func(text string, at time.Time) {
			eng.Feed(text, at)
		}); err != nil {
			return err
		}
	}
	if _, last := eng.Span(); !last.IsZero() {
		eng.Tick(last.Truncate(time.Second).Add(time.Second))
	}
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	tui.Report(w, eng, tui.ReportOptions{
		Width: o.width, Color: tui.IsTTY(os.Stdout), TopN: o.top,
		MaxAnomaly: o.maxAnom, Source: src,
	})
	return nil
}
