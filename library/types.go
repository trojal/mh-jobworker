package library

import (
	"context"
	"os"
	"os/exec"
	"sync"
)

// JobStatus represents the current state of a job
type JobStatus int

const (
	StatusUnknown JobStatus = iota
	StatusRunning
	StatusCompleted
	StatusFailed
	StatusStopped
)

// Common gRPC status codes
const (
	StatusOK            = 0
	StatusNotFound      = 5
	StatusFailedPrecond = 9
	StatusInternal      = 13
)

// JobManagerError represents an error returned by the job manager
type JobManagerError struct {
	Code    int    // gRPC status code
	Message string // Error message
}

func (e *JobManagerError) Error() string {
	return e.Message
}

// Job represents a running process/job and associated metadata
type Job struct {
	ID        string
	cmd       *exec.Cmd
	status    JobStatus
	hasExited bool

	// Synchronization
	mu            sync.Mutex
	dataAvailable *sync.Cond

	// Job output
	output *JobOutput
}

// JobOutput manages the output file and synchronization
type JobOutput struct {
	mu      sync.RWMutex
	file    *os.File
	baseDir string
	jobID   string
}

// OutputReader handles reading from a job's output file
type OutputReader struct {
	file *os.File
	pos  int64
	job  *Job // Reference to the job
}

// JobManager provides methods to manage jobs
type JobManager interface {
	StartJob(ctx context.Context, cmd string, args []string) (*Job, error)
	StopJob(ctx context.Context, jobID string) error
	GetJobStatus(ctx context.Context, jobID string) (*Job, error)
	StreamOutput(ctx context.Context, jobID string) (<-chan []byte, error)
}
