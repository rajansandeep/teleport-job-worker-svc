# Design: Job Worker Service

## What

A prototype job worker service that works on 64 bit Linux. It comprises of three components: a reusable library implementing the functionality for working with jobs, a gRPC API server secured with mTLS and a CLI client.

The service executes arbitrary Linux processes, manages their lifecycle (start, stop, query status) and streams output to multiple concurrent clients in real-time.

Target: Level 4

## Why

The Teleport systems engineering challenge requires building a prototype job worker service which demonstrates good process management, secure transport and concurrent streaming. The designed system must be safe, correct and minimal.

## Design details

### Scope

**In-Scope:**
- Process lifecycle management (start, stop, query)
- Efficient output streaming without polling. 
    - Support multiple concurrent clients and late-joiners
- Process based job termination 
    - Kills main process and all children in the group
- gRPC API with mTLS and certificate based authorization
- Binary safe output handling

**Out-of-Scope:**
- Job persistence / crash recovery
- Graceful SIGTERM termination. SIGKILL only supported.

### Architecture

Three components with clear separation:

![alt text](arch.png)

- Library: Contains process execution, state machine and output buffer. Has no knowledge of transport or auth.
- gRPC Server: Thin wrapper mapping RPCs to library calls. It owns mTLS and authorization.
- CLI: Subcommands that maps to RPCs. It writes the raw bytes to stdout.

### Process execution

For spawning services and running processes, the decision is to use `os.exec.Cmd` with `cmd.Stdout = Writer` and `cmd.StdErr = Writer`. 

As noted above, stdout and stderr are combined into a single stream. This simplifies the streaming infrastructure and also maps how `kubectl logs` behaves today.

Alternatives considered:
- Using `StdoutPipe()` + `io.Copy` goroutines. While this is a valid alternative, to keep it simple and avoid the `StdoutPipe` and `Wait` coordination issue.
- `CombinedOutput()` does not satisfy the criteria of real time streaming as it blocks until process completion.

### Output Streaming

For capturing the stdout/stderr and multiple clients streaming the job's output in real time, including late joiners, without polling.

To achieve this, the decision is to use a shared growable `[]byte` buffer that is protected by a `sync.Mutex`, with `sync.Cond.Broadcast()` for subscriber notification. Each subscriber tracks its own read offset.

This method satisfies:
- Late joiners: Each reader starts at offset=0
- No Polling: `sync.Cond.Wait()` truly blocks a goroutine and consumes zero CPU
- Independent reader: Each reader has their own offset
- Readers can exit early without affecting the underlying job or other active readers
- Binary safe: Everything is `[]byte` e2e

Some considerations:
- `sync.Cond.Wait()` cannot be interrupted by context. A fix for this is to spawn a goroutine helper that calls `Broadcast()` on `ctx.Done()` and check `ctx.Err()`. The helper can then be cleaned up via `done` preventing any leaks.
- Each stream handler goroutine is independent and hence if a stream blocks on a slow client, only that reader will be affected. The buffer's `Write()` is unaffected.
- A known limitation here is that the buffer keeps growing with process output. A mitigation here would be to have a max size configured and stop recording with the threshold reaches. This part would be skipped and is a TODO.

![alt text](output_stream.png)

Alternatives considered:
- Channel based fan out: Late joiners cannot get output from the start as channels are forward only.
- `io.Pipe` and `io.MultiWriter`: Cannot add subscribers after process starts. 
- Using a File backed buffer: This can work using notification via `inotify` or polling, but it adds complexity in the former and violates requirements in the latter. Maybe valid for production systems, but an overkill here.

### Process termination

To handle processes, `SysProcAttr.Setpgid = true` would create a new process group. On Stop, send SIGKILL to the entire hroup using `syscall.Kill(-pgid, SIGKILL)`. 
To prevent `Wait()` from hanging, `cmd.WaitDelay` will be used.

`cmd.Wait()` blocks until the process exits and all I/O copying completes. `cmd.WaitDelay` forces pipe closure after the process exits. Without this, there can be a situation where a stopped job can hang the goroutine forever.

```
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

cmd.WaitDelay = 5 * time.Second
```

The alternative considered was `cmd.Process.Kill()`, but that can cause child processes to become orphaned, with no way to track or kill them.

### Job State Machine

![alt text](job_state.png)

All state transitions here are protected by `sync.RWMutex`, which allows for concurrent status reads.
Transitions here are validated as well. Example: Cannot go from Completed to Running.

### Library API

The library exposes a Go API with no knowledge of gRPC, TLS, or transport. The gRPC server is a thin wrapper that maps RPCs to these calls.

#### ProcessExecutor Interface

The Worker manager doesn't know how processes are launched or killed and delegates it to this interface.

```go
// ProcessExecutor is a layer that hides the details of how processes are started
// Ensures the rest of the code is easy to test and easy to evolve
type ProcessExecutor interface {
    // Start launches the command and begins writing output to the buffer
    // Returns immediately after the process starts
    Start(cmd string, args []string, output io.Writer) (Process, error)
}

// Process represents a running or completed OS process.
type Process interface {
    // Pid returns the OS process ID
    Pid() int
    // Wait blocks until the process exits and returns the exit code
    // err is for OS-level failures, not for non-zero exit codes
    // Example: A process that exits with code 1 returns exitCode=1, err=nil
    Wait() (int, error)
    // Kill terminates the process and all its children
    Kill() error
}
```

#### OutputBuffer

`OutputBuffer` is a shared container that stores bytes of output as they arrive. It is append-only and implements io.Writer.

```go
type outputBuffer struct { /* sync.Mutex, sync.Cond, []byte, closed bool */ }

// Write appends data and wakes all waiting readers
func (b *outputBuffer) Write(p []byte) (int, error)

// NewReader returns a new Reader starting at offset 0
func (b *outputBuffer) NewReader() *Reader

// Close marks the buffer as complete
func (b *outputBuffer) Close()
```

#### OutputReader and Reader

```go
// OutputReader is the interface exposed through WorkerService
// The gRPC layer depends on this
type OutputReader interface {
    // Next blocks until the next chunk of output is available, the buffer
    // is closed, or the context is cancelled
    Next(ctx context.Context) ([]byte, error)
}

// Reader is the concrete implementation of OutputReader
//  It provides a per-client view into an OutputBuffer, tracking its own offset
type reader struct { /* buffer *OutputBuffer, offset int */ }

func (r *reader) Next(ctx context.Context) ([]byte, error)
```

#### Worker Manager

The Worker manager is responsible for creating a UUID for the jobID.

```go
// StartSpec contains everything needed to create a job
type StartSpec struct {
    Command string
    Args    []string
    Owner   string   // CN of the client that created the job
}

type Worker struct { /* sync.RWMutex, map[string]*Job, ProcessExecutor */ }

// Start creates a new job, executes the command, and returns the job ID
func (w *Worker) Start(ctx context.Context, spec StartSpec) (string, error)

// Stop kills a running job and all its children
// Returns an error if the job is not in the Running state
func (w *Worker) Stop(ctx context.Context, jobID string) error

// Status returns a snapshot of the job's current state and metadata
func (w *Worker) Status(ctx context.Context, jobID string) (JobInfo, error)

// List returns a snapshot of all jobs state and metadata
func (w *Worker) List(ctx context.Context) ([]JobInfo, error)

// StreamOutput returns an OutputReader for the job's output buffer
// The caller reads from offset 0 regardless of when the job started or
// whether it has already completed
func (w *Worker) StreamOutput(ctx context.Context, jobID string) (OutputReader, error)
```

#### JobInfo

```go
// JobInfo is a read-only value snapshot returned by Status and List
type JobInfo struct {
    ID         string
    Command    string
    Args       []string
    State      JobState    // Running, Completed, Failed, Stopped
    ExitCode   *int
    Error      string
    Owner      string      // CN of the client that created the job
    CreatedAt  time.Time
    StartedAt  time.Time 
    FinishedAt time.Time
}
```

#### Relationship to gRPC Layer

The gRPC server takes a `WorkerService` interface and acts as a thin adapter:

```go
// WorkerService defines the operations the gRPC layer depends on
type WorkerService interface {
    Start(ctx context.Context, spec StartSpec) (string, error)
    Stop(ctx context.Context, jobID string) error
    Status(ctx context.Context, jobID string) (JobInfo, error)
    List(ctx context.Context) ([]JobInfo, error)
    StreamOutput(ctx context.Context, jobID string) (OutputReader, error)
}

type Server struct {
    worker WorkerService  // interface, not concrete *Worker
}
```

### Configuration

#### gRPC Definition

```protobuf
package jobworker.v1;

service JobWorker {
  // Starts a new job executing the given command
  rpc Start(StartRequest) returns (StartResponse);

  // Stops a running job
  rpc Stop(StopRequest) returns (StopResponse);

  // Returns the current state and metadata of a job
  rpc Status(StatusRequest) returns (StatusResponse);

  // Returns the state and metadata of all jobs
  rpc List(ListRequest) returns (ListResponse);

  // Streams process output (stdout+stderr) from the start of execution
  rpc StreamOutput(StreamOutputRequest) returns (stream OutputChunk);
}

message StartRequest {
  // Executable path or name
  string command = 1;

  // Arguments to execute
  repeated string args = 2;
}

message StartResponse {
  // UID for a created job
  string job_id = 1;
}

message StopRequest {
  // UID for a job to stop
  string job_id = 1;
}

message StopResponse {}

message StatusRequest {
  // UID to query a job's status
  string job_id = 1;
}

message StatusResponse {
  // UID of a job
  string job_id = 1;

  // Executable path or name
  string command = 2;

  // Arguments to execute
  repeated string args = 3;

  // State of a job
  JobState state = 4;

  // A job's exit code. Meaningful only when state is COMPLETED or FAILED
  int32 exit_code = 5;

  // Readable error messages
  string error = 6;         

  // CN of the client that created the job
  string owner = 7;

  // Job created using Unix timestamp seconds
  int64 created_at = 8;  

  // Job started using Unix timestamp seconds    
  int64 started_at = 9;

  // Job finished using Unix timestamp seconds
  int64 finished_at = 10;
}

enum JobState {
  JOB_STATE_UNSPECIFIED = 0;
  JOB_STATE_RUNNING = 1;
  JOB_STATE_COMPLETED = 2;
  JOB_STATE_FAILED = 3;
  JOB_STATE_STOPPED = 4;
}

message ListRequest {}

message ListResponse {
  // Complete status response for all jobs
  repeated StatusResponse jobs = 1;
}

message StreamOutputRequest {
  // // UID of a job whose output to stream
  string job_id = 1;
}

message OutputChunk {
  // // Chunk of process output.
  bytes data = 1;
}
```

#### Error Handling

| Scenario | gRPC Code |
|----------|-----------|
| Job not found | `NOT_FOUND` |
| Unauthorized CN | `PERMISSION_DENIED` |
| Insufficient role | `PERMISSION_DENIED` |
| Job already stopped/completed | `FAILED_PRECONDITION` |
| Invalid request (empty command) | `INVALID_ARGUMENT` |
| Process execution failure | `INTERNAL` |


## Security Considerations

### Transport: mTLS with TLS 1.3

Only TLS 1.3 connections are allowed. In Go, TLS 1.3 already uses only modern and secure encryption options by default. It also keeps things simple. 

The server is configured with `tls.RequireAndVerifyClientCert` so every client must present a valid certificate, preventing anonymous client connections.

### Authentication

Authentication here is handled my mTLS. The server is configured with `tls.RequireAndVerifyClientCert`:
- Every connection must present a client certificate
- Only accept client certificates that trace back to our own CA. Reject certificates issued by anyone else
- Expiry, key usage, and chain validation are enforced by `crypto/tls` in Go

For this prototype service, we identify the user using Common Name (CN) like `admin` to keep things simple. For a production service, this is not recommended and using an approach such as SAN URIs is better. 

The service will also not support certificate revocation, but that would be something to strongly consider for production systems.

### Authorization

After mTLS proves who the client is, a layer of authorization is added to determine what the client can do. 

The design decision here is having a hard coded CN to role map, enforced via gRPC interceptors.

Authorization Flow:
- Extract CN from verified CN Cert (Authentication is already done)
- Look up CN in map -> role (admin, viewer, unknown)
- If allowed -> proceed to handler
- If denied -> return PERMISSION_DENIED immediately

The design principle here is Deny by Default. Any combination not in the map is denied. 

This check runs on every RPC call and not just at connection establishment. Authorization is enforced per call and not per connection as a single TLS connection can have multiple gRPC streams. 
Both gRPC unary and stream interceptors are needed as `StreamOutput` uses the stream interceptor path, while Start/Stop/Status/List uses the unary path.

The design described above is simple as serves its purpose. A dynamic auth (RBAC, OPA/ABAC) was not chosen as it adds complexity for the service. 
Also, job record of the client who started the job is recorded, but all admins can stop the job. An owner-only stop is not added but can be added in the future. Similarly, viewers can view any job, even if they did not create it.

**Role matrix:**

| Role | Start | Stop | Status | List | StreamOutput |
|------|-------|------|--------|------|-------------|
| admin | yes | yes | yes | yes | yes |
| viewer | no | no | yes | yes | yes |
| unknown | no | no | no | no | no |

## CLI UX

The CLI resolves client certificates in the order of precedence: 
- Explicit flags
- Env variables
- default paths

This means that certs can be configured once and omitted from subsequent commands:

```bash
# Option 1: Environment variables (set once per shell session)
export JOBWORKER_CERT=certs/admin.crt
export JOBWORKER_KEY=certs/admin.key
export JOBWORKER_CA=certs/ca.crt

# Option 2: Default paths (zero config after initial setup)
mkdir -p ~/.jobworker/certs
cp certs/admin.crt ~/.jobworker/certs/client.crt
cp certs/admin.key ~/.jobworker/certs/client.key
cp certs/ca.crt ~/.jobworker/certs/ca.crt
```

### Examples

```bash
# Generate test certificates
make certs

# Start the server
./jobworker-server \
  --cert certs/server.crt \
  --key certs/server.key \
  --ca certs/ca.crt \
  --listen :50051

# Start a job (admin only)
# Everything after "--" is the command + args
./jobworker-cli \
  --cert certs/admin.crt --key certs/admin.key --ca certs/ca.crt \
  start -- ls -la /tmp
# Output: job_id: "550e8400-e29b-41d4-a716-446655440000"

# Remaining examples assume env vars or default paths are configured 

# Stream output (admin or viewer)
./jobworker-cli stream 550e8400-e29b-41d4-a716-446655440000
# Output: (raw process stdout+stderr streamed to terminal)

# Query job status
./jobworker-cli status 550e8400-e29b-41d4-a716-446655440000
# Output:
#   ID:        550e8400-e29b-41d4-a716-446655440000
#   Command:   ls -la /tmp
#   State:     COMPLETED
#   Exit Code: 0

# List all jobs
./jobworker-cli list
# Output:
#   ID          COMMAND       STATE       EXIT CODE
#   550e8400    ls -la /tmp   COMPLETED   0
#   7c9e2f10    bash -c ...   RUNNING     -

# Stop a running job (admin only)
./jobworker-cli stop 550e8400-e29b-41d4-a716-446655440000

# Viewer attempting to start (denied: using viewer certs via env vars)
./jobworker-cli \
  --cert certs/viewer.crt --key certs/viewer.key --ca certs/ca.crt \
  start -- echo hello
# error: permission denied: role "viewer" cannot call Start
```

## Implementation Plan

| PR | Scope |
|----|-------|
| PR1 | Core library with  process lifecycle, output buffer with sync.Cond, process group kill, unit tests |
| PR2 | gRPC API + mTLS + authorization interceptors + server binary + cert generation |
| PR3 | CLI client with subcommands, mTLS flags, raw binary output to stdout |
| PR4 | Integration tests + README |