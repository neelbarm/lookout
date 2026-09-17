package engine

import (
	"bufio"
	"context"
	"io"
	"os"
	"time"

	"github.com/neelbarmecha/lookout/internal/parse"
)

// Event is one line delivered by a source.
type Event struct {
	Text string
	At   time.Time
}

// MaxLine bounds how much of a single line is retained. A longer line is
// truncated and the rest discarded. bufio.Scanner cannot do this: it fails the
// whole stream with "token too long", which used to abort a report and end a
// live stream silently in the middle of a file.
const MaxLine = 1 << 20

// lineReader reads newline-delimited lines of any length, truncating anything
// past MaxLine so that one pathological line can neither exhaust memory nor
// stop the stream.
type lineReader struct {
	rd  *bufio.Reader
	buf []byte
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{rd: bufio.NewReaderSize(r, 64*1024)}
}

// appendCapped appends b, dropping whatever does not fit in MaxLine.
func (l *lineReader) appendCapped(b []byte) {
	if n := MaxLine - len(l.buf); n > 0 {
		if len(b) > n {
			b = b[:n]
		}
		l.buf = append(l.buf, b...)
	}
}

// next returns the next line without its trailing newline. The error is
// reported only once no more data is available.
func (l *lineReader) next() (string, error) {
	l.buf = l.buf[:0]
	for {
		chunk, err := l.rd.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			l.appendCapped(chunk)
			continue
		}
		if err != nil {
			if len(chunk) == 0 && len(l.buf) == 0 {
				return "", err
			}
			l.appendCapped(chunk)
			return string(l.buf), nil
		}
		l.appendCapped(chunk[:len(chunk)-1])
		return string(l.buf), nil
	}
}

// ReaderSource streams lines from r until EOF, then closes the channel.
func ReaderSource(ctx context.Context, r io.Reader) <-chan Event {
	ch := make(chan Event, 1024)
	go func() {
		defer close(ch)
		lr := newLineReader(r)
		for {
			text, err := lr.next()
			if err != nil {
				return
			}
			select {
			case ch <- Event{Text: text, At: time.Now()}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// TailSource follows a file the way `tail -f` does, including across renames
// and truncations. It never closes the channel until ctx is cancelled.
func TailSource(ctx context.Context, path string, fromStart bool) (<-chan Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fromStart {
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			f.Close()
			return nil, err
		}
	}

	ch := make(chan Event, 1024)
	go func() {
		defer close(ch)
		defer func() {
			if f != nil {
				f.Close()
			}
		}()
		rd := bufio.NewReaderSize(f, 64*1024)
		var pending []byte
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		for {
			line, err := rd.ReadBytes('\n')
			if len(line) > 0 {
				complete := line[len(line)-1] == '\n'
				if complete {
					line = line[:len(line)-1]
				}
				// Cap the partial line the same way lineReader does, so a
				// gigantic line in a followed file cannot grow without bound.
				if n := MaxLine - len(pending); n > 0 {
					if len(line) > n {
						line = line[:n]
					}
					pending = append(pending, line...)
				}
				if complete {
					text := string(pending)
					pending = pending[:0]
					select {
					case ch <- Event{Text: text, At: time.Now()}:
					case <-ctx.Done():
						return
					}
					continue
				}
			}
			if err != nil && err != io.EOF {
				return
			}
			// At EOF: wait, then check for rotation or truncation.
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			ns, serr := os.Stat(path)
			if serr != nil {
				continue
			}
			cur, _ := f.Stat()
			off, _ := f.Seek(0, io.SeekCurrent)
			rotated := cur == nil || !os.SameFile(st, ns) || ns.Size() < off-int64(rd.Buffered())
			if rotated {
				nf, oerr := os.Open(path)
				if oerr != nil {
					continue
				}
				f.Close()
				f = nf
				st = ns
				rd = bufio.NewReaderSize(f, 64*1024)
				pending = pending[:0]
			}
		}
	}()
	return ch, nil
}

// ReplaySource replays a file using its own timestamps, scaled by speed.
func ReplaySource(ctx context.Context, path string, speed float64) (<-chan Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if speed <= 0 {
		speed = 1
	}
	ch := make(chan Event, 1024)
	go func() {
		defer close(ch)
		defer f.Close()
		lr := newLineReader(f)
		var prev time.Time
		havePrev := false
		for {
			text, rerr := lr.next()
			if rerr != nil {
				return
			}
			rec := parse.Parse(text, time.Now())
			if rec.HasTime {
				if havePrev {
					gap := rec.Time.Sub(prev)
					if gap > 0 {
						d := time.Duration(float64(gap) / speed)
						if d > 2*time.Second {
							d = 2 * time.Second
						}
						if d > 0 {
							t := time.NewTimer(d)
							select {
							case <-t.C:
							case <-ctx.Done():
								t.Stop()
								return
							}
						}
					}
				}
				prev, havePrev = rec.Time, true
			}
			select {
			case ch <- Event{Text: text, At: time.Now()}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// ForEachLine reads a whole file as fast as possible, calling fn per line.
func ForEachLine(path string, fn func(text string, at time.Time)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return ForEachReader(f, fn)
}

// ForEachReader reads r to EOF, calling fn per line.
func ForEachReader(r io.Reader, fn func(text string, at time.Time)) error {
	lr := newLineReader(r)
	now := time.Now()
	for {
		text, err := lr.next()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		fn(text, now)
	}
}
