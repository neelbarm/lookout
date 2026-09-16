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

const scanBuf = 1 << 20

func newScanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), scanBuf)
	return s
}

// ReaderSource streams lines from r until EOF, then closes the channel.
func ReaderSource(ctx context.Context, r io.Reader) <-chan Event {
	ch := make(chan Event, 1024)
	go func() {
		defer close(ch)
		sc := newScanner(r)
		for sc.Scan() {
			select {
			case ch <- Event{Text: sc.Text(), At: time.Now()}:
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
				pending = append(pending, line...)
				if pending[len(pending)-1] == '\n' {
					text := string(pending[:len(pending)-1])
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
		sc := newScanner(f)
		var prev time.Time
		havePrev := false
		for sc.Scan() {
			text := sc.Text()
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
	sc := newScanner(f)
	now := time.Now()
	for sc.Scan() {
		fn(sc.Text(), now)
	}
	return sc.Err()
}
