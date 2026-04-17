package worker

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestWorkerStartInvalidCommands(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
	}{
		{"empty", ""},
		{"spaces", "   "},
		{"tab", "\t"},
		{"newline", "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewWorker().Start(t.Context(), tc.cmd, nil)
			if !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("expected ErrInvalidCommand, got %v", err)
			}
		})
	}
}

func TestWorkerNotFound(t *testing.T) {
	ctx := t.Context()
	w := NewWorker()

	t.Run("Status", func(t *testing.T) {
		_, err := w.Status(ctx, "missing")
		if !errors.Is(err, ErrJobNotFound) {
			t.Fatalf("expected ErrJobNotFound, got %v", err)
		}
	})
	t.Run("Stop", func(t *testing.T) {
		err := w.Stop(ctx, "missing")
		if !errors.Is(err, ErrJobNotFound) {
			t.Fatalf("expected ErrJobNotFound, got %v", err)
		}
	})
	t.Run("StreamOutput", func(t *testing.T) {
		_, err := w.StreamOutput(ctx, "missing")
		if !errors.Is(err, ErrJobNotFound) {
			t.Fatalf("expected ErrJobNotFound, got %v", err)
		}
	})
}

// recordingCloser is an io.ReadCloser that records whether Close was called.
type recordingCloser struct {
	io.Reader
	closed bool
}

func (r *recordingCloser) Close() error {
	r.closed = true
	return nil
}

func TestCancelOnCloseReaderCallsStopOnClose(t *testing.T) {
	stopped := false
	r := &cancelOnCloseReader{
		ReadCloser: io.NopCloser(strings.NewReader("")),
		stop:       func() bool { stopped = true; return true },
	}

	if err := r.Close(); err != nil {
		t.Fatalf("unexpected Close error: %v", err)
	}
	if !stopped {
		t.Fatal("expected stop to be called on Close")
	}
}

func TestCancelOnCloseReaderDelegatesClose(t *testing.T) {
	rc := &recordingCloser{Reader: strings.NewReader("")}
	r := &cancelOnCloseReader{
		ReadCloser: rc,
		stop:       func() bool { return true },
	}

	if err := r.Close(); err != nil {
		t.Fatalf("unexpected Close error: %v", err)
	}
	if !rc.closed {
		t.Fatal("expected underlying reader to be closed")
	}
}
