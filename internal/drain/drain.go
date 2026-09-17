// Package drain implements online log template mining following the Drain
// algorithm (He et al., ICWS 2017): a fixed-depth parse tree keyed first by
// token count and then by the leading tokens, with a similarity search inside
// each leaf. Templates are refined in place by merging mismatched positions
// into the wildcard <*>.
package drain

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Wildcard is the placeholder token for a variable position.
const Wildcard = "<*>"

// Param is one extracted variable for a matched line.
type Param struct {
	Index  int     // token position in the template
	Name   string  // best-effort name ("latency", "status", "arg3")
	Raw    string  // the raw token as it appeared
	Value  float64 // numeric value, if HasNum
	Unit   string  // "ms" when a duration was normalized, else ""
	HasNum bool
}

// Cluster is a mined log template plus its running bookkeeping.
type Cluster struct {
	ID       int
	Tokens   []string
	Count    int
	First    time.Time
	Last     time.Time
	examples []string
	exNext   int
}

// Template renders the cluster's template string.
func (c *Cluster) Template() string { return strings.Join(c.Tokens, " ") }

// Examples returns the retained example lines, oldest first.
func (c *Cluster) Examples() []string {
	out := make([]string, 0, len(c.examples))
	for i := 0; i < len(c.examples); i++ {
		out = append(out, c.examples[(c.exNext+i)%len(c.examples)])
	}
	return out
}

// Example returns the most recent example line.
func (c *Cluster) Example() string {
	if len(c.examples) == 0 {
		return ""
	}
	return c.examples[(c.exNext-1+len(c.examples))%len(c.examples)]
}

func (c *Cluster) addExample(line string, cap int) {
	if len(c.examples) < cap {
		c.examples = append(c.examples, line)
		c.exNext = len(c.examples) % cap
		return
	}
	c.examples[c.exNext] = line
	c.exNext = (c.exNext + 1) % cap
}

// Config tunes the miner.
type Config struct {
	Depth        int     // parse-tree depth; Depth-2 token layers below the count layer
	SimThreshold float64 // minimum fraction of matching positions to join a cluster
	MaxChildren  int     // per-node child cap before falling back to a wildcard child
	MaxExamples  int     // examples retained per cluster
	MaxClusters  int     // per-leaf cluster cap; beyond it, lines join the closest cluster
}

// DefaultConfig returns the tuned defaults used by the CLI.
func DefaultConfig() Config {
	return Config{Depth: 4, SimThreshold: 0.4, MaxChildren: 512, MaxExamples: 5, MaxClusters: 500}
}

type node struct {
	children map[string]*node
	clusters []*Cluster
}

func newNode() *node { return &node{children: map[string]*node{}} }

// Miner is the online template miner. It is not safe for concurrent use.
type Miner struct {
	cfg      Config
	root     *node
	clusters []*Cluster
	nextID   int
}

// New creates a Miner.
func New(cfg Config) *Miner {
	if cfg.Depth < 3 {
		cfg.Depth = 3
	}
	if cfg.SimThreshold <= 0 {
		cfg.SimThreshold = 0.4
	}
	if cfg.MaxChildren <= 0 {
		cfg.MaxChildren = 512
	}
	if cfg.MaxExamples <= 0 {
		cfg.MaxExamples = 5
	}
	if cfg.MaxClusters <= 0 {
		cfg.MaxClusters = 500
	}
	return &Miner{cfg: cfg, root: newNode(), nextID: 1}
}

// Clusters returns every mined cluster in creation order.
func (m *Miner) Clusters() []*Cluster { return m.clusters }

// Add feeds one message (already stripped of its timestamp/level prefix) to the
// miner and returns the matching cluster, whether it was created by this call,
// and the variable values extracted from this line.
func (m *Miner) Add(msg, raw string, now time.Time) (*Cluster, bool, []Param) {
	rawToks := strings.Fields(msg)
	if len(rawToks) == 0 {
		rawToks = []string{""}
	}
	masked := make([]string, len(rawToks))
	for i, t := range rawToks {
		masked[i] = Mask(t)
	}

	leaf := m.leafFor(masked)
	cl, sim := bestMatch(leaf.clusters, masked)
	created := false
	// Once a leaf is saturated, widening the closest template beats minting an
	// unbounded number of near-duplicates.
	full := len(leaf.clusters) >= m.cfg.MaxClusters
	if cl == nil || (sim < m.cfg.SimThreshold && !full) {
		cl = &Cluster{ID: m.nextID, Tokens: append([]string(nil), masked...), First: now}
		m.nextID++
		leaf.clusters = append(leaf.clusters, cl)
		m.clusters = append(m.clusters, cl)
		created = true
	} else {
		merge(cl.Tokens, masked)
	}
	cl.Count++
	cl.Last = now
	cl.addExample(raw, m.cfg.MaxExamples)
	return cl, created, extractParams(cl.Tokens, rawToks)
}

// leafFor walks (and grows) the parse tree for a masked token sequence.
func (m *Miner) leafFor(masked []string) *node {
	cur := m.root
	cur = child(cur, "len:"+strconv.Itoa(len(masked)), m.cfg.MaxChildren)
	layers := m.cfg.Depth - 2
	for i := 0; i < layers && i < len(masked); i++ {
		cur = child(cur, treeKey(masked[i]), m.cfg.MaxChildren)
	}
	return cur
}

func child(n *node, key string, max int) *node {
	if c, ok := n.children[key]; ok {
		return c
	}
	if len(n.children) >= max {
		if c, ok := n.children[Wildcard]; ok {
			return c
		}
		c := newNode()
		n.children[Wildcard] = c
		return c
	}
	c := newNode()
	n.children[key] = c
	return c
}

// treeKey routes on the masked token. Only a token that masked away entirely
// becomes a wildcard branch: keeping "runningboardd[<*>]:" distinct from
// "mds[<*>]:" is what stops every line in a syslog-shaped stream from landing
// in the same leaf.
func treeKey(tok string) string {
	if tok == Wildcard || tok == "" {
		return Wildcard
	}
	return tok
}

func hasDigit(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			return true
		}
	}
	return false
}

// bestMatch returns the most similar cluster in a leaf and its similarity.
// Only clusters of the same token count are candidates: once the tree's
// per-node child cap folds several token counts into one wildcard branch a
// leaf can hold mixed lengths, and merging across lengths indexes past the end
// of the shorter one.
func bestMatch(cs []*Cluster, masked []string) (*Cluster, float64) {
	var best *Cluster
	bestSim := -1.0
	for _, c := range cs {
		if len(c.Tokens) != len(masked) {
			continue
		}
		s := similarity(c.Tokens, masked)
		if s > bestSim {
			best, bestSim = c, s
		}
	}
	return best, bestSim
}

// similarity is the fraction of positions that match exactly. Wildcard
// positions in the template contribute nothing, as in the original Drain.
func similarity(tmpl, toks []string) float64 {
	if len(tmpl) != len(toks) || len(tmpl) == 0 {
		return 0
	}
	same := 0
	for i := range tmpl {
		if tmpl[i] == Wildcard {
			continue
		}
		if tmpl[i] == toks[i] {
			same++
		}
	}
	return float64(same) / float64(len(tmpl))
}

// merge widens the template in place where the new line disagrees. Callers
// must pass equal lengths; the guard keeps a future caller from panicking.
func merge(tmpl, toks []string) {
	if len(tmpl) != len(toks) {
		return
	}
	for i := range tmpl {
		if tmpl[i] != toks[i] {
			tmpl[i] = Wildcard
		}
	}
}

// extractParams pulls the raw values sitting at variable template positions.
func extractParams(tmpl, raw []string) []Param {
	if len(tmpl) != len(raw) {
		return nil
	}
	var out []Param
	for i, t := range tmpl {
		if !strings.Contains(t, Wildcard) {
			continue
		}
		p := Param{Index: i, Raw: raw[i], Name: paramName(tmpl, raw, i)}
		p.Value, p.Unit, p.HasNum = numeric(raw[i])
		out = append(out, p)
	}
	return out
}

// keyOf returns the "key" of a key=value token, or "" if the token is not one.
// Hand-rolled because this runs on every variable position of every line.
func keyOf(tok string) string {
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		switch {
		case c == '=':
			if i == 0 {
				return ""
			}
			return tok[:i]
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9', c == '.', c == '-':
			if i == 0 {
				return ""
			}
		default:
			return ""
		}
	}
	return ""
}

func paramName(tmpl, raw []string, i int) string {
	if k := keyOf(raw[i]); k != "" {
		return k
	}
	if k := keyOf(tmpl[i]); k != "" {
		return k
	}
	if i > 0 {
		prev := strings.Trim(tmpl[i-1], ":=,")
		if prev != "" && !strings.Contains(prev, Wildcard) && isWord(prev) {
			return prev
		}
	}
	return "arg" + strconv.Itoa(i)
}

func isWord(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_') {
			return false
		}
	}
	return len(s) > 0
}

// numeric extracts a comparable number from a raw token. Durations are
// normalized to milliseconds so "1.2s" and "40ms" live on one scale. Anything
// that is not a plain decimal (optionally followed by a short unit) is
// rejected, so a hex id such as "93e56199" never becomes 9.3e+41.
func numeric(tok string) (float64, string, bool) {
	v := tok
	if eq := strings.IndexByte(v, '='); eq > 0 {
		v = v[eq+1:]
	}
	v = strings.Trim(v, `"',;)(][`)
	if v == "" {
		return 0, "", false
	}

	i := 0
	if v[0] == '-' || v[0] == '+' {
		i++
	}
	digits, dots := 0, 0
	for ; i < len(v); i++ {
		c := v[i]
		if c >= '0' && c <= '9' {
			digits++
			continue
		}
		if c == '.' && dots == 0 && digits > 0 {
			dots++
			continue
		}
		break
	}
	if digits == 0 {
		return 0, "", false
	}
	num, unit := v[:i], v[i:]
	if len(unit) > 3 {
		return 0, "", false
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, "", false
	}
	switch unit {
	case "":
		return finite(f, "")
	case "ns":
		return finite(f/1e6, "ms")
	case "us", "\u00b5s":
		return finite(f/1e3, "ms")
	case "ms":
		return finite(f, "ms")
	case "s":
		return finite(f*1e3, "ms")
	case "m":
		return finite(f*60e3, "ms")
	case "h":
		return finite(f*3600e3, "ms")
	}
	for j := 0; j < len(unit); j++ {
		c := unit[j]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '%') {
			return 0, "", false
		}
	}
	return finite(f, unit)
}

// finite rejects a value that overflowed while being scaled to milliseconds.
// A 300-digit number with an "h" suffix parses fine and then becomes +Inf,
// which poisons the running statistics and makes encoding/json refuse the
// anomaly \u2014 silently, because Encode writes nothing when it fails.
func finite(v float64, unit string) (float64, string, bool) {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, "", false
	}
	return v, unit, true
}

var maskers = []struct {
	re   *regexp.Regexp
	repl string
}{
	// UUID
	{regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`), Wildcard},
	// IPv4 with optional port
	{regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b`), Wildcard},
	// hex literals and long hex ids
	{regexp.MustCompile(`\b0[xX][0-9a-fA-F]+\b`), Wildcard},
	{regexp.MustCompile(`\b[0-9a-fA-F]{12,}\b`), Wildcard},
	// durations and byte sizes
	{regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h)\b`), Wildcard},
	{regexp.MustCompile(`\b\d+(?:\.\d+)?(?:B|KB|MB|GB|TB|KiB|MiB|GiB)\b`), Wildcard},
	// filesystem paths
	{regexp.MustCompile(`(?:/(?:var|usr|tmp|home|etc|opt|bin|sbin|lib|proc|dev|mnt|srv))(?:/[\w.%+@-]+)+`), Wildcard},
	// URL path segments that carry an identifier
	{regexp.MustCompile(`/[\w.%+@-]*\d[\w.%+@-]*`), "/" + Wildcard},
	// timestamps left inside the message body
	{regexp.MustCompile(`\b\d{2}:\d{2}:\d{2}(?:[.,]\d+)?\b`), Wildcard},
	// anything else numeric
	{regexp.MustCompile(`\b\d+(?:\.\d+)?\b`), Wildcard},
	// collapse runs of wildcards, including ones joined by punctuation
	{regexp.MustCompile(`(?:<\*>[.:_/-])+<\*>`), Wildcard},
	{regexp.MustCompile(`(?:<\*>){2,}`), Wildcard},
}

// Mask replaces the obviously-variable parts of a token with the wildcard.
func Mask(tok string) string {
	// Fast path: every masking rule needs either a digit or a path separator,
	// and most tokens in a log line are plain words. Skipping the regex engine
	// for those is worth several times the throughput.
	if !hasDigit(tok) && !strings.ContainsRune(tok, '/') {
		return tok
	}
	for _, m := range maskers {
		tok = m.re.ReplaceAllString(tok, m.repl)
	}
	return tok
}

// MaskLine masks every token of a message; exported for tests and tooling.
func MaskLine(msg string) string {
	f := strings.Fields(msg)
	for i, t := range f {
		f[i] = Mask(t)
	}
	return strings.Join(f, " ")
}
