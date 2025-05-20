package library

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Test artifacts are created under /tmp/jobworker/ with unique job IDs.
// Each test job creates a directory structure like:
//   /tmp/jobworker/<job-id>/output.log
// These files are not automatically cleaned up between test runs.

func checkCgroupLimit(t *testing.T, cgroupPath, limitFile, expectedValue string) {
	content, err := os.ReadFile(filepath.Join(cgroupPath, limitFile))
	if err != nil {
		t.Errorf("Failed to read %s: %v", limitFile, err)
		return
	}

	// Trim any whitespace and newlines from both values
	actual := strings.TrimSpace(string(content))
	expected := strings.TrimSpace(expectedValue)

	if actual != expected {
		t.Errorf("%s = %q, want %q", limitFile, actual, expected)
	}
}

func TestNewJobManager(t *testing.T) {
	// Test that NewJobManager returns a non-nil JobManager
	manager := NewJobManager()
	if manager == nil {
		t.Error("NewJobManager returned nil")
	}

	// Test that the parent cgroup was created
	if _, err := os.Stat(parentCgroupPath); os.IsNotExist(err) {
		t.Errorf("Parent cgroup directory %s was not created", parentCgroupPath)
	}

	// Test that the required controllers were enabled
	controllers, err := os.ReadFile(filepath.Join(parentCgroupPath, cgroupSubtreeControl))
	if err != nil {
		t.Errorf("Failed to read cgroup.subtree_control: %v", err)
	}

	if !strings.Contains(string(controllers), "cpu") ||
		!strings.Contains(string(controllers), "memory") ||
		!strings.Contains(string(controllers), "io") {
		t.Error("Required controllers (cpu, memory, io) were not enabled")
	}
}

func TestJobManager_StartJob(t *testing.T) {
	tests := []struct {
		name    string
		command string
		args    []string
		wantErr bool
	}{
		{
			name:    "simple command",
			command: "echo",
			args:    []string{"hello"},
			wantErr: false,
		},
		{
			name:    "command with output",
			command: "yes",
			args:    []string{"test"},
			wantErr: false,
		},
		{
			name:    "nonexistent command",
			command: "nonexistentcommand",
			args:    []string{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewJobManager()
			ctx := context.Background()

			job, err := m.StartJob(ctx, tt.command, tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("StartJob() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				// Verify job directory exists
				jobDir := filepath.Join("/tmp/jobworker", job.ID)
				if _, err := os.Stat(jobDir); os.IsNotExist(err) {
					t.Errorf("job directory %s does not exist", jobDir)
				}

				// Verify output file exists
				outputFile := filepath.Join(jobDir, "output.log")
				if _, err := os.Stat(outputFile); os.IsNotExist(err) {
					t.Errorf("output file %s does not exist", outputFile)
				}

				// Verify cgroup exists and has correct limits
				cgroupPath := filepath.Join("/sys/fs/cgroup/jobworker.slice", "job-"+job.ID+".scope")
				if _, err := os.Stat(cgroupPath); os.IsNotExist(err) {
					t.Errorf("cgroup directory %s does not exist", cgroupPath)
				}

				// Check required cgroup limits
				checkCgroupLimit(t, cgroupPath, "cpu.max", cpuMax)
				checkCgroupLimit(t, cgroupPath, "memory.high", memoryHigh)

				// Check IO weight only if available
				if isIOWeightAvailable(cgroupPath) {
					checkCgroupLimit(t, cgroupPath, "io.weight", ioWeight)
				} else {
					t.Log("io.weight is not available, skipping IO weight check")
				}

				// Cleanup
				m.StopJob(ctx, job.ID)
			}
		})
	}
}

func TestJobManager_StopJob(t *testing.T) {
	m := NewJobManager()
	ctx := context.Background()

	// Start a long-running job
	job, err := m.StartJob(ctx, "sleep", []string{"100"})
	if err != nil {
		t.Fatalf("Failed to start job: %v", err)
	}

	// Stop the job
	err = m.StopJob(ctx, job.ID)
	if err != nil {
		t.Errorf("StopJob() error = %v", err)
	}

	// Verify job is stopped but still accessible
	stoppedJob, err := m.GetJobStatus(ctx, job.ID)
	if err != nil {
		t.Errorf("GetJobStatus() error = %v", err)
	}
	if stoppedJob.status != StatusStopped {
		t.Errorf("GetJobStatus() status = %v, want %v", stoppedJob.status, StatusStopped)
	}

	// Verify cgroup is removed
	cgroupPath := filepath.Join("/sys/fs/cgroup/jobworker.slice", "job-"+job.ID+".scope")
	if _, err := os.Stat(cgroupPath); !os.IsNotExist(err) {
		t.Errorf("cgroup directory %s still exists", cgroupPath)
	}
}

func TestJobManager_OutputLogging(t *testing.T) {
	m := NewJobManager()
	ctx := context.Background()

	// Start a job that will produce output
	expectedOutput := "test output"
	job, err := m.StartJob(ctx, "echo", []string{expectedOutput})
	if err != nil {
		t.Fatalf("Failed to start job: %v", err)
	}

	// Wait a short time for the job to complete
	time.Sleep(100 * time.Millisecond)

	// Verify output file exists and contains expected content
	outputPath := filepath.Join("/tmp/jobworker", job.ID, "output.log")
	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Errorf("Failed to read output file: %v", err)
	}

	// Check that the output contains our expected string
	// Note: echo adds a newline, so we trim it for comparison
	if strings.TrimSpace(string(content)) != expectedOutput {
		t.Errorf("Output file content = %q, want %q", strings.TrimSpace(string(content)), expectedOutput)
	}

	// Clean up
	if err := m.StopJob(ctx, job.ID); err != nil {
		t.Errorf("Failed to stop job: %v", err)
	}
}

func TestJobManager_StreamOutput(t *testing.T) {
	m := NewJobManager()
	ctx := context.Background()

	// Create a script that will output in two phases
	script := `#!/bin/bash
echo "first output"
sleep 0.5
echo "second output"
`
	scriptPath := filepath.Join(os.TempDir(), "test_script.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("Failed to create test script: %v", err)
	}
	defer os.Remove(scriptPath)

	// Start the job
	job, err := m.StartJob(ctx, scriptPath, nil)
	if err != nil {
		t.Fatalf("Failed to start job: %v", err)
	}

	outputChan, err := m.StreamOutput(ctx, job.ID)
	if err != nil {
		t.Fatalf("Failed to start streaming: %v", err)
	}

	// Collect all output
	var output []string
	for data := range outputChan {
		output = append(output, strings.TrimSpace(string(data)))
	}

	// Clean up
	if err := m.StopJob(ctx, job.ID); err != nil {
		t.Errorf("Failed to stop job: %v", err)
	}

	// Verify we got both outputs in the correct order
	if len(output) != 2 {
		t.Fatalf("Got %d outputs, want 2", len(output))
	}
	if output[0] != "first output" {
		t.Errorf("First output = %q, want %q", output[0], "first output")
	}
	if output[1] != "second output" {
		t.Errorf("Second output = %q, want %q", output[1], "second output")
	}
}

func TestJobManager_StopJobTwice(t *testing.T) {
	m := NewJobManager()
	ctx := context.Background()

	// Start a long-running job
	job, err := m.StartJob(ctx, "sleep", []string{"100"})
	if err != nil {
		t.Fatalf("Failed to start job: %v", err)
	}

	// First stop should succeed
	if err := m.StopJob(ctx, job.ID); err != nil {
		t.Errorf("First StopJob() error = %v", err)
	}

	// Second stop should also succeed (idempotent)
	if err := m.StopJob(ctx, job.ID); err != nil {
		t.Errorf("Second StopJob() error = %v", err)
	}

	// Try to stop a non-existent job
	nonExistentID := "non-existent-id"
	err = m.StopJob(ctx, nonExistentID)
	if err == nil {
		t.Error("StopJob() on non-existent job expected error, got nil")
		return
	}

	// Check the status code
	jobErr := err.(*JobManagerError)
	if jobErr.Code != StatusNotFound {
		t.Errorf("StopJob() on non-existent job returned wrong status code: %d, want %d", jobErr.Code, StatusNotFound)
	}
}
