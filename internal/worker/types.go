package worker

import (
	"fmt"
	"time"
)

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
	ID       string
	Command  string
	Args     []string
	State    JobState
	ExitCode *int
	// ErrMsg contains diagnostic information from the underlying Wait call.
	// A non-empty ErrMsg does not imply failure.
	ErrMsg     string
	StartedAt  time.Time
	FinishedAt time.Time
}
