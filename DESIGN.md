---
author: Marc Hawkinger (trojal@gmail.com)
---

# Design Document: Job Worker Service

## What

This document outlines the design for a job worker service that provides an API to run arbitrary Linux processes. The service consists of three components: a job worker library, a gRPC API server, and a CLI client. The design prioritizes fulfilling the core requirements of starting, stopping, querying, and streaming output from jobs, with a focus on security and resource control. Trade-offs are made to minimize complexity and development time, as suggested in the problem description.

## Scope

The design prioritizes simplicity and correctness over scalability and production-readiness. We'll make several trade-offs to keep the implementation focused and manageable, addressed in [Trade-offs and Future Improvements](#trade-offs-and-future-improvements).

## Core Components

### 1. Job Worker Library

The job worker library provides the core functionality for managing jobs:

```go
type Job struct {
    ID            string
    cmd           *exec.Cmd
    status        JobStatus
    hasExited     bool
    
    // Synchronization
    mu            sync.Mutex
    dataAvailable *sync.Cond
    
    // Job output
    output        *JobOutput
    stdoutPipe    struct {
        reader *io.PipeReader
        writer *io.PipeWriter
    }
    stderrPipe    struct {
        reader *io.PipeReader
        writer *io.PipeWriter
    }
}

type JobManager interface {
    StartJob(ctx context.Context, cmd string, args []string) (*Job, error)
    StopJob(ctx context.Context, jobID string) (StopStatus, error)
    GetJobStatus(ctx context.Context, jobID string) (*Job, error)
    StreamOutput(ctx context.Context, jobID string) (<-chan []byte, error)
}

type JobOutput struct {
    mu      sync.RWMutex
    file    *os.File
    baseDir string
    jobID   string
}
```

#### Key implementation details

##### StartJob

- `os/exec` will be used to start processes.
- `StartJob` will:
  1. create a new `Job`
  2. create a new cgroup `jobworker.slice/job-<uuid>.scope` for the job
  3. apply the specified resource limits (CPU, memory, disk I/O)
  4. start the process in its own cgroup by setting the `CgroupFD` `SysProcAttr`
  5. manage its output streams by:
     - Creating output file in append mode
     - Creating pipes for stdout and stderr
     - Starting a goroutine that:
       - Reads from pipes
       - Writes to file
       - Broadcasts on `sync.Cond` `dataAvailable` after each write
         - NOTE: A channel was considered here, but `sync.Cond` Broadcast is efficient with no readers and simple with multiple waiting readers
  6. start a goroutine that:
     - Waits for process completion
     - Sets `pids.max` to 0 to prevent new processes
     - Waits briefly for any stragglers
     - Attempts to remove the cgroup with no retry, for simplicity
     - Removes the `Job` from the map of jobs in memory
- Each job will have:
  - A dedicated directory in `/tmp/jobworker/<job_id>/`
  - A single output file containing both stdout and stderr
- The output file will:
  - Be created when the job starts
  - Grow as output is written
  - Remain available after job completion

##### StreamOutput

- When a new client connects:
  1. Open output file in read-only mode
  2. Start reading from the beginning
  3. Continue reading as new output arrives
  4. Multiple readers can concurrently read from the file
  5. If we reach EOF, we terminate if the process is terminated
  6. If we reach EOF and the process is not terminated, we wait on `sync.Cond` `job.dataAvailable`
  7. When new data arrives, we read from the file from our last position
  ```go
  type OutputReader struct {
      file *os.File
      pos  int64
      job  *Job // Reference to the job to check status
  }
  ```

##### GetJobStatus

- A `Job` struct will track the state of jobs. A map of jobs will be stored in memory, using unique job ID as key.
  ```proto
    enum JobStatus {
        STATUS_UNKNOWN = 0;
        STATUS_RUNNING = 1;
        STATUS_COMPLETED = 2;
        STATUS_FAILED = 3;
        STATUS_STOPPED = 4;
    }
  ```

##### StopJob

- `StopJob` will:
  1. set `pids.max` to 0 in the job's cgroup to prevent new processes
  2. send SIGKILL to all processes in the cgroup
  3. wait briefly for processes to terminate
  4. attempt once to remove the cgroup with no retry, for simplicity
  5. remove the `Job` from the map of jobs in memory

### 2. gRPC API Server

The API is defined using Protocol Buffers:

See [jobworker.proto](/proto/v1/jobworker.proto).

The API Server will be configured with the necessary server and client TLS certificates for authentication.

### 3. CLI Client

A CLI library ([cobra](https://github.com/spf13/cobra)) will be used to parse command-line arguments. We will use the generated gRPC client code to communicate with the server. The client will be configured with the server's address and the necessary TLS certificates for mTLS.

The CLI provides a simple interface to interact with the service:

```bash
# Start a job
$ jobworker-cli start sleep 100
Job ID: c06eede4-27e1-48e6-9df5-17becdd9b385

# Send an argument with contained space
$ jobworker-cli start touch "my file.txt"
Job ID: abb0f888-a17f-4e1c-a95e-97eb22b1349e

# Run a command with multiple arguments
$ jobworker-cli start ls -l /tmp
Job ID: 92462ee4-137b-4467-baac-62991f367d55

# Get job status
$ jobworker-cli status "c06eede4-27e1-48e6-9df5-17becdd9b385"
Status: RUNNING
Exit Code: -

# Stream job output (no output from "sleep 100")
$ jobworker-cli stream "c06eede4-27e1-48e6-9df5-17becdd9b385"


# Stop a job
$ jobworker-cli stop "c06eede4-27e1-48e6-9df5-17becdd9b385"
Job stop requested

# Try to stop a non-existent job
$ jobworker-cli stop "c06eede4-27e1-48e6-9df5-17becdd9b385"
Error: Job not found

# Get status after stopping
$ jobworker-cli status "c06eede4-27e1-48e6-9df5-17becdd9b385"
Status: STOPPED
Exit Code: 137
```

The CLI will:
1. Parse and validate command line arguments
2. Connect to the server using mTLS
3. Format and display responses in a user-friendly way

## Security

### Authentication

- Client certificates will be required.
- The `crypto/tls` package will be used to configure the gRPC server with mTLS.
- Strong cipher suites (TLS 1.3) will be enforced.

### Authorization

A simple authorization scheme will be implemented. The client's certificate will be inspected to extract the CN identifier. A hardcoded map will associate identifiers with API access.
```
# Client Certificate
Subject: CN=Alice@jobworker
```
- Hardcoded authorization for prototype:

| Identifier | Access                                     |
| ---------- | ------------------------------------------ |
| Alice      | start/stop/stream/status (ie. full access) |
| Bob        | stream/status (ie. read-only)              |
| Carol      | no access                                  |

## Resource Management

### cgroups Implementation

- Use cgroups v2 for resource limits for `cpu.max`, `memory.high`, and `io.weight`.
- Default limits per job will be hardcoded and the same limits will be used for all jobs.
- Use `pids.max` during job cleanup to prevent child processes spawning.

## Testing Strategy

Focus on testing critical components:
- Process management and cleanup
- Output streaming
- cgroups resource limits
- Authorization logic

## Trade-offs and Future Improvements

1. **State Management**
- Current: In-memory with basic filesystem persistence
- Future: Use a proper database for job state

2. **Configuration**
- Current: Hardcoded values
- Future: Configuration system with environment variables and config files

3. **Resource Limits**
- Current: Hardcoded per-server job limits (not shared pool)
- Future: Configurable job limits

4. **High Availability**
- Current: Single instance
- Future: API and worker replication

5. **Monitoring**
- Current: Logging to stdout and stderr
- Future: Centralized logging system for manageability and analysis

6. **Secrets**
- Current: Pre-generate secrets and distribute in repo
- Future: Auto-generate secrets and limit read access

7. **Input Validation**
- Current: No input validation is performed
- Future: Validate input for errors / abuse (path traversal, command whitelist, maximum input length)

8. **Authorization**
- Current: Hardcoded auth based on client identifiers
- Future: More robust system (e.g., RBAC)

9. **Job Termination**
- Current: Send uncatchable SIGKILL to terminate processes immediately
- Future: Send SIGTERM, wait, then send SIGKILL to allow processes to terminate gracefully

10. **Job Output**
- Current: Single file for combined stdout and stderr
- Future: Separate stdout and stderr streams, timestamps

11. **Cgroups Library**
- Current: No shared code used
- Future: Use a library or external code for managing cgroups (ie. [containerd/cgroups](https://github.com/containerd/cgroups))

12. **Output Streaming**
- Current: Broadcast to waiting readers on every write
- Future: Batch notifications, add backpressure, use per-reader channels

13. **Job Management**
- Current: Two goroutines per job for output and lifecycle
- Future: Worker pool for output processing, more sophisticated process management

## Building and Running

### Building the Binaries

```bash
# Build all binaries
go build -o bin/jobworker-server ./server
go build -o bin/jobworker-cli ./client
```

### Running the Server

The server requires TLS certificates for mTLS authentication. For the prototype, certificates will be pre-generated and placed in a `certs` directory:

```bash
- certs/
   - ca.crt           # CA certificate
   ─ server.crt       # Server certificate
   ─ server.key       # Server private key
   ─ alice.crt        # Client certificate for Alice (full access)
   ─ alice.key        # Client private key for Alice
   ─ bob.crt          # Client certificate for Bob (read-only)
   ─ bob.key          # Client private key for Bob
   - carol.crt        # Client certificate for Carol (no access)
   - carol.key        # Client private key for Carol 
```

Start the server:
```bash
# Default port (50051)
./bin/jobworker-server --certs-dir ./certs

# Custom port
./bin/jobworker-server --certs-dir ./certs --port 50052
```

#### Requirements

NOTE: The server expects cgroupsv2 to be available on the system and will return an error if only cgroupsv1 is available.

### Running the CLI

The CLI requires the client certificate and key for authentication. For the prototype, we'll specify the exact certificate files via arguments:

```bash
# Start a job (using Alice's certificate for full access)
./bin/jobworker-cli --cert ./certs/alice.crt --key ./certs/alice.key --server localhost:50051 start "sleep 100"

# Get job status (using Bob's certificate for read-only access)
./bin/jobworker-cli --cert ./certs/bob.crt --key ./certs/bob.key --server localhost:50051 status <job_id>

# Stream output (using Bob's certificate)
./bin/jobworker-cli --cert ./certs/bob.crt --key ./certs/bob.key --server localhost:50051 stream <job_id>

# Stop a job (using Alice's certificate)
./bin/jobworker-cli --cert ./certs/alice.crt --key ./certs/alice.key --server localhost:50051 stop <job_id>
```

## Testing

Run the test suite:
```bash
# Run server tests
go test ./server

# Run client tests
go test ./client

# Run all tests
go test ./...
```
