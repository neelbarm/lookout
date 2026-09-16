package tui

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// Term owns the controlling terminal: raw mode, the alternate screen, size
// queries and key input. It uses stty so that no third-party package is
// required.
type Term struct {
	tty     *os.File
	out     *bufio.Writer
	saved   string
	mu      sync.Mutex
	w, h    int
	restore sync.Once
}

// IsTTY reports whether f is a character device.
func IsTTY(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// OpenTerm puts the controlling terminal into raw mode and switches to the
// alternate screen. Stdin may be a pipe; keys are read from /dev/tty.
func OpenTerm() (*Term, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	t := &Term{tty: tty, out: bufio.NewWriterSize(tty, 1<<16)}
	if saved, err := t.stty("-g"); err == nil {
		t.saved = strings.TrimSpace(saved)
	}
	if _, err := t.stty("raw", "-echo"); err != nil {
		tty.Close()
		return nil, err
	}
	t.Size()
	t.out.WriteString("\x1b[?1049h\x1b[?25l\x1b[2J")
	t.out.Flush()
	return t, nil
}

func (t *Term) stty(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = t.tty
	out, err := cmd.Output()
	return string(out), err
}

// Size returns the current terminal size, refreshing it from the tty.
func (t *Term) Size() (int, int) {
	out, err := t.stty("size")
	t.mu.Lock()
	defer t.mu.Unlock()
	if err == nil {
		f := strings.Fields(strings.TrimSpace(out))
		if len(f) == 2 {
			h, e1 := strconv.Atoi(f[0])
			w, e2 := strconv.Atoi(f[1])
			if e1 == nil && e2 == nil && w > 0 && h > 0 {
				t.w, t.h = w, h
			}
		}
	}
	if t.w == 0 || t.h == 0 {
		t.w, t.h = 100, 30
	}
	return t.w, t.h
}

// Write queues bytes for the next flush.
func (t *Term) Write(s string) { t.out.WriteString(s) }

// Flush pushes the frame to the terminal in one syscall.
func (t *Term) Flush() { t.out.Flush() }

// Keys starts a goroutine that delivers key bytes until the tty closes.
func (t *Term) Keys() <-chan byte {
	ch := make(chan byte, 32)
	go func() {
		defer close(ch)
		buf := make([]byte, 16)
		for {
			n, err := t.tty.Read(buf)
			if n > 0 {
				for _, b := range buf[:n] {
					ch <- b
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

// Close restores the terminal. It is safe to call more than once, including
// from a deferred call during a panic.
func (t *Term) Close() {
	t.restore.Do(func() {
		t.out.WriteString("\x1b[0m\x1b[?25h\x1b[?1049l")
		t.out.Flush()
		if t.saved != "" {
			t.stty(t.saved)
		} else {
			t.stty("sane")
		}
		t.tty.Close()
	})
}
