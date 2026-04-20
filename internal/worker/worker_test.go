package worker

import (
	"errors"
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
			_, err := NewWorker().Start(tc.cmd, nil)
			if !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("expected ErrInvalidCommand, got %v", err)
			}
		})
	}
}

func TestWorkerNotFound(t *testing.T) {
	w := NewWorker()

	t.Run("Status", func(t *testing.T) {
		_, err := w.Status("missing")
		if !errors.Is(err, ErrJobNotFound) {
			t.Fatalf("expected ErrJobNotFound, got %v", err)
		}
	})

	t.Run("Stop", func(t *testing.T) {
		err := w.Stop("missing")
		if !errors.Is(err, ErrJobNotFound) {
			t.Fatalf("expected ErrJobNotFound, got %v", err)
		}
	})

	t.Run("StreamOutput", func(t *testing.T) {
		_, err := w.StreamOutput("missing")
		if !errors.Is(err, ErrJobNotFound) {
			t.Fatalf("expected ErrJobNotFound, got %v", err)
		}
	})
}
