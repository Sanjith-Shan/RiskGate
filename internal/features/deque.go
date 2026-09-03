package features

// deque is a growable ring buffer. Events enter at the back and expire from
// the front, which a slice re-sliced from the front would handle by leaking
// its head until the next reallocation. Capacity is a power of two so
// indexing is a mask, and it shrinks after a burst so a key that was hot for
// an hour does not hold that memory for a week.
type deque[T any] struct {
	buf  []T
	head int
	n    int
}

func (d *deque[T]) len() int { return d.n }

// at returns a pointer to element i, counted from the front.
func (d *deque[T]) at(i int) *T { return &d.buf[(d.head+i)&(len(d.buf)-1)] }

func (d *deque[T]) front() *T { return d.at(0) }

func (d *deque[T]) back() *T { return d.at(d.n - 1) }

func (d *deque[T]) pushBack(x T) {
	if d.n == len(d.buf) {
		d.resize(max(4, 2*len(d.buf)))
	}
	d.buf[(d.head+d.n)&(len(d.buf)-1)] = x
	d.n++
}

func (d *deque[T]) popFront() {
	var zero T
	d.buf[d.head] = zero
	d.head = (d.head + 1) & (len(d.buf) - 1)
	d.n--
	if len(d.buf) > 16 && d.n <= len(d.buf)/4 {
		d.resize(len(d.buf) / 2)
	}
}

func (d *deque[T]) resize(c int) {
	buf := make([]T, c)
	for i := range d.n {
		buf[i] = *d.at(i)
	}
	d.buf, d.head = buf, 0
}

func (d *deque[T]) cap() int { return len(d.buf) }
