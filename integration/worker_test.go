//go:build integration

package integration

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rajansandeep/teleport-job-worker-svc/internal/worker"
)

// shellPath returns the path to sh, skipping the test when sh is unavailable.
func shellPath(t *testing.T) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	return sh
}

// waitForState polls Status until the job reaches the wanted state or the
// test's deadline (5 s) expires.
func waitForState(t *testing.T, w *worker.Worker, jobID string, want worker.JobState) worker.JobInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	for {
		info, err := w.Status(jobID)
		if err != nil {
			t.Fatalf("Status(%q): %v", jobID, err)
		}
		if info.State == want {
			return info
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for state %s; last=%s", want, info.State)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// waitForWG waits for a WaitGroup to reach zero or the given timeout,
// failing the test if the timeout expires first.
func waitForWG(t *testing.T, wg *sync.WaitGroup, timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()

	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for goroutines: %v", ctx.Err())
	}
}

func TestWorkerStartStatusComplete(t *testing.T) {
	t.Parallel()
	w := worker.NewWorker()
	sh := shellPath(t)

	jobID, err := w.Start(sh, []string{"-c", "echo hello"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	info := waitForState(t, w, jobID, worker.JobStateCompleted)
	if info.ExitCode == nil || *info.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %v", info.ExitCode)
	}
}

func TestWorkerStopMarksStopped(t *testing.T) {
	t.Parallel()
	w := worker.NewWorker()
	sh := shellPath(t)

	jobID, err := w.Start(sh, []string{"-c", "sleep 10"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := w.Stop(jobID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	info := waitForState(t, w, jobID, worker.JobStateStopped)
	if info.FinishedAt.IsZero() {
		t.Fatal("expected FinishedAt to be set")
	}
	if info.ExitCode != nil {
		t.Fatalf("expected no exit code for stopped job, got %d", *info.ExitCode)
	}
}

func TestWorkerLateJoinerStreamGetsFullOutput(t *testing.T) {
	t.Parallel()
	w := worker.NewWorker()
	sh := shellPath(t)

	jobID, err := w.Start(sh, []string{"-c", `for i in 1 2 3 4 5; do echo "line-$i"; sleep 0.05; done`})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Join after some output has already been produced.
	time.Sleep(120 * time.Millisecond)

	r, err := w.StreamOutput(jobID)
	if err != nil {
		t.Fatalf("StreamOutput: %v", err)
	}
	t.Cleanup(func() { r.Close() })

	waitForState(t, w, jobID, worker.JobStateCompleted)

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	out := string(got)
	if !strings.HasPrefix(out, "line-1\n") {
		t.Fatalf("late joiner did not replay from offset 0: got %q", out)
	}
	if !strings.Contains(out, "line-5\n") {
		t.Fatalf("expected final line in output, got %q", out)
	}
}

func TestWorkerConcurrentStopOnlyOneSucceeds(t *testing.T) {
	t.Parallel()
	w := worker.NewWorker()
	sh := shellPath(t)

	jobID, err := w.Start(sh, []string{"-c", "sleep 10"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	const callers = 10
	start := make(chan struct{})
	results := make(chan error, callers)

	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			<-start
			results <- w.Stop(jobID)
		})
	}

	close(start)
	waitForWG(t, &wg, 3*time.Second)
	close(results)

	var successes, notRunning int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, worker.ErrJobNotRunning):
			notRunning++
		default:
			t.Fatalf("unexpected Stop error: %v", err)
		}
	}

	if successes != 1 {
		t.Fatalf("expected exactly 1 successful Stop, got %d", successes)
	}
	if notRunning != callers-1 {
		t.Fatalf("expected %d ErrJobNotRunning, got %d", callers-1, notRunning)
	}

	waitForState(t, w, jobID, worker.JobStateStopped)
}

func TestWorkerStartNonexistentBinary(t *testing.T) {
	t.Parallel()
	w := worker.NewWorker()

	_, err := w.Start("/no/such/bin", nil)
	if err == nil {
		t.Fatal("expected error for nonexistent binary, got nil")
	}
}

// TestWorkerStopNonRunning verifies that Stop returns ErrJobNotRunning when the
// job is no longer in the Running state, regardless of how it stopped.
func TestWorkerStopNonRunning(t *testing.T) {
	t.Parallel()
	sh := shellPath(t)

	tests := []struct {
		name  string
		setup func(t *testing.T, w *worker.Worker) string
	}{
		{
			name: "completed job",
			setup: func(t *testing.T, w *worker.Worker) string {
				t.Helper()
				jobID, err := w.Start(sh, []string{"-c", "echo done"})
				if err != nil {
					t.Fatalf("Start: %v", err)
				}
				waitForState(t, w, jobID, worker.JobStateCompleted)
				return jobID
			},
		},
		{
			name: "already stopped job",
			setup: func(t *testing.T, w *worker.Worker) string {
				t.Helper()
				jobID, err := w.Start(sh, []string{"-c", "sleep 10"})
				if err != nil {
					t.Fatalf("Start: %v", err)
				}
				if err := w.Stop(jobID); err != nil {
					t.Fatalf("first Stop: %v", err)
				}
				waitForState(t, w, jobID, worker.JobStateStopped)
				return jobID
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := worker.NewWorker()
			jobID := tc.setup(t, w)

			err := w.Stop(jobID)
			if !errors.Is(err, worker.ErrJobNotRunning) {
				t.Fatalf("expected ErrJobNotRunning, got %v", err)
			}
		})
	}
}

func TestWorkerStartedAtNonZero(t *testing.T) {
	t.Parallel()
	w := worker.NewWorker()
	sh := shellPath(t)

	jobID, err := w.Start(sh, []string{"-c", "sleep 1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Register cleanup before the first assertion so the sleep is always stopped
	// even if a t.Fatalf fires early.
	t.Cleanup(func() { _ = w.Stop(jobID) })

	info, err := w.Status(jobID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if info.StartedAt.IsZero() {
		t.Fatal("expected StartedAt to be non-zero")
	}
}

func TestWorkerFailedExitCode(t *testing.T) {
	t.Parallel()
	w := worker.NewWorker()
	sh := shellPath(t)

	jobID, err := w.Start(sh, []string{"-c", "exit 1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	info := waitForState(t, w, jobID, worker.JobStateFailed)
	if info.ExitCode == nil {
		t.Fatal("expected non-nil ExitCode")
	}
	if *info.ExitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", *info.ExitCode)
	}
}

func TestWorkerConcurrentStatusDuringStop(t *testing.T) {
	t.Parallel()
	w := worker.NewWorker()
	sh := shellPath(t)

	jobID, err := w.Start(sh, []string{"-c", "sleep 10"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	errCh := make(chan error, 101)
	stopDone := make(chan struct{})

	go func() {
		defer close(stopDone)
		time.Sleep(50 * time.Millisecond)
		errCh <- w.Stop(jobID)
	}()

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()

			for {
				info, err := w.Status(jobID)
				if err != nil {
					errCh <- err
					return
				}
				if info.State == worker.JobStateStopped {
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		})
	}

	waitForWG(t, &wg, 4*time.Second)

	select {
	case <-stopDone:
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for Stop goroutine")
	}

	close(errCh)

	for err := range errCh {
		switch {
		case err == nil, errors.Is(err, worker.ErrJobNotRunning):
			// expected
		case errors.Is(err, worker.ErrJobNotFound):
			t.Fatalf("unexpected ErrJobNotFound during concurrent status/stop: %v", err)
		default:
			t.Fatalf("unexpected error during concurrent status/stop: %v", err)
		}
	}

	waitForState(t, w, jobID, worker.JobStateStopped)
}

func TestWorkerConcurrentStreamDuringStop(t *testing.T) {
	t.Parallel()
	w := worker.NewWorker()
	sh := shellPath(t)

	jobID, err := w.Start(sh, []string{"-c", `i=1; while [ $i -le 100 ]; do echo "line-$i"; i=$((i+1)); sleep 0.02; done`})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	r, err := w.StreamOutput(jobID)
	if err != nil {
		t.Fatalf("StreamOutput: %v", err)
	}
	t.Cleanup(func() { r.Close() })

	// Read the first byte to confirm output is flowing before stopping.
	var first [1]byte
	if _, err := io.ReadFull(r, first[:]); err != nil {
		t.Fatalf("waiting for first byte of output: %v", err)
	}

	if err := w.Stop(jobID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Drain remaining output and prepend the byte we already read.
	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	got := append(first[:], rest...)

	// got is guaranteed non-empty because first[] always contributes one byte.
	// verify we have at least one recognisable line as a sanity check.
	if !strings.Contains(string(got), "line-") {
		t.Fatalf("expected output containing 'line-', got %q", got)
	}

	waitForState(t, w, jobID, worker.JobStateStopped)
}

func TestWorkerConcurrentMultipleStreams(t *testing.T) {
	t.Parallel()
	w := worker.NewWorker()
	sh := shellPath(t)

	jobID, err := w.Start(sh, []string{"-c", `i=1; while [ $i -le 40 ]; do echo "line-$i"; i=$((i+1)); sleep 0.01; done`})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	const readers = 8
	outputs := make([]string, readers)
	errs := make(chan error, readers)

	var wg sync.WaitGroup
	for i := range readers {
		wg.Go(func() {
			r, err := w.StreamOutput(jobID)
			if err != nil {
				errs <- err
				return
			}
			defer r.Close()

			got, err := io.ReadAll(r)
			if err != nil {
				errs <- err
				return
			}
			outputs[i] = string(got)
		})
	}

	waitForWG(t, &wg, 5*time.Second)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("unexpected stream error: %v", err)
		}
	}

	for i := 1; i < readers; i++ {
		if outputs[i] != outputs[0] {
			t.Fatalf("stream %d output mismatch", i)
		}
	}
	if !strings.Contains(outputs[0], "line-1\n") {
		t.Fatalf("expected replay from beginning, got %q", outputs[0])
	}
	if !strings.Contains(outputs[0], "line-40\n") {
		t.Fatalf("expected final line in output, got %q", outputs[0])
	}
}

// TestWorkerErrJobNotFound verifies that Status, Stop, and StreamOutput all
// return ErrJobNotFound for an unknown job ID.
func TestWorkerErrJobNotFound(t *testing.T) {
	t.Parallel()
	w := worker.NewWorker()
	const fakeID = "00000000-0000-0000-0000-000000000000"

	t.Run("Status", func(t *testing.T) {
		t.Parallel()
		_, err := w.Status(fakeID)
		if !errors.Is(err, worker.ErrJobNotFound) {
			t.Fatalf("Status: got %v, want ErrJobNotFound", err)
		}
	})

	t.Run("Stop", func(t *testing.T) {
		t.Parallel()
		err := w.Stop(fakeID)
		if !errors.Is(err, worker.ErrJobNotFound) {
			t.Fatalf("Stop: got %v, want ErrJobNotFound", err)
		}
	})

	t.Run("StreamOutput", func(t *testing.T) {
		t.Parallel()
		_, err := w.StreamOutput(fakeID)
		if !errors.Is(err, worker.ErrJobNotFound) {
			t.Fatalf("StreamOutput: got %v, want ErrJobNotFound", err)
		}
	})
}

// TestWorkerStreamAfterCompletion verifies that a stream opened after a job
// has already completed replays all output from the beginning.
func TestWorkerStreamAfterCompletion(t *testing.T) {
	t.Parallel()
	w := worker.NewWorker()
	sh := shellPath(t)

	jobID, err := w.Start(sh, []string{"-c", `echo "line-1"; echo "line-2"; echo "line-3"`})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the job to finish before opening any stream.
	waitForState(t, w, jobID, worker.JobStateCompleted)

	r, err := w.StreamOutput(jobID)
	if err != nil {
		t.Fatalf("StreamOutput after completion: %v", err)
	}
	t.Cleanup(func() { r.Close() })

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	const want = "line-1\nline-2\nline-3\n"
	if string(got) != want {
		t.Fatalf("got %q, want %q", string(got), want)
	}
}

// TestWorkerProcessGroupKill verifies that Stop kills the entire process group,
// not just the direct child. The test starts a shell that spawns a grandchild
// (sleep 300 &) and confirms the grandchild is dead after Stop returns.
func TestWorkerProcessGroupKill(t *testing.T) {
	t.Parallel()
	w := worker.NewWorker()
	sh := shellPath(t)

	// The shell prints the grandchild PID, then blocks on `wait`, keeping the
	// job in Running state until we stop it.
	jobID, err := w.Start(sh, []string{"-c", "sleep 300 & echo $!; wait"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait until Running so the shell has started the grandchild.
	waitForState(t, w, jobID, worker.JobStateRunning)

	// Open a stream and read the first line to obtain the grandchild's PID.
	r, err := w.StreamOutput(jobID)
	if err != nil {
		t.Fatalf("StreamOutput: %v", err)
	}
	sc := bufio.NewScanner(r)
	if !sc.Scan() {
		r.Close()
		t.Fatalf("expected PID line from stream: %v", sc.Err())
	}
	pidStr := strings.TrimSpace(sc.Text())
	r.Close()

	grandchildPID, err := strconv.Atoi(pidStr)
	if err != nil {
		t.Fatalf("parse grandchild PID %q: %v", pidStr, err)
	}

	// Sanity-check: the grandchild must be alive before we stop.
	if err := syscall.Kill(grandchildPID, 0); err != nil {
		t.Fatalf("grandchild PID %d not alive before Stop: %v", grandchildPID, err)
	}

	// Stop sends SIGKILL to the whole process group.
	if err := w.Stop(jobID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	waitForState(t, w, jobID, worker.JobStateStopped)

	// Note: there is a theoretical PID-reuse race: after Stop, the OS could
	// recycle grandchildPID for a new unrelated process before we poll here.
	// On a lightly-loaded machine this is vanishingly unlikely, and there is no
	// portable way to guard against it without additional infrastructure.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	for {
		err := syscall.Kill(grandchildPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return // grandchild is gone
		}
		select {
		case <-ctx.Done():
			t.Fatalf("grandchild PID %d still exists after Stop: last kill(0) err = %v", grandchildPID, err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
