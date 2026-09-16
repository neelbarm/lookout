// Package detect holds the five online anomaly detectors that run on top of
// the mined templates. Everything is streaming: EMA mean/variance for rates,
// Welford accumulators plus a bounded reservoir for parameter distributions,
// and per-(template, kind) hysteresis so one incident does not produce a
// thousand alerts.
package detect

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/neelbarmecha/lookout/internal/drain"
	"github.com/neelbarmecha/lookout/internal/parse"
)

// Kind identifies which detector fired.
type Kind string

const (
	KindNovel   Kind = "novel"
	KindSpike   Kind = "spike"
	KindBurst   Kind = "burst"
	KindParam   Kind = "param"
	KindSilence Kind = "silence"
)

// Anomaly is one finding. It is also the JSON schema emitted in piped mode.
type Anomaly struct {
	Time       time.Time `json:"time"`
	Kind       Kind      `json:"kind"`
	Severity   int       `json:"severity"`
	Tag        string    `json:"tag"`
	TemplateID int       `json:"template_id"`
	Template   string    `json:"template"`
	Line       string    `json:"line"`
	Reason     string    `json:"reason"`
	Score      float64   `json:"score,omitempty"`
	Param      string    `json:"param,omitempty"`
	Value      float64   `json:"value,omitempty"`
}

// Config tunes the detectors.
type Config struct {
	WarmupLines     int
	WarmupDur       time.Duration
	Z               float64       // z-score threshold for rate spikes
	BurstZ          float64       // z-score threshold for error bursts
	SilenceSecs     int           // seconds of zero traffic before a silence alert
	MinSeconds      int           // seconds of history before rate stats are trusted
	MinParamSamples int           // samples before a parameter distribution is trusted
	MaxNewPerMinute int           // novelty is only reported once the vocabulary has settled
	MinActiveSecs   int           // seconds a template must have been active to earn a silence alert
	ParamSigma      float64       // sigma multiplier for parameter outliers
	ParamP99        float64       // p99 multiplier for parameter outliers
	Cooldown        time.Duration // per-template, per-kind hysteresis
	Alpha           float64       // EMA smoothing factor
}

// DefaultConfig returns the defaults used by the CLI.
func DefaultConfig() Config {
	return Config{
		WarmupLines:     500,
		WarmupDur:       10 * time.Second,
		Z:               3.5,
		BurstZ:          4.0,
		SilenceSecs:     10,
		MinSeconds:      15,
		MinParamSamples: 50,
		MaxNewPerMinute: 3,
		MinActiveSecs:   30,
		ParamSigma:      4.0,
		ParamP99:        1.5,
		Cooldown:        8 * time.Second,
		Alpha:           0.12,
	}
}

const histLen = 60

func mod(a int64, n int) int {
	m := a % int64(n)
	if m < 0 {
		m += int64(n)
	}
	return int(m)
}

// emaUpdate folds one observation into an EMA mean and variance, winsorizing
// the sample to +/-3 sigma first. Without that clamp the very burst we want to
// report inflates the variance in the same second and hides itself.
func emaUpdate(mean, varr *float64, x, alpha float64) {
	sd := math.Sqrt(*varr)
	xw := x
	if sd > 0 {
		if hi := *mean + 3*sd; xw > hi {
			xw = hi
		} else if lo := *mean - 3*sd; xw < lo {
			xw = lo
		}
	}
	prev := *mean
	*mean = alpha*xw + (1-alpha)**mean
	*varr = alpha*(xw-prev)*(xw-prev) + (1-alpha)**varr
}

type pstat struct {
	name   string
	unit   string
	n      int
	nonNum int
	mean   float64
	m2     float64
	// Log-space Welford. Latencies, payload sizes and queue depths are
	// right-skewed, so a mean+k*sigma rule in linear space fires on the
	// ordinary tail. Scoring in log space fixes that.
	logN    int
	logMean float64
	logM2   float64
	allPos  bool
	res     []float64
	sorted  []float64
	dirty   bool
	rng     *rand.Rand
}

func (p *pstat) push(v float64) {
	if p.n == 0 {
		p.allPos = true
	}
	p.n++
	d := v - p.mean
	p.mean += d / float64(p.n)
	p.m2 += d * (v - p.mean)
	if v > 0 {
		lv := math.Log(v)
		p.logN++
		ld := lv - p.logMean
		p.logMean += ld / float64(p.logN)
		p.logM2 += ld * (lv - p.logMean)
	} else {
		p.allPos = false
	}
	const cap = 512
	if len(p.res) < cap {
		p.res = append(p.res, v)
	} else if j := p.rng.Intn(p.n); j < cap {
		p.res[j] = v
	}
	p.dirty = true
}

func (p *pstat) sd() float64 {
	if p.n < 2 {
		return 0
	}
	return math.Sqrt(p.m2 / float64(p.n-1))
}

func (p *pstat) logSD() float64 {
	if p.logN < 2 {
		return 0
	}
	return math.Sqrt(p.logM2 / float64(p.logN-1))
}

// zscore returns how extreme v is, in log space for strictly positive
// right-skewed measurements and in linear space otherwise.
func (p *pstat) zscore(v float64) (float64, bool) {
	if p.allPos && v > 0 && p.logN >= 2 {
		sd := p.logSD()
		if sd > 1e-9 {
			return (math.Log(v) - p.logMean) / sd, true
		}
	}
	sd := p.sd()
	if sd <= 1e-9 {
		return 0, false
	}
	return (v - p.mean) / sd, false
}

// distinct counts the distinct values held in the reservoir. A position with
// only a handful of distinct values (an HTTP status, a boolean, an enum) is not
// a measurement and must not be treated as one.
func (p *pstat) distinct() int {
	p.sortRes()
	n := 0
	for i, v := range p.sorted {
		if i == 0 || v != p.sorted[i-1] {
			n++
		}
	}
	return n
}

func (p *pstat) sortRes() {
	if p.dirty {
		p.sorted = append(p.sorted[:0], p.res...)
		sort.Float64s(p.sorted)
		p.dirty = false
	}
}

func (p *pstat) quantile(q float64) float64 {
	if len(p.res) == 0 {
		return 0
	}
	if p.dirty {
		p.sorted = append(p.sorted[:0], p.res...)
		sort.Float64s(p.sorted)
		p.dirty = false
	}
	i := int(q * float64(len(p.sorted)-1))
	if i < 0 {
		i = 0
	}
	if i >= len(p.sorted) {
		i = len(p.sorted) - 1
	}
	return p.sorted[i]
}

type tstate struct {
	cl         *drain.Cluster
	cur        int
	hist       [histLen]float64
	histSec    int64
	mean       float64
	varr       float64
	secs       int
	lastLine   string
	lastSec    int64
	silenced   bool
	activeSecs int
	anomalies  int
	lastAlert  map[Kind]time.Time
	params     map[int]*pstat
	peakMean   float64
}

// TemplateStat is a snapshot row for the UI and the report.
type TemplateStat struct {
	ID        int
	Template  string
	Example   string
	Count     int
	PerSec    float64
	Hist      []float64
	Anomalies int
	First     time.Time
	Last      time.Time
}

// Detector runs every detector over a stream of classified lines.
type Detector struct {
	cfg Config

	states map[int]*tstate
	order  []int

	start     time.Time
	startSet  bool
	now       time.Time
	curSec    int64
	lines     int
	anomalies int
	warm      bool
	// closing is set while the final partial second is being flushed, so that
	// "the stream ended" is never reported as "this template stopped".
	closing bool

	errCur  int
	errHist [histLen]float64
	errMean float64
	errVar  float64
	errSecs int
	lastErr time.Time

	newRecent []time.Time
	// lastLineSec is the last second in which the stream produced any line at
	// all, used to tell "this template stopped" from "everything stopped".
	lastLineSec int64
	rng         *rand.Rand
}

// New creates a Detector.
func New(cfg Config) *Detector {
	if cfg.Alpha <= 0 {
		cfg.Alpha = 0.12
	}
	return &Detector{cfg: cfg, states: map[int]*tstate{}, rng: rand.New(rand.NewSource(42))}
}

// Config returns the active configuration.
func (d *Detector) Config() Config { return d.cfg }

// Lines returns the number of lines observed.
func (d *Detector) Lines() int { return d.lines }

// Anomalies returns the number of anomalies emitted.
func (d *Detector) Anomalies() int { return d.anomalies }

// Warm reports whether the warm-up period has elapsed.
func (d *Detector) Warm() bool { return d.warm }

// WarmProgress returns a 0..1 estimate of warm-up completion.
func (d *Detector) WarmProgress() float64 {
	if d.warm {
		return 1
	}
	byLines := 0.0
	if d.cfg.WarmupLines > 0 {
		byLines = float64(d.lines) / float64(d.cfg.WarmupLines)
	}
	byTime := 0.0
	if d.cfg.WarmupDur > 0 && d.startSet {
		byTime = d.now.Sub(d.start).Seconds() / d.cfg.WarmupDur.Seconds()
	}
	p := math.Max(byLines, byTime)
	return math.Min(p, 0.999)
}

// Start returns the timestamp of the first observed line.
func (d *Detector) Start() time.Time { return d.start }

// Now returns the detector's current clock (stream time).
func (d *Detector) Now() time.Time { return d.now }

// Tick advances the detector clock without feeding a line. It closes elapsed
// one-second buckets and runs the silence detector.
func (d *Detector) Tick(now time.Time) []Anomaly {
	if !d.startSet {
		return nil
	}
	return d.advance(now)
}

// Finish flushes the last partial second at end of stream. Downward rate
// anomalies and silence are suppressed during the flush: a stream that ended is
// not a template that went quiet.
func (d *Detector) Finish() []Anomaly {
	if !d.startSet {
		return nil
	}
	d.closing = true
	defer func() { d.closing = false }()
	return d.advance(d.now.Truncate(time.Second).Add(time.Second))
}

// Observe feeds one classified line and returns any anomalies it triggered.
func (d *Detector) Observe(rec parse.Record, cl *drain.Cluster, isNew bool, params []drain.Param) []Anomaly {
	now := rec.Time
	if !d.startSet {
		d.start, d.now, d.curSec = now, now, now.Unix()
		d.startSet = true
	}
	out := d.advance(now)
	d.lines++
	d.updateWarm()

	st := d.states[cl.ID]
	if st == nil {
		st = &tstate{cl: cl, lastAlert: map[Kind]time.Time{}, params: map[int]*pstat{}, histSec: d.curSec}
		d.states[cl.ID] = st
		d.order = append(d.order, cl.ID)
	}
	st.cur++
	st.lastSec = d.curSec
	d.lastLineSec = d.curSec
	st.lastLine = rec.Raw
	st.silenced = false

	if parse.IsProblem(rec.Level) {
		d.errCur++
	}

	if isNew {
		// Only templates discovered after warm-up count toward the novelty
		// budget; the vocabulary learned during warm-up is the baseline.
		if d.warm {
			d.newRecent = append(d.newRecent, now)
			for len(d.newRecent) > 0 && now.Sub(d.newRecent[0]) > 60*time.Second {
				d.newRecent = d.newRecent[1:]
			}
		}
		// Novelty only means something once the template vocabulary has
		// settled. While new templates are still arriving in bunches the
		// stream is simply varied, not anomalous.
		limit := d.cfg.MaxNewPerMinute
		if limit <= 0 {
			limit = 3
		}
		if d.warm && len(d.newRecent) <= limit {
			rarity := 1.0 / float64(len(d.newRecent))
			sev := 2
			if parse.IsProblem(rec.Level) {
				sev = 3
			} else if rarity > 0.9 {
				sev = 3
			}
			out = append(out, d.emit(st, Anomaly{
				Time: now, Kind: KindNovel, Severity: sev, Tag: "NOVEL",
				TemplateID: cl.ID, Template: cl.Template(), Line: rec.Raw, Score: rarity,
				Reason: fmt.Sprintf("new template #%d after %d lines and %d known templates, with only %d new template(s) in the last minute: %q",
					cl.ID, d.lines-1, len(d.states), len(d.newRecent), shorten(cl.Template(), 70)),
			}))
		}
	}

	out = append(out, d.checkParams(st, rec, cl, params, now)...)
	return out
}

func (d *Detector) updateWarm() {
	if d.warm {
		return
	}
	// The time condition alone is not enough: a sparse file can cover ten
	// seconds of stream time in thirty lines, which is no baseline at all.
	minLines := d.cfg.WarmupLines / 5
	if minLines < 50 {
		minLines = 50
	}
	if d.lines >= d.cfg.WarmupLines ||
		(d.cfg.WarmupDur > 0 && d.now.Sub(d.start) >= d.cfg.WarmupDur && d.lines >= minLines) {
		d.warm = true
	}
}

// advance closes every elapsed one-second bucket up to now.
func (d *Detector) advance(now time.Time) []Anomaly {
	if now.Before(d.now) {
		return nil // out-of-order line: keep the clock monotonic
	}
	d.now = now
	sec := now.Unix()
	if sec <= d.curSec {
		return nil
	}
	gap := sec - d.curSec
	if gap > 3600 {
		// A jump this large is a discontinuity in the stream, not a stall we
		// watched happen. Re-baseline the silence bookkeeping rather than
		// claiming every template went quiet for a day.
		d.curSec = sec
		d.lastLineSec = sec
		d.errCur = 0
		for _, id := range d.order {
			st := d.states[id]
			st.cur = 0
			st.lastSec = sec
			st.silenced = true
		}
		return nil
	}
	var out []Anomaly
	for s := d.curSec; s < sec; s++ {
		out = append(out, d.closeSecond(s)...)
	}
	d.curSec = sec
	d.updateWarm()
	return out
}

func (d *Detector) closeSecond(sec int64) []Anomaly {
	var out []Anomaly
	at := time.Unix(sec, 0)
	a := d.cfg.Alpha

	for _, id := range d.order {
		st := d.states[id]
		x := float64(st.cur)
		st.hist[mod(sec, histLen)] = x
		if x > 0 {
			st.activeSecs++
		}
		if d.warm && st.secs >= d.cfg.MinSeconds {
			// Counting noise is at least Poisson, so never let the EMA
			// variance claim a template is steadier than sqrt(mean).
			sd := math.Max(math.Sqrt(st.varr), math.Sqrt(st.mean))
			if sd > 0.3 && st.mean >= 2.0 {
				z := (x - st.mean) / sd
				if math.Abs(z) >= d.cfg.Z && (x >= st.mean+2 || x <= st.mean-2) && !(d.closing && z < 0) {
					dir, cmp := "spiked", "above"
					if z < 0 {
						dir, cmp = "dropped", "below"
					}
					out = append(out, d.emit(st, Anomaly{
						Time: at, Kind: KindSpike, Severity: sevFromZ(math.Abs(z)),
						Tag:        fmt.Sprintf("SPIKE %.1fσ", math.Abs(z)),
						TemplateID: st.cl.ID, Template: st.cl.Template(), Line: st.lastLine,
						Score: math.Abs(z),
						Reason: fmt.Sprintf("rate %s to %.0f/s, %.1fσ %s the running mean of %.1f/s (sd %.1f)",
							dir, x, math.Abs(z), cmp, st.mean, sd),
					}))
				}
			}
		}
		emaUpdate(&st.mean, &st.varr, x, a)
		if st.mean > st.peakMean {
			st.peakMean = st.mean
		}
		st.secs++
		st.cur = 0

		// Silence: a template that used to be busy and has gone quiet.
		// Silence only means something while the rest of the stream is still
		// flowing; if everything stopped, that is not this template's story.
		streamAlive := sec-d.lastLineSec < int64(d.cfg.SilenceSecs) && !d.closing
		if d.warm && streamAlive && !st.silenced && st.peakMean >= 1.0 &&
			st.activeSecs >= d.cfg.MinActiveSecs && st.secs >= d.cfg.MinSeconds &&
			sec-st.lastSec >= int64(d.cfg.SilenceSecs) {
			st.silenced = true
			out = append(out, d.emit(st, Anomaly{
				Time: at, Kind: KindSilence, Severity: 2, Tag: "SILENCE",
				TemplateID: st.cl.ID, Template: st.cl.Template(), Line: st.lastLine,
				Score: float64(sec - st.lastSec),
				Reason: fmt.Sprintf("template #%d went silent for %ds after running at %.1f/s for %ds",
					st.cl.ID, sec-st.lastSec, st.peakMean, st.activeSecs),
			}))
		}
	}

	// Error burst over a 5s sliding window versus the EMA baseline.
	x := float64(d.errCur)
	d.errHist[mod(sec, histLen)] = x
	if d.warm && d.errSecs >= d.cfg.MinSeconds {
		win := 0.0
		for i := int64(0); i < 5; i++ {
			win += d.errHist[mod(sec-i, histLen)]
		}
		exp := d.errMean * 5
		sd := math.Sqrt(d.errVar) * math.Sqrt(5)
		if win >= 5 && (sd == 0 || (win-exp)/sd >= d.cfg.BurstZ) && win >= exp*2.5+3 {
			z := d.cfg.BurstZ
			if sd > 0 {
				z = (win - exp) / sd
			}
			if d.lastErr.IsZero() || at.Sub(d.lastErr) >= 15*time.Second {
				d.lastErr = at
				d.anomalies++
				line, tmplID, tmpl := d.worstRecentError()
				out = append(out, Anomaly{
					Time: at, Kind: KindBurst, Severity: sevFromZ(z), Tag: "BURST",
					TemplateID: tmplID, Template: tmpl, Line: line, Score: z,
					Reason: fmt.Sprintf("%.0f WARN/ERROR lines in the last 5s versus a baseline of %.1f (%.1fσ)", win, exp, z),
				})
			}
		}
	}
	emaUpdate(&d.errMean, &d.errVar, x, a)
	d.errSecs++
	d.errCur = 0

	return out
}

// worstRecentError picks the busiest recently-active template as the face of a
// burst so the alert carries a concrete example line.
func (d *Detector) worstRecentError() (string, int, string) {
	var best *tstate
	for _, id := range d.order {
		st := d.states[id]
		if d.curSec-st.lastSec > 5 {
			continue
		}
		if best == nil || st.cl.Count < best.cl.Count {
			best = st // the rarest active template is usually the error one
		}
	}
	if best == nil {
		return "", 0, ""
	}
	return best.lastLine, best.cl.ID, best.cl.Template()
}

func (d *Detector) checkParams(st *tstate, rec parse.Record, cl *drain.Cluster, params []drain.Param, now time.Time) []Anomaly {
	var out []Anomaly
	for _, p := range params {
		ps := st.params[p.Index]
		if ps == nil {
			ps = &pstat{name: p.Name, unit: p.Unit, rng: d.rng}
			st.params[p.Index] = ps
		}
		if ps.name == "" {
			ps.name = p.Name
		}
		if !p.HasNum {
			// A position that is sometimes text is an identifier, not a
			// measurement: stop scoring it.
			ps.nonNum++
			continue
		}
		if d.warm && ps.nonNum == 0 && ps.n >= d.cfg.MinParamSamples && ps.distinct() >= 20 {
			z, logSpace := ps.zscore(p.Value)
			if z >= d.cfg.ParamSigma {
				p99 := ps.quantile(0.99)
				med := ps.quantile(0.5)
				if p99 > 0 && p.Value > p99*d.cfg.ParamP99 {
					space := ""
					if logSpace {
						space = " in log space"
					}
					// Compare against the median when there is one; a median of
					// zero (a counter that is usually idle) makes the ratio
					// meaningless, so fall back to the p99.
					ref, refName := med, "median"
					if ref <= 0 {
						ref, refName = p99, "p99"
					}
					ratio := p.Value / ref
					tail := fmt.Sprintf(" over %d samples", ps.n)
					if refName == "median" {
						tail = fmt.Sprintf(" (p99 %s over %d samples)", fmtVal(p99, ps.unit), ps.n)
					}
					reason := fmt.Sprintf("%s=%s is %sx the %s of %s and %.1fσ above it%s%s",
						p.Name, fmtVal(p.Value, ps.unit), fmtRatio(ratio), refName,
						fmtVal(ref, ps.unit), z, space, tail)
					out = append(out, d.emit(st, Anomaly{
						Time: now, Kind: KindParam, Severity: sevFromRatio(ratio),
						Tag:        "PARAM " + p.Name,
						TemplateID: cl.ID, Template: cl.Template(), Line: rec.Raw,
						Param: p.Name, Value: p.Value, Score: ratio,
						Reason: reason,
					}))
				}
			}
		}
		ps.push(p.Value)
	}
	return out
}

// emit applies per-(template, kind) hysteresis and counts the anomaly.
func (d *Detector) emit(st *tstate, a Anomaly) Anomaly {
	last := st.lastAlert[a.Kind]
	if !last.IsZero() && a.Time.Sub(last) < d.cfg.Cooldown {
		a.Kind = "" // suppressed; filtered out by the caller
		return a
	}
	st.lastAlert[a.Kind] = a.Time
	st.anomalies++
	d.anomalies++
	return a
}

// Filter drops suppressed anomalies produced by emit.
func Filter(in []Anomaly) []Anomaly {
	out := in[:0]
	for _, a := range in {
		if a.Kind != "" {
			out = append(out, a)
		}
	}
	return out
}

func sevFromZ(z float64) int {
	switch {
	case z >= 8:
		return 3
	case z >= 5:
		return 2
	default:
		return 1
	}
}

func sevFromRatio(r float64) int {
	switch {
	case r >= 10:
		return 3
	case r >= 3:
		return 2
	default:
		return 1
	}
}

// fmtRatio keeps "41x" readable and never prints a wall of digits.
func fmtRatio(r float64) string {
	switch {
	case math.IsInf(r, 0) || math.IsNaN(r):
		return "many"
	case r >= 1e6:
		return fmt.Sprintf("%.0e", r)
	case r >= 10:
		return fmt.Sprintf("%.0f", r)
	default:
		return fmt.Sprintf("%.1f", r)
	}
}

func fmtVal(v float64, unit string) string {
	switch {
	case unit == "ms" && v >= 1000:
		return fmt.Sprintf("%.2fs", v/1000)
	case unit != "":
		return fmt.Sprintf("%.0f%s", v, unit)
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		return fmt.Sprintf("%.0f", v)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// Snapshot returns the busiest templates, most active first.
func (d *Detector) Snapshot(limit int) []TemplateStat {
	out := make([]TemplateStat, 0, len(d.order))
	for _, id := range d.order {
		st := d.states[id]
		h := make([]float64, histLen)
		for i := 0; i < histLen; i++ {
			h[i] = st.hist[mod(d.curSec-int64(histLen)+int64(i), histLen)]
		}
		out = append(out, TemplateStat{
			ID: st.cl.ID, Template: st.cl.Template(), Example: st.cl.Example(),
			Count: st.cl.Count, PerSec: st.mean, Hist: h, Anomalies: st.anomalies,
			First: st.cl.First, Last: st.cl.Last,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].PerSec != out[j].PerSec {
			return out[i].PerSec > out[j].PerSec
		}
		return out[i].Count > out[j].Count
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// GlobalHistory returns the last 60 seconds of total line counts.
func (d *Detector) GlobalHistory() []float64 {
	h := make([]float64, histLen)
	for _, id := range d.order {
		st := d.states[id]
		for i := 0; i < histLen; i++ {
			h[i] += st.hist[mod(d.curSec-int64(histLen)+int64(i), histLen)]
		}
	}
	return h
}

// KindLabel returns a human label for a kind.
func KindLabel(k Kind) string {
	switch k {
	case KindNovel:
		return "novel template"
	case KindSpike:
		return "rate spike"
	case KindBurst:
		return "error burst"
	case KindParam:
		return "parameter outlier"
	case KindSilence:
		return "silence"
	}
	return strings.ToUpper(string(k))
}
