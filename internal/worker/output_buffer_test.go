package worker

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func waitGroupOrTimeout(t *testing.T, wg *sync.WaitGroup, timeout time.Duration) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("timed out waiting for goroutines")
	}
}

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

	var want bytes.Buffer
	for range 10 {
		want.WriteString("x\n")
		if _, err := b.Write([]byte("x\n")); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}

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

	if string(got1) != want.String() {
		t.Fatalf("reader 1 mismatch: got %q want %q", got1, want.String())
	}
	if string(got2) != want.String() {
		t.Fatalf("reader 2 mismatch: got %q want %q", got2, want.String())
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
	b := newOutputBuffer()

	const totalLines = 50
	var want bytes.Buffer
	for i := 1; i <= totalLines; i++ {
		want.WriteString("line\n")
	}

	r1 := b.NewReader()
	r2 := b.NewReader()
	r3 := b.NewReader()

	var writeWg sync.WaitGroup
	writeWg.Go(func() {
		for i := 1; i <= totalLines; i++ {
			if _, err := b.Write([]byte("line\n")); err != nil {
				t.Errorf("Write failed: %v", err)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		b.Close()
	})

	// All three readers run concurrently with the writer and with each other.
	outputs := make([]string, 3)
	var readWg sync.WaitGroup
	for i, r := range []io.ReadCloser{r1, r2, r3} {
		readWg.Go(func() {
			got, err := io.ReadAll(r)
			if err != nil {
				t.Errorf("ReadAll reader %d failed: %v", i+1, err)
				return
			}
			outputs[i] = string(got)
		})
	}

	waitGroupOrTimeout(t, &readWg, 3*time.Second)
	waitGroupOrTimeout(t, &writeWg, 3*time.Second)

	for i, got := range outputs {
		if got != want.String() {
			t.Fatalf("reader %d mismatch: got %d bytes want %d bytes", i+1, len(got), len(want.String()))
		}
	}
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
	if n != 0 || err != io.EOF {
		t.Fatalf("expected io.EOF after reader.Close, got n=%d err=%v", n, err)
	}
}

func TestReaderCloseUnblocksReadWithEOF(t *testing.T) {
	b := newOutputBuffer()
	r := b.NewReader()

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := r.Read(buf)
		done <- err
	}()

	// There is no way to know exactly when the goroutine has entered Read without
	// adding hooks to the production code. In practice it blocks within
	// microseconds, so a short sleep is enough to make the test reliable.
	// TODO: for a more robust test, add an optional callback to reader that fires
	// when cond.Wait is entered, so the test can synchronise on the actual blocked
	// state instead of relying on timing.
	time.Sleep(50 * time.Millisecond)

	if err := r.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	select {
	case err := <-done:
		if err != io.EOF {
			t.Fatalf("expected io.EOF after reader Close, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Read did not unblock after Close")
	}
}
