# Job Worker Service

A gRPC service for running and monitoring shell commands on a remote server. Start a command, stream its output in real time, check its status, or stop it: all over a secure, mutually-authenticated TLS connection.

---

## Table of Contents

- [How it works](#how-it-works)
- [Prerequisites](#prerequisites)
- [Build](#build)
- [Set up certificates](#set-up-certificates)
- [Run the server](#run-the-server)
- [Use the CLI](#use-the-cli)
  - [start](#start)
  - [status](#status)
  - [stream](#stream)
  - [stop](#stop)
- [Supply certificates to the CLI](#supply-certificates-to-the-cli)
- [Roles and permissions](#roles-and-permissions)
- [Job states](#job-states)
- [Full walkthrough](#full-walkthrough)

---

## How it works

```
 ______________        gRPC / mTLS          _____________________
| jobworker-  | -------------------------> |  jobworker-server  |
|     cli     |                            |                    |
|_____________|                            |  runs OS processes |
                                           |  streams output    |
                                           |____________________|
```

- The server runs on a machine and executes commands as child processes.
- The CLI connects to the server, submits commands, and receives results.
- Every connection is authenticated with mutual TLS. Both sides present a certificate signed by the same CA.
- Access is controlled by role. The client certificate's common name determines what the caller can do.

---

## Prerequisites

- Go 1.26 or later
- `openssl` (for generating certificates)
- `protoc` and the Go protobuf plugins (only needed if you change the `.proto` file)

---

## Build

```bash
# Build the server binary
make build-server      # produces ./jobworker-server

# Build the CLI binary
make build-cli         # produces ./jobworker-cli
```

Both binaries are self-contained. Copy them to any machine running the same OS and architecture.

---

## Set up certificates

The service uses mutual TLS. Both the server and every client need a certificate signed by a shared CA.

Generate a full set of development certificates with one command:

```bash
make certs
```

This runs `scripts/generate-certs.sh` and writes the following files into the `certs/` directory:

| File | Description |
|---|---|
| `ca.crt` / `ca.key` | Certificate authority: used to sign all other certs |
| `server.crt` / `server.key` | Server identity: presented to connecting clients |
| `admin.crt` / `admin.key` | Client cert with the `admin` role |
| `viewer.crt` / `viewer.key` | Client cert with the `viewer` role |

The CA certificate (`ca.crt`) must be present on both the server and every client so each side can verify the other.

> **Note:** These certificates are valid for 365 days and are intended for development use only. For production, replace them with certificates from your own PKI.

---

## Run the server

```bash
./jobworker-server \
  --cert certs/server.crt \
  --key  certs/server.key \
  --ca   certs/ca.crt
```

The server listens on `:50051` by default. Use `--listen` to change it:

```bash
./jobworker-server \
  --listen :9090 \
  --cert certs/server.crt \
  --key  certs/server.key \
  --ca   certs/ca.crt
```

All three certificate flags are required. The server will not start without them.

---

## Use the CLI

```
./jobworker-cli [global flags] <command> [args]

Global flags:
  --addr   server address (default "localhost:50051")
  --cert   path to client certificate PEM file
  --key    path to client private key PEM file
  --ca     path to CA certificate PEM file
```

### Start

Start a new job on the server. Prints the job ID to stdout.

```bash
./jobworker-cli [global flags] start -- <command> [args...]
```

The `--` separator is required. Everything after it is the command to run.

```bash
# Run a simple command
./jobworker-cli --cert certs/admin.crt --key certs/admin.key --ca certs/ca.crt \
  start -- ls -la /tmp

# Run a long-running script
./jobworker-cli --cert certs/admin.crt --key certs/admin.key --ca certs/ca.crt \
  start -- sh -c 'for i in $(seq 1 10); do echo "line $i"; sleep 1; done'
```

Output:
```
3f2e1d4c-8b9a-4f1e-a2b3-c4d5e6f70001
```

Save the job ID as you will need it for the other commands.

---

### Status

Show the current state of a job.

```bash
./jobworker-cli [global flags] status <job-id>
```

```bash
./jobworker-cli --cert certs/admin.crt --key certs/admin.key --ca certs/ca.crt \
  status 3f2e1d4c-8b9a-4f1e-a2b3-c4d5e6f70001
```

Output:
```
ID:         3f2e1d4c-8b9a-4f1e-a2b3-c4d5e6f70001
Command:    ls -la /tmp
State:      Completed
Exit Code:  0
```

---

### Stream

Stream the combined stdout and stderr of a job to your terminal.

```bash
./jobworker-cli [global flags] stream <job-id>
```

```bash
./jobworker-cli --cert certs/admin.crt --key certs/admin.key --ca certs/ca.crt \
  stream 3f2e1d4c-8b9a-4f1e-a2b3-c4d5e6f70001
```

A few things worth knowing:

- **Replay from the start.** You always receive the full output from the beginning, even if you connect after the job has been running for a while.
- **Blocks until done.** The stream stays open until the job finishes or you press `Ctrl+C`.
- **Multiple readers.** Any number of clients can stream the same job at the same time. Each gets their own independent copy of the output.
- **Pipe-friendly.** Output goes to stdout, so you can pipe or redirect it normally.

```bash
# Save output to a file
./jobworker-cli --cert certs/admin.crt --key certs/admin.key --ca certs/ca.crt \
  stream 3f2e1d4c-8b9a-4f1e-a2b3-c4d5e6f70001 > output.txt

# Search through output as it arrives
./jobworker-cli --cert certs/admin.crt --key certs/admin.key --ca certs/ca.crt \
  stream 3f2e1d4c-8b9a-4f1e-a2b3-c4d5e6f70001 | grep ERROR
```

---

### Stop

Stop a running job. Sends SIGKILL to the process and its entire process group.

```bash
./jobworker-cli [global flags] stop <job-id>
```

```bash
./jobworker-cli --cert certs/admin.crt --key certs/admin.key --ca certs/ca.crt \
  stop 3f2e1d4c-8b9a-4f1e-a2b3-c4d5e6f70001
```

The command returns as soon as the kill signal is sent. The job transitions to `Stopped` shortly after, once the OS confirms the process has exited. Use `status` to confirm:

```bash
./jobworker-cli --cert certs/admin.crt --key certs/admin.key --ca certs/ca.crt \
  status 3f2e1d4c-8b9a-4f1e-a2b3-c4d5e6f70001
# State: Stopped
```

---

## Supply certificates to the CLI

Passing certificate flags on every command gets repetitive. The CLI resolves certificate paths in this order:

1. **Command-line flags**: `--cert`, `--key`, `--ca`
2. **Environment variables**: `JOBWORKER_CERT`, `JOBWORKER_KEY`, `JOBWORKER_CA`
3. **Default paths**: `~/.jobworker/certs/client.crt`, `~/.jobworker/certs/client.key`, `~/.jobworker/certs/ca.crt`

The recommended setup for day-to-day use is to copy your certificate files to the default location:

```bash
mkdir -p ~/.jobworker/certs
cp certs/admin.crt ~/.jobworker/certs/client.crt
cp certs/admin.key ~/.jobworker/certs/client.key
cp certs/ca.crt    ~/.jobworker/certs/ca.crt
chmod 600 ~/.jobworker/certs/*
```

After that, you can omit the certificate flags entirely:

```bash
./jobworker-cli start -- echo hello
./jobworker-cli status <job-id>
```

Or use environment variables if you switch between roles often:

```bash
export JOBWORKER_CERT=certs/viewer.crt
export JOBWORKER_KEY=certs/viewer.key
export JOBWORKER_CA=certs/ca.crt

./jobworker-cli status <job-id>
./jobworker-cli stream <job-id>
```

---

## Roles and permissions

Access is determined by the common name (CN) in the client certificate.

| Role | CN in certificate | start | stop | status | stream |
|---|---|:---:|:---:|:---:|:---:|
| Admin | `admin` | Y | Y | Y | Y |
| Viewer | `viewer` | N | N | Y | Y |

The `admin.crt` generated by `make certs` has CN=admin. The `viewer.crt` has CN=viewer.

Connecting with a certificate that has an unknown CN is rejected with a permission denied error.

---

## Job states

| State | Meaning |
|---|---|
| `Running` | The process is active |
| `Completed` | The process exited with code 0 |
| `Failed` | The process exited with a non-zero code |
| `Stopped` | The job was explicitly stopped via `stop` |

Once a job reaches `Completed`, `Failed`, or `Stopped` it stays in that state. You can still call `status` and `stream` on a finished job.

---

## Full walkthrough

This example walks through starting a job, watching its output live, and checking the final status.

**Terminal 1: start the server:**

```bash
make certs build-server

./jobworker-server \
  --cert certs/server.crt \
  --key  certs/server.key \
  --ca   certs/ca.crt
```

**Terminal 2: run a job and stream its output:**

```bash
make build-cli

# Set up certificate environment
export JOBWORKER_CERT=certs/admin.crt
export JOBWORKER_KEY=certs/admin.key
export JOBWORKER_CA=certs/ca.crt

# Start a job that prints 5 lines over 5 seconds
JOB_ID=$(./jobworker-cli start -- sh -c 'for i in $(seq 1 5); do echo "line $i"; sleep 1; done')
echo "Job ID: $JOB_ID"

# Stream the output as it arrives
./jobworker-cli stream "$JOB_ID"
```

**Terminal 3: join the stream late and still see all output from the start:**

```bash
export JOBWORKER_CERT=certs/viewer.crt
export JOBWORKER_KEY=certs/viewer.key
export JOBWORKER_CA=certs/ca.crt

# Connect partway through: you will still receive line 1 onwards
./jobworker-cli stream "$JOB_ID"
```

**After the job finishes, check the final status:**

```bash
./jobworker-cli status "$JOB_ID"
# ID:         51e944fa-521b-48e0-a0db-ac8a6fd42256
# Command:    sh -c for i in $(seq 1 5); do echo "line $i"; sleep 1; done
# State:      Completed
# Exit Code:  0
```

**Stop a long-running job early:**

```bash
export JOBWORKER_CERT=certs/admin.crt
export JOBWORKER_KEY=certs/admin.key
export JOBWORKER_CA=certs/ca.crt

JOB_ID=$(./jobworker-cli start -- sleep 300)
./jobworker-cli stop "$JOB_ID"
./jobworker-cli status "$JOB_ID"
# ID:         4309cac2-3ef9-4c69-8b7c-86b9bf890d3e
# Command:    sleep 300
# State:      Stopped
# Exit Code:  -
```
