package worker

import (
	"io"
	"sync"
)

// outputBuffer is an append-only, multi-reader byte buffer. Writers append to a
// single shared slice. Each reader maintains its own offset so late joiners
// replay from byte 0. Blocked readers are woken via a condition variable when
// new data arrives or the buffer is closed.
type outputBuffer struct {
	mu   sync.Mutex
	cond *sync.Cond
	// TODO: cap data to prevent unbounded memory growth for long-running jobs.
	// Options: a configurable max with Write returning an error or file-backed
	// storage with a rolling window for large outputs.
	data   []byte
	closed bool
}

func newOutputBuffer() *outputBuffer {
	b := &outputBuffer{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *outputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(p) == 0 {
		return 0, nil
	}

	if b.closed {
		return 0, io.ErrClosedPipe
	}

	b.data = append(b.data, p...)
	b.cond.Broadcast()
	return len(p), nil
}

func (b *outputBuffer) NewReader() io.ReadCloser {
	return &reader{buffer: b}
}

func (b *outputBuffer) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}

	b.closed = true
	b.cond.Broadcast()
}

type reader struct {
	buffer *outputBuffer
	offset int
	closed bool
}

func (r *reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	b := r.buffer
	b.mu.Lock()
	defer b.mu.Unlock()

	for {
		if r.closed {
			return 0, io.ErrClosedPipe
		}

		if r.offset < len(b.data) {
			n := copy(p, b.data[r.offset:])
			r.offset += n
			return n, nil
		}

		if b.closed {
			return 0, io.EOF
		}

		b.cond.Wait()
	}
}

func (r *reader) Close() error {
	b := r.buffer
	b.mu.Lock()
	defer b.mu.Unlock()

	if r.closed {
		return nil
	}

	r.closed = true
	b.cond.Broadcast()
	return nil
}
