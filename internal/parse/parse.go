// Package parse turns a raw log line into a Record: an optional timestamp, a
// normalized level, and the message body with the timestamp/level prefix
// stripped. Unknown formats still parse: the caller's arrival time is used and
// the whole line becomes the message.
package parse

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Record is a single parsed log line.
type Record struct {
	Raw     string
	Time    time.Time
	HasTime bool
	Level   string // "", TRACE, DEBUG, INFO, WARN, ERROR, FATAL
	Message string
}

var levelNames = map[string]string{
	"TRACE": "TRACE", "TRC": "TRACE",
	"DEBUG": "DEBUG", "DBG": "DEBUG",
	"INFO": "INFO", "INF": "INFO", "NOTICE": "INFO",
	"WARN": "WARN", "WARNING": "WARN", "WRN": "WARN",
	"ERROR": "ERROR", "ERR": "ERROR", "SEVERE": "ERROR", "EXCEPTION": "ERROR",
	"FATAL": "FATAL", "CRIT": "FATAL", "CRITICAL": "FATAL", "PANIC": "FATAL", "EMERG": "FATAL",
}

// IsProblem reports whether a level counts toward the error-burst detector.
func IsProblem(level string) bool {
	switch level {
	case "WARN", "ERROR", "FATAL":
		return true
	}
	return false
}

// Severity orders levels for display purposes.
func Severity(level string) int {
	switch level {
	case "FATAL":
		return 3
	case "ERROR":
		return 3
	case "WARN":
		return 2
	}
	return 1
}

var (
	reISO = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:[.,]\d{1,9})?(?:Z|[+-]\d{2}(?::?\d{2})?)?)\s*`)
	// [2024-01-02 15:04:05] or [2024-01-02T15:04:05Z]
	reBracketISO = regexp.MustCompile(`^\[(\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:[.,]\d{1,9})?(?:Z|[+-]\d{2}(?::?\d{2})?)?)\]\s*`)
	// Jan  2 15:04:05 host proc[123]:
	reSyslog = regexp.MustCompile(`^([A-Z][a-z]{2}\s+\d{1,2} \d{2}:\d{2}:\d{2})\s+(\S+\s+)?(\S+?(?:\[\d+\])?:)?\s*`)
	// nginx / apache common (and combined) log format
	reCommon = regexp.MustCompile(`^(\S+) \S+ (\S+) \[(\d{2}/[A-Za-z]{3}/\d{4}:\d{2}:\d{2}:\d{2} [+-]\d{4})\] "([^"]*)" (\d{3}) (\S+)(.*)$`)
	// a leading level token: ERROR  [ERROR]  ERROR:  E/
	reLevelPrefix = regexp.MustCompile(`^(?:\[\s*([A-Za-z]{3,9})\s*\]|([A-Za-z]{3,9})\s*:|([A-Z]{3,9})\b)\s*`)
	reLogfmtKey   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*=`)
	rePanicy      = regexp.MustCompile(`(?i)\b(panic|traceback|exception|fatal error|segfault|stack trace)\b`)
)

var isoLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	// Go's parser accepts a fractional second of any length after the seconds
	// field even when the layout does not mention one, so these cover
	// ".040548-0400" and ".510-0500" alike.
	"2006-01-02T15:04:05Z0700",
	"2006-01-02 15:04:05Z0700",
	"2006-01-02T15:04:05.000Z07",
	"2006-01-02T15:04:05Z07",
	"2006-01-02 15:04:05.000Z07",
	"2006-01-02 15:04:05Z07",
	"2006-01-02T15:04:05.000",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05.000000",
	"2006-01-02 15:04:05.000",
	"2006-01-02 15:04:05",
}

// reColonMillis matches vendors that separate milliseconds with a colon, as in
// 2026-02-09T13:28:05:510-0500.
var reColonMillis = regexp.MustCompile(`(\d{2}:\d{2}:\d{2}):(\d{1,9})`)

func parseISO(s string) (time.Time, bool) {
	s = strings.Replace(s, ",", ".", 1)
	s = reColonMillis.ReplaceAllString(s, "$1.$2")
	for _, l := range isoLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	// "2006-01-02 15:04:05Z07:00" style with a space separator
	if t, err := time.Parse(time.RFC3339Nano, strings.Replace(s, " ", "T", 1)); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// Parse parses one line. arrival is used when the line carries no timestamp.
func Parse(line string, arrival time.Time) Record {
	line = strings.TrimRight(line, "\r\n")
	r := Record{Raw: line, Time: arrival, Message: line}

	trimmed := strings.TrimLeft(line, " \t")
	switch {
	case strings.HasPrefix(trimmed, "{"):
		if parseJSON(trimmed, &r) {
			return finish(r)
		}
	case reCommon.MatchString(line):
		if parseCommon(line, &r) {
			return finish(r)
		}
	case reLogfmtKey.MatchString(trimmed):
		if parseLogfmt(trimmed, &r) {
			return finish(r)
		}
	}

	body := line
	if m := reBracketISO.FindStringSubmatch(body); m != nil {
		if t, ok := parseISO(m[1]); ok {
			r.Time, r.HasTime = t, true
			body = body[len(m[0]):]
		}
	} else if m := reISO.FindStringSubmatch(body); m != nil {
		if t, ok := parseISO(m[1]); ok {
			r.Time, r.HasTime = t, true
			body = body[len(m[0]):]
		}
	} else if m := reSyslog.FindStringSubmatch(body); m != nil {
		if t, err := time.Parse("Jan _2 15:04:05", normalizeSyslog(m[1])); err == nil {
			t = t.AddDate(arrival.Year(), 0, 0)
			r.Time, r.HasTime = t, true
			body = body[len(m[0]):]
		}
	}

	// A level prefix, if present, right after the timestamp.
	if m := reLevelPrefix.FindStringSubmatch(body); m != nil {
		tok := m[1] + m[2] + m[3]
		if lv, ok := levelNames[strings.ToUpper(tok)]; ok {
			r.Level = lv
			body = body[len(m[0]):]
		}
	}
	r.Message = strings.TrimSpace(body)
	return finish(r)
}

func normalizeSyslog(s string) string {
	f := strings.Fields(s)
	if len(f) == 3 {
		return f[0] + " " + f[1] + " " + f[2]
	}
	return s
}

func parseJSON(s string, r *Record) bool {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return false
	}
	for _, k := range []string{"time", "ts", "timestamp", "@timestamp", "t"} {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch x := v.(type) {
		case string:
			if t, ok := parseISO(x); ok {
				r.Time, r.HasTime = t, true
			}
		case float64:
			if x > 1e11 { // milliseconds
				r.Time, r.HasTime = time.UnixMilli(int64(x)), true
			} else if x > 1e8 {
				r.Time, r.HasTime = time.Unix(int64(x), 0), true
			}
		}
		if r.HasTime {
			delete(m, k)
			break
		}
	}
	for _, k := range []string{"level", "lvl", "severity", "loglevel"} {
		if v, ok := m[k].(string); ok {
			if lv, ok := levelNames[strings.ToUpper(v)]; ok {
				r.Level = lv
				delete(m, k)
				break
			}
		}
	}
	msg := ""
	for _, k := range []string{"msg", "message", "event", "text"} {
		if v, ok := m[k].(string); ok {
			msg = v
			delete(m, k)
			break
		}
	}
	// Append the remaining fields in a stable order so templates are stable.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	var b strings.Builder
	b.WriteString(msg)
	for _, k := range keys {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(scalar(m[k]))
	}
	r.Message = b.String()
	return true
}

func scalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case nil:
		return "null"
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return "?"
		}
		return string(b)
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func parseCommon(line string, r *Record) bool {
	m := reCommon.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	if t, err := time.Parse("02/Jan/2006:15:04:05 -0700", m[3]); err == nil {
		r.Time, r.HasTime = t, true
	}
	status, _ := strconv.Atoi(m[5])
	switch {
	case status >= 500:
		r.Level = "ERROR"
	case status >= 400:
		r.Level = "WARN"
	default:
		r.Level = "INFO"
	}
	r.Message = strings.TrimSpace(m[1] + " " + m[4] + " " + m[5] + " " + m[6] + m[7])
	return true
}

func parseLogfmt(line string, r *Record) bool {
	pairs := splitLogfmt(line)
	if len(pairs) < 2 {
		return false
	}
	var keep []string
	sawKey := false
	for _, p := range pairs {
		eq := strings.IndexByte(p, '=')
		if eq <= 0 {
			keep = append(keep, p)
			continue
		}
		k := strings.ToLower(p[:eq])
		v := strings.Trim(p[eq+1:], `"`)
		switch k {
		case "time", "ts", "timestamp", "t":
			if t, ok := parseISO(v); ok {
				r.Time, r.HasTime = t, true
				sawKey = true
				continue
			}
		case "level", "lvl", "severity":
			if lv, ok := levelNames[strings.ToUpper(v)]; ok {
				r.Level = lv
				sawKey = true
				continue
			}
		}
		keep = append(keep, p)
	}
	if !sawKey {
		return false
	}
	r.Message = strings.Join(keep, " ")
	return true
}

// splitLogfmt splits on spaces but keeps quoted values together.
func splitLogfmt(s string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQ = !inQ
			cur.WriteByte(c)
		case c == ' ' && !inQ:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func finish(r Record) Record {
	if r.Message == "" {
		r.Message = r.Raw
	}
	if r.Level == "" {
		if rePanicy.MatchString(r.Message) {
			r.Level = "ERROR"
		} else if lv := scanLevel(r.Message); lv != "" {
			r.Level = lv
		}
	}
	return r
}

// scanLevel looks for a bare level word in the first few tokens of a message.
func scanLevel(msg string) string {
	f := strings.Fields(msg)
	if len(f) > 4 {
		f = f[:4]
	}
	for _, tok := range f {
		t := strings.Trim(tok, "[]():,")
		if t != strings.ToUpper(t) {
			continue
		}
		if lv, ok := levelNames[t]; ok {
			return lv
		}
	}
	return ""
}
