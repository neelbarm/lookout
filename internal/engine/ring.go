package engine

// ring is a fixed-capacity circular buffer. Appending is allocation-free once
// the buffer is warm, which is what keeps lookout's per-line cost flat.
type ring[T any] struct {
	buf  []T
	next int
	n    int
}

func newRing[T any](capacity int) ring[T] {
	if capacity <= 0 {
		capacity = 1
	}
	return ring[T]{buf: make([]T, capacity)}
}

func (r *ring[T]) push(v T) {
	if len(r.buf) == 0 {
		r.buf = make([]T, 1)
	}
	r.buf[r.next] = v
	r.next = (r.next + 1) % len(r.buf)
	if r.n < len(r.buf) {
		r.n++
	}
}

func (r *ring[T]) len() int { return r.n }

// eachForward visits the retained items oldest first until fn returns false.
func (r *ring[T]) eachForward(fn func(T) bool) {
	if r.n == 0 {
		return
	}
	start := (r.next - r.n + len(r.buf)) % len(r.buf)
	for i := 0; i < r.n; i++ {
		if !fn(r.buf[(start+i)%len(r.buf)]) {
			return
		}
	}
}

// eachBackward visits the retained items newest first until fn returns false.
func (r *ring[T]) eachBackward(fn func(T) bool) {
	for i := 0; i < r.n; i++ {
		idx := (r.next - 1 - i + 2*len(r.buf)) % len(r.buf)
		if !fn(r.buf[idx]) {
			return
		}
	}
}
