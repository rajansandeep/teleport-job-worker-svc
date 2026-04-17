package worker

import (
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
// TODO: Add Close/Shutdown so callers can gracefully stop running jobs,
// reject new ones, and release retained in-memory state.
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

// NewWorker creates a new Worker ready to accept jobs.
func NewWorker() *Worker {
	return &Worker{
		jobs: make(map[string]*job),
	}
}

// Start launches cmd with args as a new background job and returns its ID.
// The ID can be used with Stop, Status, and StreamOutput.
//
// TODO: accept a per-job timeout or context to bound process lifetime
// independently of any RPC context.
func (w *Worker) Start(cmd string, args []string) (string, error) {
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
// the job is not in the Running state. If the OS process has already exited
// but the job record has not yet been updated by the background waiter,
// ErrJobNotRunning is also returned.
func (w *Worker) Stop(jobID string) error {
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

	// Hold the lock across Kill so that waiter cannot observe stopRequested=true
	// and read ProcessState before the signal is actually delivered, which would
	// cause it to misclassify a natural exit as JobStateStopped.
	err = syscall.Kill(-pid, syscall.SIGKILL)
	switch {
	case err == nil:
		j.mu.Unlock()
		return nil
	case errors.Is(err, syscall.ESRCH):
		// The process (or process group) is already gone. Clear stopRequested so
		// the waiter classifies the terminal state based on the actual exit.
		j.stopRequested = false
		j.mu.Unlock()
		return ErrJobNotRunning
	default:
		j.stopRequested = false
		j.mu.Unlock()
		return err
	}
}

// Status returns a point-in-time snapshot of the job's state and metadata.
// Returns ErrJobNotFound if jobID does not exist.
func (w *Worker) Status(jobID string) (JobInfo, error) {
	j, err := w.getJob(jobID)
	if err != nil {
		return JobInfo{}, err
	}

	return j.toInfo(), nil
}

// StreamOutput returns an io.ReadCloser that replays the job's combined
// stdout+stderr from byte 0. Late callers receive a full replay of all output
// produced since the job started. Read blocks until new output is available
// or the job exits, at which point it returns io.EOF. If the reader is closed
// before the job finishes, Read returns io.ErrClosedPipe.
//
// The caller owns the reader's lifetime and must call Close when done.
//
// Returns ErrJobNotFound if jobID does not exist.
func (w *Worker) StreamOutput(jobID string) (io.ReadCloser, error) {
	j, err := w.getJob(jobID)
	if err != nil {
		return nil, err
	}

	return j.buffer.NewReader(), nil
}

func (w *Worker) waiter(j *job) {
	err := j.cmd.Wait()
	finishedAt := time.Now()

	j.mu.Lock()
	defer j.buffer.Close()
	defer j.mu.Unlock()

	j.finishedAt = finishedAt

	signaledByStop := false
	if ps := j.cmd.ProcessState; ps != nil {
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL {
			signaledByStop = true
		}
	}

	if j.stopRequested && signaledByStop {
		j.state = JobStateStopped
		j.exitCode = nil
		j.errMsg = ""
		return
	}

	if j.cmd.ProcessState == nil {
		// ProcessState is nil when Wait returns an error before the process
		// produced any exit status (e.g. OS resource exhaustion after Start).
		j.state = JobStateFailed
		j.exitCode = nil
		j.errMsg = ""
		if err != nil {
			j.errMsg = err.Error()
		}
		return
	}

	exitCode := j.cmd.ProcessState.ExitCode()
	j.exitCode = &exitCode

	switch {
	case exitCode != 0:
		j.state = JobStateFailed
	case errors.Is(err, exec.ErrWaitDelay) && exitCode == 0:
		// Process exited cleanly but a child it spawned kept the pipe open past
		// WaitDelay. Treat as completed; errMsg records the detail.
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

func newJobID() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("unable to generate job ID: %w", err)
	}

	return id.String(), nil
}
