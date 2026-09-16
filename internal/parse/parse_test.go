package parse

import (
	"testing"
	"time"
)

var ref = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

func TestTimestampFormats(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string // RFC3339 in UTC
		msg  string
	}{
		{
			"rfc3339",
			"2026-09-16T18:24:09Z INFO request method=GET",
			"2026-09-16T18:24:09Z",
			"request method=GET",
		},
		{
			"rfc3339 nanos and offset",
			"2026-09-16T18:24:09.123-04:00 ERROR pool exhausted",
			"2026-09-16T22:24:09Z",
			"pool exhausted",
		},
		{
			"space separated with short offset",
			"2026-09-14 00:44:56-04 host installd[7543]: PackageKit",
			"2026-09-14T04:44:56Z",
			"host installd[7543]: PackageKit",
		},
		{
			"space separated with micros and compact offset",
			"2026-09-16 18:24:28.040548-0400  localhost runningboardd[386]: hello",
			"2026-09-16T22:24:28Z",
			"localhost runningboardd[386]: hello",
		},
		{
			"bracketed",
			"[2026-09-16 18:24:09] WARN disk almost full",
			"2026-09-16T18:24:09Z",
			"disk almost full",
		},
		{
			"comma millis",
			"2026-09-16 18:24:09,250 INFO started",
			"2026-09-16T18:24:09Z",
			"started",
		},
		{
			"vendor colon millis in logfmt",
			`SessionID=abc Timestamp=2026-02-09T13:28:05:510-0500 Description="hello"`,
			"2026-02-09T18:28:05Z",
			`SessionID=abc Description="hello"`,
		},
		{
			"nginx common log",
			`10.0.0.7 - frank [16/Sep/2026:18:24:09 -0400] "GET /api/orders HTTP/1.1" 200 2326`,
			"2026-09-16T22:24:09Z",
			"",
		},
		{
			"json",
			`{"ts":"2026-09-16T18:24:09Z","level":"error","msg":"pool exhausted","waiters":37}`,
			"2026-09-16T18:24:09Z",
			"pool exhausted waiters=37",
		},
		{
			"logfmt",
			`ts=2026-09-16T18:24:09Z level=warn msg="slow query" dur=1.2s`,
			"2026-09-16T18:24:09Z",
			`msg="slow query" dur=1.2s`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Parse(c.line, ref)
			if !r.HasTime {
				t.Fatalf("no timestamp parsed from %q", c.line)
			}
			if got := r.Time.UTC().Format(time.RFC3339); got != c.want {
				t.Errorf("time = %s, want %s", got, c.want)
			}
			if c.msg != "" && r.Message != c.msg {
				t.Errorf("message = %q, want %q", r.Message, c.msg)
			}
		})
	}
}

func TestSyslogTimestamp(t *testing.T) {
	r := Parse("Sep 16 18:24:09 mac kernel[0]: something happened", ref)
	if !r.HasTime {
		t.Fatal("syslog timestamp not parsed")
	}
	if r.Time.Month() != time.September || r.Time.Day() != 16 || r.Time.Hour() != 18 {
		t.Errorf("syslog time = %v", r.Time)
	}
	if r.Message != "something happened" {
		t.Errorf("message = %q", r.Message)
	}
}

func TestLevels(t *testing.T) {
	cases := []struct{ line, want string }{
		{"2026-09-16T18:24:09Z ERROR boom", "ERROR"},
		{"2026-09-16T18:24:09Z WARN careful", "WARN"},
		{"2026-09-16T18:24:09Z INFO fine", "INFO"},
		{"2026-09-16T18:24:09Z DEBUG noisy", "DEBUG"},
		{"2026-09-16T18:24:09Z FATAL dead", "FATAL"},
		{"[WARNING] something", "WARN"},
		{`{"level":"error","msg":"x"}`, "ERROR"},
		{`ts=2026-09-16T18:24:09Z level=debug msg=x`, "DEBUG"},
		{"goroutine panic: runtime error", "ERROR"},
		{`10.0.0.7 - - [16/Sep/2026:18:24:09 -0400] "GET /x HTTP/1.1" 503 12`, "ERROR"},
		{`10.0.0.7 - - [16/Sep/2026:18:24:09 -0400] "GET /x HTTP/1.1" 404 12`, "WARN"},
		{`10.0.0.7 - - [16/Sep/2026:18:24:09 -0400] "GET /x HTTP/1.1" 200 12`, "INFO"},
	}
	for _, c := range cases {
		if got := Parse(c.line, ref).Level; got != c.want {
			t.Errorf("level(%q) = %q, want %q", c.line, got, c.want)
		}
	}
	if !IsProblem("ERROR") || !IsProblem("WARN") || !IsProblem("FATAL") {
		t.Error("IsProblem should accept WARN/ERROR/FATAL")
	}
	if IsProblem("INFO") || IsProblem("") {
		t.Error("IsProblem should reject INFO and the empty level")
	}
}

func TestUnknownFormatStillWorks(t *testing.T) {
	line := "totally unstructured line with no timestamp at all"
	r := Parse(line, ref)
	if r.HasTime {
		t.Error("should not claim a timestamp")
	}
	if !r.Time.Equal(ref) {
		t.Errorf("should fall back to arrival time, got %v", r.Time)
	}
	if r.Message != line {
		t.Errorf("message = %q, want the whole line", r.Message)
	}
}

func TestJSONFieldsAreStable(t *testing.T) {
	a := Parse(`{"msg":"hi","b":2,"a":1}`, ref).Message
	b := Parse(`{"a":1,"msg":"hi","b":2}`, ref).Message
	if a != b {
		t.Errorf("JSON field order leaked into the message: %q vs %q", a, b)
	}
	if a != "hi a=1 b=2" {
		t.Errorf("message = %q", a)
	}
}
