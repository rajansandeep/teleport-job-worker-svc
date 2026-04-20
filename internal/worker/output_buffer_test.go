package worker

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
)

func TestOutputBufferEmptyWrite(t *testing.T) {
	b := newOutputBuffer()
	r := b.NewReader()
	defer r.Close()

	n, err := b.Write([]byte{})
	if err != nil {
		t.Fatalf("empty Write returned error: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected empty Write to return 0, got %d", n)
	}

	b.Close()

	buf := make([]byte, 8)
	n, err = r.Read(buf)
	if n != 0 || err != io.EOF {
		t.Fatalf("expected io.EOF after empty write + close, got n=%d err=%v", n, err)
	}
}

func TestMultipleReadersAreIndependent(t *testing.T) {
	b := newOutputBuffer()

	data := bytes.Repeat([]byte("x\n"), 10)
	if _, err := b.Write(data); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	want := string(data)

	r1 := b.NewReader()
	r2 := b.NewReader()
	b.Close()

	got1, err := io.ReadAll(r1)
	if err != nil {
		t.Fatalf("ReadAll(r1) failed: %v", err)
	}
	got2, err := io.ReadAll(r2)
	if err != nil {
		t.Fatalf("ReadAll(r2) failed: %v", err)
	}

	if string(got1) != want {
		t.Fatalf("reader 1 mismatch: got %q want %q", got1, want)
	}
	if string(got2) != want {
		t.Fatalf("reader 2 mismatch: got %q want %q", got2, want)
	}
}

func TestReadUsesCallerBufferSize(t *testing.T) {
	b := newOutputBuffer()

	if _, err := b.Write([]byte("abcdefghij")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	b.Close()

	r := b.NewReader()
	defer r.Close()

	buf := make([]byte, 4)

	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("first Read returned unexpected error: %v", err)
	}
	if n != 4 || string(buf[:n]) != "abcd" {
		t.Fatalf("unexpected first chunk: n=%d data=%q", n, buf[:n])
	}

	n, err = r.Read(buf)
	if err != nil {
		t.Fatalf("second Read returned unexpected error: %v", err)
	}
	if n != 4 || string(buf[:n]) != "efgh" {
		t.Fatalf("unexpected second chunk: n=%d data=%q", n, buf[:n])
	}

	n, err = r.Read(buf)
	if err != nil {
		t.Fatalf("third Read returned unexpected error: %v", err)
	}
	if n != 2 || string(buf[:n]) != "ij" {
		t.Fatalf("unexpected third chunk: n=%d data=%q", n, buf[:n])
	}

	n, err = r.Read(buf)
	if n != 0 || err != io.EOF {
		t.Fatalf("expected final EOF, got n=%d err=%v", n, err)
	}
}

func TestBufferCloseReturnsEOFOnceDrained(t *testing.T) {
	b := newOutputBuffer()
	r := b.NewReader()
	defer r.Close()

	if _, err := b.Write([]byte("hello")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	b.Close()

	buf := make([]byte, 8)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("unexpected error while draining buffer: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("unexpected output: got %q want %q", buf[:n], "hello")
	}

	n, err = r.Read(buf)
	if n != 0 || err != io.EOF {
		t.Fatalf("expected io.EOF after draining buffer, got n=%d err=%v", n, err)
	}
}

func TestOutputBufferAllReadersGetFullOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newOutputBuffer()

		const totalLines = 50
		want := strings.Repeat("line\n", totalLines)

		r1 := b.NewReader()
		r2 := b.NewReader()
		r3 := b.NewReader()

		// Writer and readers run concurrently. The writer closes the buffer when
		// done, which unblocks all readers with EOF.
		outputs := make([]string, 3)
		go func() {
			for range totalLines {
				if _, err := b.Write([]byte("line\n")); err != nil {
					t.Errorf("Write failed: %v", err)
					return
				}
			}
			b.Close()
		}()
		for i, r := range []io.ReadCloser{r1, r2, r3} {
			go func() {
				got, err := io.ReadAll(r)
				if err != nil {
					t.Errorf("ReadAll reader %d failed: %v", i+1, err)
					return
				}
				outputs[i] = string(got)
			}()
		}

		// synctest.Wait returns only after all goroutines in the bubble have
		// exited or are durably blocked. All writes to outputs[i] happen before
		// the goroutines exit, so reading outputs here is safe without a mutex
		// under synctest's happens-before guarantee.
		synctest.Wait()

		for i, got := range outputs {
			if got != want {
				t.Fatalf("reader %d mismatch: got %d bytes want %d bytes", i+1, len(got), len(want))
			}
		}
	})
}

func TestOutputBufferCloseSemantics(t *testing.T) {
	t.Run("WriteAfterClose", func(t *testing.T) {
		b := newOutputBuffer()
		b.Close()

		_, err := b.Write([]byte("x"))
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("expected io.ErrClosedPipe, got %v", err)
		}
	})

	t.Run("MultipleClose", func(t *testing.T) {
		b := newOutputBuffer()
		// This subtest is assertless by design.
		// It passes as long as calling Close twice does not panic.
		b.Close()
		b.Close()
	})
}

func TestReaderReadAfterReaderClose(t *testing.T) {
	b := newOutputBuffer()
	r := b.NewReader()

	if err := r.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	buf := make([]byte, 8)
	n, err := r.Read(buf)
	if n != 0 || err != io.ErrClosedPipe {
		t.Fatalf("expected io.ErrClosedPipe after reader.Close, got n=%d err=%v", n, err)
	}
}

func TestReaderCloseUnblocksReadWithEOF(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newOutputBuffer()
		r := b.NewReader()

		done := make(chan error, 1)
		go func() {
			buf := make([]byte, 16)
			_, err := r.Read(buf)
			done <- err
		}()

		// Wait until all goroutines in the bubble are blocked — at this point
		// the goroutine above is guaranteed to be inside cond.Wait.
		synctest.Wait()

		if err := r.Close(); err != nil {
			t.Fatalf("Close failed: %v", err)
		}

		if err := <-done; err != io.ErrClosedPipe {
			t.Fatalf("expected io.ErrClosedPipe after reader Close, got %v", err)
		}
	})
}
