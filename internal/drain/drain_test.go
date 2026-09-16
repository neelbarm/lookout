package drain

import (
	"strings"
	"testing"
	"time"
)

// corpus is 40 lines drawn from exactly 6 distinct message shapes. A correct
// miner recovers those 6 and nothing more.
var corpus = []string{
	"Received block blk_1608999687919862906 of size 91178 from /10.250.19.102",
	"Received block blk_7503483334202473044 of size 233217 from /10.250.10.6",
	"Received block blk_-3544583377289625738 of size 67072 from /10.250.19.102",
	"Received block blk_-9073992586687739851 of size 91178 from /10.251.31.85",
	"Received block blk_3587508140051953248 of size 91178 from /10.251.42.84",
	"Received block blk_5402003568334525940 of size 67072 from /10.251.43.21",
	"Received block blk_1122334455667788990 of size 12345 from /10.251.43.22",
	"PacketResponder 1 for block blk_1608999687919862906 terminating",
	"PacketResponder 0 for block blk_7503483334202473044 terminating",
	"PacketResponder 2 for block blk_-3544583377289625738 terminating",
	"PacketResponder 1 for block blk_-9073992586687739851 terminating",
	"PacketResponder 0 for block blk_3587508140051953248 terminating",
	"PacketResponder 2 for block blk_5402003568334525940 terminating",
	"PacketResponder 1 for block blk_1122334455667788990 terminating",
	"Verification succeeded for blk_1608999687919862906",
	"Verification succeeded for blk_7503483334202473044",
	"Verification succeeded for blk_-3544583377289625738",
	"Verification succeeded for blk_-9073992586687739851",
	"Verification succeeded for blk_3587508140051953248",
	"Verification succeeded for blk_5402003568334525940",
	"Verification succeeded for blk_1122334455667788990",
	"request method=GET path=/api/orders status=200 latency=42ms",
	"request method=GET path=/api/orders status=200 latency=51ms",
	"request method=GET path=/api/orders status=404 latency=13ms",
	"request method=GET path=/api/orders status=200 latency=38ms",
	"request method=GET path=/api/orders status=500 latency=902ms",
	"request method=GET path=/api/orders status=200 latency=44ms",
	"request method=GET path=/api/orders status=200 latency=47ms",
	"cache lookup key=session:1001 hit=true size=213B",
	"cache lookup key=session:1002 hit=false size=884B",
	"cache lookup key=session:1003 hit=true size=112B",
	"cache lookup key=session:1004 hit=true size=755B",
	"cache lookup key=session:1005 hit=false size=431B",
	"cache lookup key=session:1006 hit=true size=640B",
	"cache lookup key=session:1007 hit=true size=318B",
	"worker job done queue=emails job=100001 took=12ms",
	"worker job done queue=emails job=100002 took=31ms",
	"worker job done queue=emails job=100003 took=7ms",
	"worker job done queue=emails job=100004 took=55ms",
	"worker job done queue=emails job=100005 took=23ms",
}

func mine(t *testing.T, lines []string) *Miner {
	t.Helper()
	m := New(DefaultConfig())
	now := time.Now()
	for _, l := range lines {
		m.Add(l, l, now)
	}
	return m
}

func TestSixTemplatesFromFortyLines(t *testing.T) {
	m := mine(t, corpus)
	got := m.Clusters()
	if len(got) != 6 {
		for _, c := range got {
			t.Logf("  #%d x%d  %s", c.ID, c.Count, c.Template())
		}
		t.Fatalf("expected 6 templates, got %d", len(got))
	}

	want := []string{
		"Received block <*> of size <*> from /<*>",
		"PacketResponder <*> for block <*> terminating",
		"Verification succeeded for <*>",
		"request method=GET path=/api/orders status=<*> latency=<*>",
		"cache lookup key=session:<*> <*> size=<*>",
		"worker job done queue=emails job=<*> took=<*>",
	}
	have := map[string]int{}
	for _, c := range got {
		have[c.Template()] = c.Count
	}
	for _, w := range want {
		if _, ok := have[w]; !ok {
			t.Errorf("missing template %q", w)
			for k := range have {
				t.Logf("  have: %q", k)
			}
		}
	}

	total := 0
	for _, c := range got {
		total += c.Count
	}
	if total != len(corpus) {
		t.Errorf("counts sum to %d, want %d", total, len(corpus))
	}
}

func TestStableAcrossRepeats(t *testing.T) {
	// Feeding the corpus three times must not create new templates.
	all := append(append(append([]string{}, corpus...), corpus...), corpus...)
	m := mine(t, all)
	if len(m.Clusters()) != 6 {
		t.Fatalf("expected 6 templates after repeats, got %d", len(m.Clusters()))
	}
	for _, c := range m.Clusters() {
		if c.Count%len(corpus) != 0 && c.Count < 3 {
			t.Errorf("template %q has suspicious count %d", c.Template(), c.Count)
		}
	}
}

func TestParameterPositionsAndValues(t *testing.T) {
	m := New(DefaultConfig())
	now := time.Now()
	line := "request method=GET path=/api/orders status=200 latency=42ms"
	for i := 0; i < 5; i++ {
		m.Add(line, line, now)
	}
	_, _, params := m.Add(
		"request method=GET path=/api/orders status=500 latency=3120ms",
		"request method=GET path=/api/orders status=500 latency=3120ms", now)

	byName := map[string]Param{}
	for _, p := range params {
		byName[p.Name] = p
	}

	lat, ok := byName["latency"]
	if !ok {
		t.Fatalf("no latency parameter extracted, got %+v", params)
	}
	if lat.Index != 4 {
		t.Errorf("latency at index %d, want 4", lat.Index)
	}
	if !lat.HasNum || lat.Value != 3120 || lat.Unit != "ms" {
		t.Errorf("latency = %v %q num=%v, want 3120 ms", lat.Value, lat.Unit, lat.HasNum)
	}
	st, ok := byName["status"]
	if !ok || st.Index != 3 || st.Value != 500 {
		t.Errorf("status parameter wrong: %+v", st)
	}
}

func TestDurationNormalization(t *testing.T) {
	cases := []struct {
		tok  string
		want float64
		unit string
		ok   bool
	}{
		{"latency=42ms", 42, "ms", true},
		{"latency=1.2s", 1200, "ms", true},
		{"took=500us", 0.5, "ms", true},
		{"d=2m", 120000, "ms", true},
		{"count=17", 17, "", true},
		{"size=213B", 213, "B", true},
		{"req_id=93e56199", 0, "", false}, // hex, not scientific notation
		{"id=deadbeef", 0, "", false},
		{"name=orders", 0, "", false},
	}
	for _, c := range cases {
		v, u, ok := numeric(c.tok)
		if ok != c.ok || (ok && (v != c.want || u != c.unit)) {
			t.Errorf("numeric(%q) = %v %q %v, want %v %q %v", c.tok, v, u, ok, c.want, c.unit, c.ok)
		}
	}
}

func TestMasking(t *testing.T) {
	cases := []struct{ in, want string }{
		{"user=8842", "user=<*>"},
		{"10.250.19.102", "<*>"},
		{"10.250.19.102:8080", "<*>"},
		{"7f3a9c1b-1111-2222-3333-444455556666", "<*>"},
		{"0xdeadbeef", "<*>"},
		{"latency=42ms", "latency=<*>"},
		{"size=213B", "size=<*>"},
		{"/var/log/nginx/access.log", "<*>"},
		{"/api/users/12345/orders", "/api/users/<*>/orders"},
		{"/api/orders", "/api/orders"},
		{"terminating", "terminating"},
		{"12:34:56.789", "<*>"},
	}
	for _, c := range cases {
		if got := Mask(c.in); got != c.want {
			t.Errorf("Mask(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSimilarityAndMerge(t *testing.T) {
	tmpl := []string{"a", "b", "c", "d"}
	if s := similarity(tmpl, []string{"a", "b", "c", "d"}); s != 1 {
		t.Errorf("identical similarity = %v, want 1", s)
	}
	if s := similarity(tmpl, []string{"a", "b", "x", "y"}); s != 0.5 {
		t.Errorf("half similarity = %v, want 0.5", s)
	}
	if s := similarity(tmpl, []string{"a", "b"}); s != 0 {
		t.Errorf("different length similarity = %v, want 0", s)
	}
	merge(tmpl, []string{"a", "z", "c", "z"})
	if strings.Join(tmpl, " ") != "a <*> c <*>" {
		t.Errorf("merge produced %q", strings.Join(tmpl, " "))
	}
	// A wildcard position contributes nothing to similarity.
	if s := similarity([]string{"a", Wildcard, "c", Wildcard}, []string{"a", "q", "c", "r"}); s != 0.5 {
		t.Errorf("wildcard similarity = %v, want 0.5", s)
	}
}

func TestLeafCapBoundsClusterCount(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxClusters = 8
	m := New(cfg)
	now := time.Now()
	// Every line has the same token count and leading token but is otherwise
	// unique, so without the cap this would mint one cluster per line.
	for i := 0; i < 200; i++ {
		l := "evt " + strings.Repeat("x", 1+i%7) + " " + randWord(i) + " " + randWord(i*7)
		m.Add(l, l, now)
	}
	if n := len(m.Clusters()); n > 40 {
		t.Errorf("cluster count %d is not bounded by the leaf cap", n)
	}
}

func randWord(i int) string {
	const alpha = "abcdefghijklmnopqrstuvwxyz"
	return string([]byte{alpha[i%26], alpha[(i/26)%26], alpha[(i/7)%26]})
}

func TestExamplesRingBuffer(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxExamples = 3
	m := New(cfg)
	now := time.Now()
	for i := 0; i < 10; i++ {
		l := "job done id=" + randWord(i)
		m.Add(l, l, now)
	}
	c := m.Clusters()[0]
	ex := c.Examples()
	if len(ex) != 3 {
		t.Fatalf("kept %d examples, want 3", len(ex))
	}
	if c.Example() != ex[len(ex)-1] {
		t.Errorf("Example() = %q, want the newest %q", c.Example(), ex[len(ex)-1])
	}
}
