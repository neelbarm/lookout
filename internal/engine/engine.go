// Package engine wires the parser, the template miner and the detectors into
// one stream processor, and keeps the bounded history the UI renders from.
package engine

import (
	"strings"
	"time"

	"github.com/neelbarmecha/lookout/internal/detect"
	"github.com/neelbarmecha/lookout/internal/drain"
	"github.com/neelbarmecha/lookout/internal/parse"
)

// Line is one processed log line plus what the detectors said about it.
type Line struct {
	Seq int
	Raw string
	// Msg is the line with its timestamp and level prefix stripped: what the
	// UI shows, since the timestamp already has its own column.
	Msg       string
	Level     string
	At        time.Time // stream time
	Wall      time.Time // wall-clock arrival, used for flash animation
	ClusterID int
	Novel     bool
	Anoms     []detect.Anomaly
}

// Top returns the line's most severe anomaly, which is the one worth showing
// as its tag.
func (l Line) Top() (detect.Anomaly, bool) {
	if len(l.Anoms) == 0 {
		return detect.Anomaly{}, false
	}
	best := 0
	for i, a := range l.Anoms {
		if a.Severity > l.Anoms[best].Severity {
			best = i
		}
		_ = i
	}
	return l.Anoms[best], true
}

// Severity returns the highest anomaly severity attached to the line.
func (l Line) Severity() int {
	s := 0
	for _, a := range l.Anoms {
		if a.Severity > s {
			s = a.Severity
		}
	}
	return s
}

// Tags returns the display tags of the line's anomalies.
func (l Line) Tags() []string {
	out := make([]string, 0, len(l.Anoms))
	for _, a := range l.Anoms {
		out = append(out, a.Tag)
	}
	return out
}

// Config bundles the miner and detector configuration.
type Config struct {
	Drain  drain.Config
	Detect detect.Config
	// MaxRecent bounds the in-memory line history.
	MaxRecent int
	// MaxAnomalies bounds the retained anomaly list.
	MaxAnomalies int
}

// DefaultConfig returns the CLI defaults.
func DefaultConfig() Config {
	return Config{
		Drain:        drain.DefaultConfig(),
		Detect:       detect.DefaultConfig(),
		MaxRecent:    2000,
		MaxAnomalies: 500,
	}
}

// Engine is the stream processor. It is not safe for concurrent use.
type Engine struct {
	cfg     Config
	Miner   *drain.Miner
	Det     *detect.Detector
	seq     int
	recent  ring[Line]
	anoms   ring[detect.Anomaly]
	pending []detect.Anomaly
	total   int
	byKind  map[detect.Kind]int
	first   time.Time
	last    time.Time

	// timeline holds per-second line counts for the whole run so that the
	// offline report can draw a histogram without retaining every line.
	timeline map[int64]int
}

// New builds an Engine.
func New(cfg Config) *Engine {
	if cfg.MaxRecent <= 0 {
		cfg.MaxRecent = 2000
	}
	if cfg.MaxAnomalies <= 0 {
		cfg.MaxAnomalies = 500
	}
	return &Engine{
		cfg:      cfg,
		Miner:    drain.New(cfg.Drain),
		Det:      detect.New(cfg.Detect),
		recent:   newRing[Line](cfg.MaxRecent),
		anoms:    newRing[detect.Anomaly](cfg.MaxAnomalies),
		byKind:   map[detect.Kind]int{},
		timeline: map[int64]int{},
	}
}

// Feed processes one raw line. arrival is used when the line has no timestamp.
func (e *Engine) Feed(raw string, arrival time.Time) Line {
	rec := parse.Parse(raw, arrival)
	if strings.TrimSpace(rec.Raw) == "" {
		return Line{}
	}
	cl, isNew, params := e.Miner.Add(rec.Message, rec.Raw, rec.Time)
	anoms := detect.Filter(e.Det.Observe(rec, cl, isNew, params))

	e.seq++
	e.total++
	// Track the extremes, not the first and last lines: a file can start with
	// a header line that carries no timestamp, or arrive slightly out of order.
	if e.first.IsZero() || rec.Time.Before(e.first) {
		e.first = rec.Time
	}
	if rec.Time.After(e.last) {
		e.last = rec.Time
	}
	e.timeline[rec.Time.Unix()]++

	ln := Line{
		Seq: e.seq, Raw: rec.Raw, Msg: rec.Message, Level: rec.Level, At: rec.Time, Wall: arrival,
		ClusterID: cl.ID, Novel: isNew, Anoms: anoms,
	}
	e.push(ln)
	e.record(anoms)
	return ln
}

// Tick advances the detector clock without new input.
func (e *Engine) Tick(now time.Time) []detect.Anomaly {
	a := detect.Filter(e.Det.Tick(now))
	e.record(a)
	return a
}

func (e *Engine) record(as []detect.Anomaly) {
	for _, a := range as {
		e.byKind[a.Kind]++
		e.pending = append(e.pending, a)
		e.anoms.push(a)
	}
}

// push appends to the fixed-size ring of recent lines. A ring matters: a
// re-slicing buffer reallocates on every single line once it is full, which at
// a few thousand lines a second is most of the program's allocation.
func (e *Engine) push(l Line) { e.recent.push(l) }

// Recent returns up to n recent lines, oldest first, applying the UI filters.
func (e *Engine) Recent(n int, filter string, anomOnly bool) []Line {
	out := make([]Line, 0, n)
	lf := strings.ToLower(filter)
	e.recent.eachBackward(func(l Line) bool {
		if len(out) >= n {
			return false
		}
		if anomOnly && len(l.Anoms) == 0 {
			return true
		}
		if lf != "" && !strings.Contains(strings.ToLower(l.Raw), lf) {
			return true
		}
		out = append(out, l)
		return true
	})
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Drain returns every anomaly recorded since the last call and clears the
// queue. It is how the JSON-lines mode streams findings in order.
func (e *Engine) Drain() []detect.Anomaly {
	if len(e.pending) == 0 {
		return nil
	}
	out := e.pending
	e.pending = nil
	return out
}

// Anomalies returns the retained anomalies, oldest first.
func (e *Engine) Anomalies() []detect.Anomaly {
	out := make([]detect.Anomaly, 0, e.anoms.len())
	e.anoms.eachForward(func(a detect.Anomaly) bool {
		out = append(out, a)
		return true
	})
	return out
}

// LatestAnomalies returns up to n anomalies, newest first.
func (e *Engine) LatestAnomalies(n int) []detect.Anomaly {
	out := make([]detect.Anomaly, 0, n)
	e.anoms.eachBackward(func(a detect.Anomaly) bool {
		if len(out) >= n {
			return false
		}
		out = append(out, a)
		return true
	})
	return out
}

// ByKind returns anomaly counts per kind.
func (e *Engine) ByKind() map[detect.Kind]int { return e.byKind }

// Total returns the number of lines processed.
func (e *Engine) Total() int { return e.total }

// Timeline returns per-second line counts keyed by unix second.
func (e *Engine) Timeline() map[int64]int { return e.timeline }

// Span returns the first and last stream timestamps seen.
func (e *Engine) Span() (time.Time, time.Time) { return e.first, e.last }
