package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// Worker manages a collection of running jobs.
// Jobs are retained in memory for the lifetime of the Worker; there is no
// Remove API at this level.
//
// TODO: add a Remove/Purge method to release memory for completed jobs.
type Worker struct {
	mu   sync.RWMutex
	jobs map[string]*job
}

type job struct {
	mu sync.RWMutex

	id         string
	command    string
	args       []string
	state      JobState
	exitCode   *int
	errMsg     string
	startedAt  time.Time
	finishedAt time.Time

	buffer *outputBuffer
	// cmd is set once by Start before the job is inserted into Worker.jobs
	// and never reassigned. It is safe to read without j.mu after that point.
	cmd *exec.Cmd

	// Prevent duplicate concurrent Stop calls from racing.
	stopRequested bool
}

func (j *job) toInfo() JobInfo {
	j.mu.RLock()
	defer j.mu.RUnlock()

	var exitCode *int
	if j.exitCode != nil {
		v := *j.exitCode
		exitCode = &v
	}

	return JobInfo{
		ID:         j.id,
		Command:    j.command,
		Args:       slices.Clone(j.args),
		State:      j.state,
		ExitCode:   exitCode,
		ErrMsg:     j.errMsg,
		StartedAt:  j.startedAt,
		FinishedAt: j.finishedAt,
	}
}

// NewWorker creates a new worker ready to accept jobs.
func NewWorker() *Worker {
	return &Worker{
		jobs: make(map[string]*job),
	}
}

// Start launches cmd with args as a new background job and returns its ID.
// The ID can be used with Stop, Status, and StreamOutput.
//
// The context argument is accepted but is not used to
// govern the launched process's lifetime. A cancelled context after Start
// returns has no effect on the running process. Use Stop to terminate a job.
//
// TODO: accept a per-job timeout or context to bound process lifetime
// independently of any RPC context.
func (w *Worker) Start(_ context.Context, cmd string, args []string) (string, error) {
	if strings.TrimSpace(cmd) == "" {
		return "", ErrInvalidCommand
	}

	jobID, err := newJobID()
	if err != nil {
		return "", err
	}

	buffer := newOutputBuffer()

	command := exec.Command(cmd, args...)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = 5 * time.Second
	command.Stdout = buffer
	command.Stderr = buffer

	if err := command.Start(); err != nil {
		return "", err
	}

	startedAt := time.Now()

	j := &job{
		id:        jobID,
		command:   cmd,
		args:      slices.Clone(args),
		state:     JobStateRunning,
		startedAt: startedAt,
		buffer:    buffer,
		cmd:       command,
	}

	w.mu.Lock()
	w.jobs[jobID] = j
	w.mu.Unlock()

	go w.waiter(j)

	return jobID, nil
}

// Stop sends SIGKILL to the process group of the given job and returns
// immediately. The job transitions to JobStateStopped asynchronously once
// the OS confirms the process has exited. Poll Status to observe the
// transition.
//
// Returns ErrJobNotFound if jobID does not exist, or ErrJobNotRunning if
// the job is not in the Running state.
func (w *Worker) Stop(_ context.Context, jobID string) error {
	j, err := w.getJob(jobID)
	if err != nil {
		return err
	}

	j.mu.Lock()
	if j.state != JobStateRunning || j.stopRequested {
		j.mu.Unlock()
		return ErrJobNotRunning
	}

	j.stopRequested = true
	pid := j.cmd.Process.Pid
	j.mu.Unlock()

	err = syscall.Kill(-pid, syscall.SIGKILL)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.ESRCH):
		// The process (or process group) is already gone. Clear stopRequested so
		// the waiter classifies the terminal state based on the actual exit.
		j.mu.Lock()
		j.stopRequested = false
		j.mu.Unlock()
		return ErrJobNotRunning
	default:
		j.mu.Lock()
		j.stopRequested = false
		j.mu.Unlock()
		return err
	}
}

// Status returns a point-in-time snapshot of the job's state and metadata.
// Returns ErrJobNotFound if jobID does not exist.
func (w *Worker) Status(_ context.Context, jobID string) (JobInfo, error) {
	j, err := w.getJob(jobID)
	if err != nil {
		return JobInfo{}, err
	}

	return j.toInfo(), nil
}

// StreamOutput returns an io.ReadCloser that replays the job's combined
// stdout+stderr from byte 0. Late callers receive a full replay of all output
// produced since the job started. Read blocks until new output is available,
// the job exits (io.EOF), or Close is called.
//
// Context cancellation automatically unblocks any in-flight Read and closes
// the returned reader. The caller must still call Close when done to release
// resources; doing so cancels any pending context callback so no goroutine
// outlives the reader.
//
// Returns ErrJobNotFound if jobID does not exist.
func (w *Worker) StreamOutput(ctx context.Context, jobID string) (io.ReadCloser, error) {
	j, err := w.getJob(jobID)
	if err != nil {
		return nil, err
	}

	rc := j.buffer.NewReader()
	stop := context.AfterFunc(ctx, func() {
		_ = rc.Close()
	})

	return &cancelOnCloseReader{ReadCloser: rc, stop: stop}, nil
}

func (w *Worker) waiter(j *job) {
	err := j.cmd.Wait()
	finishedAt := time.Now()

	j.mu.Lock()

	j.finishedAt = finishedAt

	if j.stopRequested {
		j.state = JobStateStopped
		j.exitCode = nil
		j.mu.Unlock()
		j.buffer.Close()
		return
	}

	exitCode := 0
	if j.cmd.ProcessState != nil {
		exitCode = j.cmd.ProcessState.ExitCode()
	}
	j.exitCode = &exitCode

	switch {
	case exitCode != 0:
		j.state = JobStateStopped
	case errors.Is(err, exec.ErrWaitDelay):
		j.state = JobStateCompleted
	case err != nil:
		j.state = JobStateFailed
	default:
		j.state = JobStateCompleted
	}

	// errMsg records Wait's error for diagnostics even when state is
	// Completed (for example, exec.ErrWaitDelay with exit code 0 means the
	// process exited cleanly but child I/O outlasted WaitDelay).
	j.errMsg = ""
	if err != nil {
		j.errMsg = err.Error()
	}

	j.mu.Unlock()
	j.buffer.Close()
}

func (w *Worker) getJob(jobID string) (*job, error) {
	w.mu.RLock()
	j := w.jobs[jobID]
	w.mu.RUnlock()

	if j == nil {
		return nil, ErrJobNotFound
	}
	return j, nil
}

// cancelOnCloseReader wraps an io.ReadCloser and cancels a pending
// context.AfterFunc callback when the reader is explicitly closed before
// ctx is done, preventing the AfterFunc goroutine from outliving the reader.
type cancelOnCloseReader struct {
	io.ReadCloser
	stop func() bool // stops the pending context.AfterFunc callback
}

func (c *cancelOnCloseReader) Close() error {
	c.stop()
	return c.ReadCloser.Close()
}

func newJobID() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("unable to generate job ID: %w", err)
	}

	return id.String(), nil
}
