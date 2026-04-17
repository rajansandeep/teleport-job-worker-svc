package worker

import (
	"fmt"
	"time"
)

// JobState represents the lifecycle state of a job.
type JobState int

const (
	// JobStateUnspecified is the zero value. Should not appear in normal use.
	JobStateUnspecified JobState = iota
	// JobStateRunning means the process is active.
	JobStateRunning
	// JobStateCompleted means the process exited with code 0.
	JobStateCompleted
	// JobStateFailed means the process exited with a non-zero code or an unexpected error occurred.
	JobStateFailed
	// JobStateStopped means Stop was called and the process was killed.
	JobStateStopped
)

func (s JobState) String() string {
	switch s {
	case JobStateUnspecified:
		return "UNSPECIFIED"
	case JobStateRunning:
		return "RUNNING"
	case JobStateCompleted:
		return "COMPLETED"
	case JobStateFailed:
		return "FAILED"
	case JobStateStopped:
		return "STOPPED"
	default:
		return fmt.Sprintf("UNKNOWN[%d]", s)
	}
}

// JobInfo is a snapshot of a job's state, returned by Status.
type JobInfo struct {
	// ID is the unique identifier assigned when the job was started.
	ID string
	// Command is the executable path passed to Start.
	Command string
	// Args are the arguments passed to Start.
	Args []string
	// State is the current lifecycle state of the job.
	State JobState
	// ExitCode is the process exit code. Nil while the job is running or if
	// it was stopped via Stop rather than exiting naturally.
	ExitCode *int
	// ErrMsg contains diagnostic information from the underlying Wait call.
	// A non-empty ErrMsg does not imply failure.
	ErrMsg string
	// StartedAt is the time the process was launched.
	StartedAt time.Time
	// FinishedAt is the time the process exited. Zero while the job is running.
	FinishedAt time.Time
}
