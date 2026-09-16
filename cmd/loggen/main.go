// Command loggen emits a realistic web-service log stream with a scripted
// incident, so that lookout can be demonstrated without waiting for a real
// outage. It is deterministic for a given --seed.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"time"
)

type gen struct {
	rng        *rand.Rand
	rate       float64
	duration   time.Duration
	incidentAt time.Duration
	start      time.Time
	live       bool
	w          *bufio.Writer
}

var getPaths = []string{"/api/orders", "/api/products", "/api/users", "/health", "/api/search"}
var postPaths = []string{"/api/checkout", "/api/cart"}
var backends = []string{"pg-primary", "pg-replica"}

func main() {
	var (
		rate       = flag.Float64("rate", 60, "lines per second")
		duration   = flag.Duration("duration", 90*time.Second, "how long to generate")
		seed       = flag.Int64("seed", 7, "random seed")
		incidentAt = flag.Duration("incident-at", 40*time.Second, "when the scripted incident starts")
		file       = flag.String("file", "", "write to this file with real timestamps instead of streaming to stdout")
	)
	flag.Parse()

	g := &gen{
		rng:        rand.New(rand.NewSource(*seed)),
		rate:       *rate,
		duration:   *duration,
		incidentAt: *incidentAt,
	}

	var out *os.File
	if *file != "" {
		f, err := os.Create(*file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "loggen:", err)
			os.Exit(1)
		}
		defer f.Close()
		out = f
		g.live = false
	} else {
		out = os.Stdout
		g.live = true
	}
	g.w = bufio.NewWriterSize(out, 1<<16)
	defer g.w.Flush()

	g.start = time.Now().Truncate(time.Second)
	if !g.live {
		g.start = g.start.Add(-g.duration)
	}
	g.run()
}

// phase returns the incident intensity at t: 0 before, ramping to 1 during the
// outage, then back to 0 through recovery.
func (g *gen) phase(t time.Duration) float64 {
	s := t - g.incidentAt
	switch {
	case s < 0:
		return 0
	case s < 4*time.Second:
		return s.Seconds() / 4
	case s < 22*time.Second:
		return 1
	case s < 30*time.Second:
		return 1 - (s.Seconds()-22)/8
	default:
		return 0
	}
}

func (g *gen) run() {
	interval := time.Duration(float64(time.Second) / g.rate)
	var t time.Duration
	nextGC := 5 * time.Second
	deploys := []time.Duration{5 * time.Second, 75 * time.Second}
	di := 0
	lastFlush := time.Now()

	for t < g.duration {
		now := g.start.Add(t)
		p := g.phase(t)

		if di < len(deploys) && t >= deploys[di] {
			g.emit(now, "INFO", fmt.Sprintf("deploy version=1.4.%d sha=ab12cd34 rollout=complete actor=ci", 7+di))
			di++
		}
		if t >= nextGC {
			nextGC += 5 * time.Second
			g.emit(now, "INFO", fmt.Sprintf("gc cycle pause=%.1fms heap=%dMB collected=%dMB",
				2+g.rng.Float64()*3, 380+g.rng.Intn(80), 40+g.rng.Intn(30)))
		}

		g.emitOne(now, t, p)

		t += interval
		if g.live {
			target := g.start.Add(t)
			if d := time.Until(target); d > 0 {
				time.Sleep(d)
			}
			if time.Since(lastFlush) > 100*time.Millisecond {
				g.w.Flush()
				lastFlush = time.Now()
			}
		}
	}
	g.w.Flush()
}

// emitOne picks one line from the service's mixture of log kinds.
func (g *gen) emitOne(now time.Time, t time.Duration, p float64) {
	// During the outage the connection pool sheds errors of its own and the
	// cache layer stops logging entirely.
	if p > 0.4 && g.rng.Float64() < 0.05 {
		g.emit(now, "ERROR", fmt.Sprintf("connection pool exhausted active=%d max=64 waiters=%d pool=%s",
			60+g.rng.Intn(5), 20+g.rng.Intn(30), backends[g.rng.Intn(len(backends))]))
		return
	}

	// Sessions re-authenticate hard while the pool is down: an existing,
	// previously steady template suddenly runs several times faster.
	if p > 0.4 && g.rng.Float64() < 0.22*p {
		g.emit(now, "INFO", fmt.Sprintf("auth token refreshed user=u%d ttl=3600s", 1000+g.rng.Intn(9000)))
		return
	}

	r := g.rng.Float64()
	switch {
	case r < 0.80:
		g.request(now, p)
	case r < 0.88:
		if p > 0.4 {
			// cache tier is down: it logs nothing at all
			g.request(now, p)
			return
		}
		g.emit(now, "DEBUG", fmt.Sprintf("cache lookup key=session:%d hit=%v size=%dB",
			1000+g.rng.Intn(9000), g.rng.Float64() < 0.85, 120+g.rng.Intn(900)))
	case r < 0.93:
		g.emit(now, "INFO", fmt.Sprintf("auth token refreshed user=u%d ttl=3600s",
			1000+g.rng.Intn(9000)))
	case r < 0.97:
		g.emit(now, "INFO", fmt.Sprintf("worker job done queue=emails job=%d took=%dms",
			100000+g.rng.Intn(99999), 5+g.rng.Intn(60)))
	default:
		g.emit(now, "DEBUG", fmt.Sprintf("metrics flushed points=%d sink=otlp took=%dms",
			200+g.rng.Intn(400), 3+g.rng.Intn(10)))
	}
}

func (g *gen) request(now time.Time, p float64) {
	post := g.rng.Float64() < 0.3
	method, path := "GET", getPaths[g.rng.Intn(len(getPaths))]
	if post {
		method = "POST"
		path = postPaths[0]
		if g.rng.Float64() < 0.4 {
			path = postPaths[1]
		}
	}

	// Latency is log-normal; during the incident checkout latency rises ~30x.
	mu, sigma := math.Log(40), 0.55
	if post {
		mu = math.Log(65)
	}
	lat := math.Exp(mu + sigma*g.rng.NormFloat64())
	if p > 0 && path == "/api/checkout" {
		lat *= 1 + 29*p
	} else if p > 0 {
		lat *= 1 + 0.6*p
	}

	status, level := 200, "INFO"
	switch {
	case p > 0 && g.rng.Float64() < 0.35*p:
		status, level = 500, "ERROR"
	case g.rng.Float64() < 0.02:
		status, level = 404, "WARN"
	case g.rng.Float64() < 0.005:
		status, level = 503, "ERROR"
	}

	g.emit(now, level, fmt.Sprintf("request method=%s path=%s status=%d latency=%.0fms user=u%d req_id=%08x",
		method, path, status, lat, 1000+g.rng.Intn(9000), g.rng.Uint32()))
}

func (g *gen) emit(now time.Time, level, msg string) {
	fmt.Fprintf(g.w, "%s %-5s %s\n", now.Format("2006-01-02T15:04:05.000Z07:00"), level, msg)
}
