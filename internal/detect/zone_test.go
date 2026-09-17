package detect

import (
	"testing"
	"time"

	"github.com/neelbarmecha/lookout/internal/drain"
	"github.com/neelbarmecha/lookout/internal/parse"
)

// Clock-driven anomalies used to be stamped with time.Unix, which is local
// time, while line-driven ones carried the log's own offset. A report of a UTC
// log then mixed two zones in one column.
func TestClockAnomaliesKeepTheStreamsZone(t *testing.T) {
	zone := time.FixedZone("TEST", 7*3600)
	cfg := DefaultConfig()
	cfg.WarmupLines = 50
	cfg.MinSeconds = 5
	d := New(cfg)
	m := drain.New(drain.DefaultConfig())

	start := time.Date(2026, 9, 16, 10, 0, 0, 0, zone)
	feed := func(at time.Time, msg string) []Anomaly {
		cl, isNew, params := m.Add(msg, msg, at)
		return d.Observe(parse.Record{Raw: msg, Message: msg, Time: at, HasTime: true}, cl, isNew, params)
	}

	var got []Anomaly
	// A steady base template, then a burst of the same template in one second.
	for s := 0; s < 60; s++ {
		for i := 0; i < 3; i++ {
			got = append(got, feed(start.Add(time.Duration(s)*time.Second), "steady heartbeat ok")...)
		}
	}
	for i := 0; i < 80; i++ {
		got = append(got, feed(start.Add(60*time.Second), "steady heartbeat ok")...)
	}
	got = append(got, feed(start.Add(61*time.Second), "steady heartbeat ok")...)
	got = append(got, d.Finish()...)

	saw := false
	for _, a := range Filter(got) {
		if a.Kind != KindSpike && a.Kind != KindSilence {
			continue
		}
		saw = true
		_, off := a.Time.Zone()
		if off != 7*3600 {
			t.Errorf("%s anomaly at %s has offset %ds, want the stream's %ds",
				a.Kind, a.Time.Format(time.RFC3339), off, 7*3600)
		}
	}
	if !saw {
		t.Fatal("no clock-driven anomaly fired; the test no longer covers the zone")
	}
}
