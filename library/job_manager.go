package library

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/google/uuid"
)

// Hardcoded cgroup limits
const (
	// CPU: 25% of a single CPU core
	cpuMax = "25000 100000"
	// Memory: 256MB (268435456 bytes)
	memoryHigh = "268435456"
	// IO: Very low weight to throttle disk I/O
	ioWeight = "default 10"
)

// Path constants
const (
	baseCgroupPath = "/sys/fs/cgroup/"
	parentCgroup   = "jobworker.slice/"
	jobScope       = "job-%s.scope"
	jobBaseDir     = "/tmp/jobworker/%s"
)

var (
	parentCgroupPath = baseCgroupPath + parentCgroup
	jobScopePath     = baseCgroupPath + parentCgroup + jobScope
)

const (
	cgroupControllers    = "+cpu +io +memory" // Cgroup controllers to enable
	cgroupSubtreeControl = "cgroup.subtree_control"
	cgroupKill           = "cgroup.kill"
)

const outputBufferSize = 4096

type jobManager struct {
	jobs        map[string]*Job
	stoppedJobs map[string]*Job
	jobsMux     sync.RWMutex
}

// checkPrerequisites verifies system requirements for the job worker
func checkPrerequisites() error {
	// Check root access
	if os.Geteuid() != 0 {
		return fmt.Errorf("job worker requires root privileges")
	}

	// Initialize cgroup setup
	if err := setupCgroups(); err != nil {
		return fmt.Errorf("failed to initialize cgroup setup: %w", err)
	}

	return nil
}

func setupCgroups() error {
	// Check if cgroupsv2 is mounted
	mounts, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return fmt.Errorf("failed to read cgroups mounts: %w", err)
	}
	if !strings.Contains(string(mounts), "cgroup2") {
		return fmt.Errorf("cgroups2 is not available in the system")
	}

	if err := os.MkdirAll(parentCgroupPath, 0755); err != nil {
		return fmt.Errorf("failed to create parent cgroup directory: %w", err)
	}

	// Enable required controllers in the parent cgroup
	if err := os.WriteFile(filepath.Join(parentCgroupPath, cgroupSubtreeControl), []byte(cgroupControllers), 0644); err != nil {
		return fmt.Errorf("failed to enable cgroup controllers: %w", err)
	}
	return nil
}

// isIOWeightAvailable checks if io.weight is available (requires `cfq` or `bfq` scheduler)
func isIOWeightAvailable(cgroupPath string) bool {
	ioWeightPath := filepath.Join(cgroupPath, "io.weight")
	_, err := os.Stat(ioWeightPath)
	return err == nil
}

// setCgroupLimit sets a cgroup limit
func setCgroupLimit(cgroupPath, limitFile string, value []byte) error {
	if err := os.WriteFile(filepath.Join(cgroupPath, limitFile), value, 0644); err != nil {
		return fmt.Errorf("failed to set %s limit: %w", limitFile, err)
	}
	return nil
}

// cleanupResources handles cleanup of all resources associated with a job
func cleanupResources(outputFile *os.File, jobDir, cgroupPath string) {
	if outputFile != nil {
		outputFile.Close()
	}
	if jobDir != "" {
		os.RemoveAll(jobDir)
	}
	if cgroupPath != "" {
		os.RemoveAll(cgroupPath)
	}
}

func NewJobManager() JobManager {
	// Check system prerequisites
	if err := checkPrerequisites(); err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	// This check is non-blocking - the service will continue to run even if IO weight
	// is unavailable. IO limits will simply not be enforced in this case.
	if !isIOWeightAvailable(parentCgroupPath) {
		fmt.Printf("Warning: io.weight is unavailable, IO limits will not be enforced. Consider enabling `cfq` or `bfq` scheduler.\n")
	}

	return &jobManager{
		jobs:        make(map[string]*Job),
		stoppedJobs: make(map[string]*Job),
	}
}

// setupJobDirectories creates the job directory and output file
func (m *jobManager) setupJobDirectories(id string) (*os.File, string, error) {
	// Create job output directory
	jobDir := fmt.Sprintf(jobBaseDir, id)
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		return nil, "", &JobManagerError{
			Code:    StatusInternal,
			Message: fmt.Sprintf("failed to create job directory: %v", err),
		}
	}

	// Create output file in the job directory
	outputFile, err := os.Create(filepath.Join(jobDir, "output.log"))
	if err != nil {
		os.RemoveAll(jobDir)
		return nil, "", &JobManagerError{
			Code:    StatusInternal,
			Message: fmt.Sprintf("failed to create output file: %v", err),
		}
	}

	return outputFile, jobDir, nil
}

// setupCgroup creates and configures the cgroup for a job
func (m *jobManager) setupCgroup(id string) (string, error) {
	cgroupPath := fmt.Sprintf(jobScopePath, id)
	if err := os.MkdirAll(cgroupPath, 0755); err != nil {
		return "", &JobManagerError{
			Code:    StatusInternal,
			Message: fmt.Sprintf("failed to create cgroup: %v", err),
		}
	}

	// Set resource limits
	if err := setCgroupLimit(cgroupPath, "cpu.max", []byte(cpuMax)); err != nil {
		return "", err
	}
	if err := setCgroupLimit(cgroupPath, "memory.high", []byte(memoryHigh)); err != nil {
		return "", err
	}

	// Set IO weight if available
	if isIOWeightAvailable(cgroupPath) {
		if err := setCgroupLimit(cgroupPath, "io.weight", []byte(ioWeight)); err != nil {
			return "", err
		}
	}

	return cgroupPath, nil
}

// startProcess starts the process in its cgroup
func (m *jobManager) startProcess(cmd *exec.Cmd, cgroupPath string) error {
	// Get cgroupFD for the process
	cgroupFD, err := syscall.Open(cgroupPath, syscall.O_RDONLY, 0)
	if err != nil {
		return &JobManagerError{
			Code:    StatusInternal,
			Message: fmt.Sprintf("failed to open cgroup directory: %v", err),
		}
	}
	defer syscall.Close(cgroupFD)

	// Set process to start in the cgroup
	cmd.SysProcAttr = &syscall.SysProcAttr{
		UseCgroupFD: true,
		CgroupFD:    cgroupFD,
	}

	if err := cmd.Start(); err != nil {
		return &JobManagerError{
			Code:    StatusInternal,
			Message: fmt.Sprintf("failed to start process: %v", err),
		}
	}

	return nil
}

// monitorJob monitors the job's completion and handles cleanup
func (m *jobManager) monitorJob(job *Job, cgroupPath string) {
	job.cmd.Wait()
	m.jobsMux.Lock()
	defer m.jobsMux.Unlock()

	if job, exists := m.jobs[job.ID]; exists {
		// Update job status based on exit code
		exitCode := job.cmd.ProcessState.ExitCode()
		if exitCode == 0 {
			job.status = StatusCompleted
		} else if exitCode == -1 {
			// For exit code -1, we need to check if the process was actually started
			if job.cmd.ProcessState != nil && job.cmd.ProcessState.Pid() > 0 {
				// Process was started and terminated by a signal
				job.status = StatusStopped
			} else {
				// Process never started
				job.status = StatusFailed
			}
		} else {
			job.status = StatusFailed
		}
		job.hasExited = true

		// Broadcast to wake up any waiting readers
		job.dataAvailable.Broadcast()

		// Set cgroup.kill to 1 to clean up stray processes
		if err := os.WriteFile(filepath.Join(cgroupPath, cgroupKill), []byte("1"), 0644); err != nil {
			fmt.Printf("warning: failed to set cgroup.kill for job %s: %v\n", job.ID, err)
		}

		// Attempt to remove the cgroup with no retry, for simplicity
		if err := os.RemoveAll(cgroupPath); err != nil {
			fmt.Printf("warning: failed to remove cgroup for job %s: %v\n", job.ID, err)
		}
	}
}

// StartJob implements JobManager.StartJob
func (m *jobManager) StartJob(ctx context.Context, command string, args []string) (*Job, error) {
	// Generate a unique ID for the job
	id := uuid.NewString()

	// Set up cleanup for any failures during job creation
	success := false
	var outputFile *os.File
	var jobDir string
	var cgroupPath string

	defer func() {
		if !success {
			cleanupResources(outputFile, jobDir, cgroupPath)
		}
	}()

	// Create job directories and output file
	outputFile, jobDir, err := m.setupJobDirectories(id)
	if err != nil {
		return nil, err
	}

	// Set up cgroup
	cgroupPath, err = m.setupCgroup(id)
	if err != nil {
		return nil, err
	}

	// Create and start process
	cmd := exec.CommandContext(ctx, command, args...)
	writer := newJobWriter(outputFile, nil) // Will be set after job creation
	cmd.Stdout = writer
	cmd.Stderr = writer

	// Create job
	job := &Job{
		ID:        id,
		cmd:       cmd,
		status:    StatusRunning,
		hasExited: false,
		mu:        sync.Mutex{},
		output: &JobOutput{
			mu:      sync.RWMutex{},
			file:    outputFile,
			baseDir: jobDir,
			jobID:   id,
		},
	}
	job.dataAvailable = sync.NewCond(&job.mu)
	writer.cond = job.dataAvailable

	// Start process
	if err := m.startProcess(cmd, cgroupPath); err != nil {
		return nil, err
	}

	// Register job
	m.jobsMux.Lock()
	m.jobs[id] = job
	m.jobsMux.Unlock()

	// Mark job creation as successful to disable automatic cleanup
	success = true

	// Start monitoring
	go m.monitorJob(job, cgroupPath)

	return job, nil
}

// StopJob implements JobManager.StopJob
func (m *jobManager) StopJob(ctx context.Context, jobID string) error {
	// First check if job exists
	m.jobsMux.RLock()
	job, exists := m.jobs[jobID]
	if !exists {
		m.jobsMux.RUnlock()
		return &JobManagerError{
			Code:    StatusNotFound,
			Message: fmt.Sprintf("job %s not found", jobID),
		}
	}
	m.jobsMux.RUnlock()

	// If job is already stopped, return success
	if job.status != StatusRunning {
		return nil
	}

	// Set cgroup.kill to 1 to kill all processes in the cgroup
	cgroupPath := fmt.Sprintf(jobScopePath, jobID)
	if err := os.WriteFile(filepath.Join(cgroupPath, cgroupKill), []byte("1"), 0644); err != nil {
		return &JobManagerError{
			Code:    StatusInternal,
			Message: fmt.Sprintf("failed to kill cgroup: %v", err),
		}
	}

	// Attempt to remove the cgroup with no retry, as per design
	if err := os.RemoveAll(cgroupPath); err != nil {
		fmt.Printf("warning: failed to remove cgroup %s: %v\n", cgroupPath, err)
	}

	// Only lock when modifying the jobs map
	m.jobsMux.Lock()
	job.status = StatusStopped
	m.jobsMux.Unlock()

	return nil
}

// GetJobStatus implements JobManager.GetJobStatus
func (m *jobManager) GetJobStatus(ctx context.Context, jobID string) (*Job, error) {
	m.jobsMux.RLock()
	defer m.jobsMux.RUnlock()

	if job, exists := m.jobs[jobID]; exists {
		return job, nil
	}

	if job, exists := m.stoppedJobs[jobID]; exists {
		return job, nil
	}

	return nil, &JobManagerError{
		Code:    StatusNotFound,
		Message: fmt.Sprintf("job %s not found", jobID),
	}
}

// StreamOutput implements JobManager.StreamOutput
func (m *jobManager) StreamOutput(ctx context.Context, jobID string) (<-chan []byte, error) {
	m.jobsMux.RLock()
	job, exists := m.jobs[jobID]
	m.jobsMux.RUnlock()

	if !exists {
		return nil, &JobManagerError{
			Code:    StatusNotFound,
			Message: fmt.Sprintf("job %s not found", jobID),
		}
	}

	// Create output channel
	outputChan := make(chan []byte)

	// Start reading from the beginning of the file
	go func() {
		defer close(outputChan)

		// Open file in read-only mode
		file, err := os.OpenFile(job.output.file.Name(), os.O_RDONLY, 0)
		if err != nil {
			return
		}
		defer file.Close()

		// Create output reader
		reader := &OutputReader{
			file: file,
			pos:  0,
			job:  job,
		}

		// Read from the beginning
		buf := make([]byte, outputBufferSize)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				// Seek to our last position
				if _, err := reader.file.Seek(reader.pos, io.SeekStart); err != nil {
					return
				}

				nBytes, err := reader.file.Read(buf)
				if err != nil {
					if err == io.EOF {
						// Check if process has terminated
						if reader.job.hasExited {
							return
						}
						// Process is still running, wait for more data
						reader.job.dataAvailable.L.Lock()
						reader.job.dataAvailable.Wait()
						reader.job.dataAvailable.L.Unlock()
						continue
					}
					return
				}

				// Update our position
				reader.pos += int64(nBytes)

				// Send data to channel
				select {
				case outputChan <- buf[:nBytes]:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return outputChan, nil
}
