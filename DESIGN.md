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
    ID        string
    cmd       *exec.Cmd
    pgid      int
}

type JobManager interface {
    StartJob(ctx context.Context, cmd string, args []string) (*Job, error)
    StopJob(ctx context.Context, jobID string) (StopStatus, error)
    GetJobStatus(ctx context.Context, jobID string) (*Job, error)
    StreamOutput(ctx context.Context, jobID string) (<-chan []byte, error)
}
```

#### Key implementation details

##### StartJob

- `os/exec` will be used to start processes.
- To capture output, `io.Pipe` will be used to create pipes for stdout and stderr, allowing non-blocking reads.
- `StartJob` will create a new `Job`, start the process in a process group (pgid), and manage its output streams.
- `StartJob` will create a new cgroup for each job, apply the specified resource limits (CPU, memory, disk I/O), and add the process to its own cgroup `jobworker.slice/job-<uuid>.scope`
- Each job will have:
  - A dedicated directory in `/tmp/jobworker/<job_id>/`
  - A single output file containing both stdout and stderr
  - File opened in append mode for writing
- Output streaming will use file-based storage:
  ```go
  type JobOutput struct {
      mu      sync.RWMutex
      file    *os.File
      baseDir string
      jobID   string
  }
  ```
- The output file will:
  - Be created when the job starts
  - Grow as output is written
  - Remain available after job completion

##### StreamOutput

- When a new client connects:
  1. Open output file in read-only mode
  2. Start reading from the beginning
  3. Continue reading as new output arrives
  4. Multiple readers can concurrently read from the file.
  ```go
  type OutputReader struct {
      file *os.File
      pos  int64
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

- `StopJob` will use `os.exec/Process.Kill` to terminate the process group, to ensure the entire process tree is terminated.

### 2. gRPC API Server

The API is defined using Protocol Buffers:

See [jobworker.proto](/proto/v1/jobworker.proto).

The API Server will be configured with the necessary server and client TLS certificates for authentication.

### 3. CLI Client

A CLI library ([cobra](https://github.com/spf13/cobra)) will be used to parse command-line arguments. We will use the generated gRPC client code to communicate with the server. The client will be configured with the server's address and the necessary TLS certificates for mTLS.

The CLI provides a simple interface to interact with the service:

```bash
# Start a job
$ jobworker start "sleep 100"
Job ID: c06eede4-27e1-48e6-9df5-17becdd9b385

# Get job status
$ jobworker status "c06eede4-27e1-48e6-9df5-17becdd9b385"
Status: RUNNING
Started: 2025-05-14T17:55:30Z
Exit Code: -

# Stream job output
$ jobworker stream "c06eede4-27e1-48e6-9df5-17becdd9b385"
[2025-05-14T17:55:30Z] Starting job...
[2025-05-14T17:55:31Z] Processing...

# Stop a job
$ jobworker stop "c06eede4-27e1-48e6-9df5-17becdd9b385"
Job stop requested

# Try to stop a non-existent job
$ jobworker stop "c06eede4-27e1-48e6-9df5-17becdd9b385"
Error: Job not found

# Get status after stopping
$ jobworker status "c06eede4-27e1-48e6-9df5-17becdd9b385"
Status: STOPPED
Started: 2025-05-14T17:55:30Z
Ended: 2025-05-14T17:55:35Z
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

A simple authorization scheme will be implemented. The client's certificate will be inspected to extract an identifier (e.g., CN). A hardcoded map will associate identifiers with API access.
- Hardcoded authorization for prototype:
| Identifier | Access                                     |
| ------------------------------------------------------- |
| Alice      | start/stop/stream/status (ie. full access) |
| Bob        | stream/status (ie. read-only)              |
| Carol      | no access                                  |
| ------------------------------------------------------- |

## Resource Management

### cgroups Implementation

- Use cgroups v2 for resource limits.
- Default limits per job will be set in a configuration file and the same limits will be used for all jobs.

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
- Current: Fixed limits per job
- Future: Configurable limits based on user roles

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
- Future: Validate input for errors / abuse

8. **Authorization**
- Current: Hardcoded auth based on client identifiers
- Future: More robust system (e.g., RBAC)

9. **Job Termination**
- Current: Send uncatchable SIGKILL to terminate processes immediately
- Future: Send SIGTERM, wait, then send SIGKILL to allow processes to terminate gracefully

10. **Job Output**
- Current: Single file for combined stdout and stderr
- Future: Separate stdout and stderr streams, timestamps

11. **Resource Allocation**
- Current: Start process then add it to cgroup
- Future: Use clone3 to start the process in a cgroup hierarchy

12. **Cgroups Library**
- Current: No shared code used
- Future: Use a library or external code for managing cgroups (ie. [containerd/cgroups](https://github.com/containerd/cgroups))

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
