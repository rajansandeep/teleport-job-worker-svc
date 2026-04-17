package worker

import "errors"

var (
	// ErrJobNotFound is returned when no job exists for the given ID.
	ErrJobNotFound = errors.New("job not found")
	// ErrJobNotRunning is returned by Stop when the job is not in the Running state.
	ErrJobNotRunning = errors.New("job is not running")
	// ErrInvalidCommand is returned by Start when the command string is empty or blank.
	ErrInvalidCommand = errors.New("invalid command")
)
