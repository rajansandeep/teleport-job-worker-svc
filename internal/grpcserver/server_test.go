package grpcserver

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	jobworkerv1 "github.com/rajansandeep/teleport-job-worker-svc/gen/jobworker/v1"
	"github.com/rajansandeep/teleport-job-worker-svc/internal/worker"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type fakeWorker struct {
	startJobID string
	startErr   error

	stopErr error

	statusInfo worker.JobInfo
	statusErr  error

	reader    io.ReadCloser
	streamErr error
}

func (f *fakeWorker) Start(cmd string, args []string) (string, error) {
	return f.startJobID, f.startErr
}

func (f *fakeWorker) Stop(jobID string) error {
	return f.stopErr
}

func (f *fakeWorker) Status(jobID string) (worker.JobInfo, error) {
	return f.statusInfo, f.statusErr
}

func (f *fakeWorker) StreamOutput(jobID string) (io.ReadCloser, error) {
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	return f.reader, nil
}

type fakeStream struct {
	ctx     context.Context
	sendErr error

	mu   sync.Mutex
	sent [][]byte
}

func (f *fakeStream) Context() context.Context { return f.ctx }

func (f *fakeStream) Send(chunk *jobworkerv1.OutputChunk) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	data := append([]byte(nil), chunk.GetData()...)
	f.sent = append(f.sent, data)
	return nil
}

// Sent returns a copy of all chunks sent so far, safe to call after
// StreamOutput has returned.
func (f *fakeStream) Sent() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.sent))
	copy(out, f.sent)
	return out
}

func (f *fakeStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeStream) SetTrailer(metadata.MD)       {}
func (f *fakeStream) SendMsg(any) error            { return nil }
func (f *fakeStream) RecvMsg(any) error            { return nil }

type blockingReader struct {
	readStarted chan struct{}
	unblock     chan struct{}

	once   sync.Once
	mu     sync.Mutex
	closed bool
}

func newBlockingReader() *blockingReader {
	return &blockingReader{
		readStarted: make(chan struct{}),
		unblock:     make(chan struct{}),
	}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	r.once.Do(func() {
		close(r.readStarted)
	})

	<-r.unblock

	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()

	if closed {
		return 0, io.ErrClosedPipe
	}
	return 0, io.EOF
}

func (r *blockingReader) Close() error {
	r.mu.Lock()
	alreadyClosed := r.closed
	r.closed = true
	r.mu.Unlock()

	// The lock ensures exactly one caller exits with alreadyClosed == false,
	// so close(r.unblock) is called at most once.
	if !alreadyClosed {
		close(r.unblock)
	}
	return nil
}

func (r *blockingReader) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

type chunkReader struct {
	mu       sync.Mutex
	chunks   [][]byte
	closeCnt int
}

func newChunkReader(chunks ...[]byte) *chunkReader {
	return &chunkReader{chunks: chunks}
}

func (r *chunkReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.chunks) == 0 {
		return 0, io.EOF
	}

	chunk := r.chunks[0]
	n := copy(p, chunk)
	if n < len(chunk) {
		// Chunk is larger than p: retain the unconsumed tail for the next Read.
		r.chunks[0] = chunk[n:]
	} else {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

func (r *chunkReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeCnt++
	return nil
}

func (r *chunkReader) CloseCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closeCnt
}

func TestStartHandlerSuccess(t *testing.T) {
	srv := NewServer(&fakeWorker{startJobID: "job-123"})

	resp, err := srv.Start(t.Context(), &jobworkerv1.StartRequest{
		Command: "echo",
		Args:    []string{"hello"},
	})
	if err != nil {
		t.Fatalf("Start returned unexpected error: %v", err)
	}
	if resp.GetJobId() != "job-123" {
		t.Fatalf("unexpected job id: got %q want %q", resp.GetJobId(), "job-123")
	}
}

func TestMapWorkerError(t *testing.T) {
	cases := []struct {
		name     string
		in       error
		wantCode codes.Code
	}{
		{"not found", worker.ErrJobNotFound, codes.NotFound},
		{"not running", worker.ErrJobNotRunning, codes.FailedPrecondition},
		{"invalid command", worker.ErrInvalidCommand, codes.InvalidArgument},
		{"unknown error", errors.New("boom"), codes.Internal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := mapWorkerError(tc.in)
			st, ok := status.FromError(err)
			if !ok {
				t.Fatalf("expected gRPC status error, got %v", err)
			}
			if st.Code() != tc.wantCode {
				t.Fatalf("code = %v, want %v", st.Code(), tc.wantCode)
			}
		})
	}
}

func TestStopHandlerNotFound(t *testing.T) {
	srv := NewServer(&fakeWorker{stopErr: worker.ErrJobNotFound})

	_, err := srv.Stop(t.Context(), &jobworkerv1.StopRequest{JobId: "missing"})
	if err == nil {
		t.Fatal("expected error")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", st.Code())
	}
}

func TestStatusHandlerMapping(t *testing.T) {
	exitCode := 7
	startedAt := time.Unix(100, 0)
	finishedAt := time.Unix(200, 0)

	t.Run("AllFieldsPopulated", func(t *testing.T) {
		srv := NewServer(&fakeWorker{
			statusInfo: worker.JobInfo{
				ID:         "job-1",
				Command:    "cmd",
				Args:       []string{"a", "b"},
				State:      worker.JobStateFailed,
				ExitCode:   &exitCode,
				ErrMsg:     "boom",
				StartedAt:  startedAt,
				FinishedAt: finishedAt,
			},
		})

		resp, err := srv.Status(t.Context(), &jobworkerv1.StatusRequest{JobId: "job-1"})
		if err != nil {
			t.Fatalf("Status returned unexpected error: %v", err)
		}
		if resp.GetJobId() != "job-1" {
			t.Fatalf("unexpected job id: %q", resp.GetJobId())
		}
		if resp.GetCommand() != "cmd" {
			t.Fatalf("unexpected command: %q", resp.GetCommand())
		}
		if len(resp.GetArgs()) != 2 || resp.GetArgs()[0] != "a" || resp.GetArgs()[1] != "b" {
			t.Fatalf("unexpected args: %#v", resp.GetArgs())
		}
		if resp.GetState() != jobworkerv1.JobState_JOB_STATE_FAILED {
			t.Fatalf("unexpected state: %v", resp.GetState())
		}
		if resp.ExitCode == nil {
			t.Fatal("expected ExitCode to be set")
		}
		if resp.GetExitCode() != 7 {
			t.Fatalf("unexpected exit code: %d", resp.GetExitCode())
		}
		if resp.GetError() != "boom" {
			t.Fatalf("unexpected error: %q", resp.GetError())
		}
		if resp.GetStartedAt() != 100 {
			t.Fatalf("unexpected started_at: %d", resp.GetStartedAt())
		}
		if resp.GetFinishedAt() != 200 {
			t.Fatalf("unexpected finished_at: %d", resp.GetFinishedAt())
		}
	})

	t.Run("RunningJobHasNilExitCodeAndZeroFinishedAt", func(t *testing.T) {
		srv := NewServer(&fakeWorker{
			statusInfo: worker.JobInfo{
				ID:        "job-1",
				Command:   "cmd",
				State:     worker.JobStateRunning,
				StartedAt: time.Unix(100, 0),
			},
		})

		resp, err := srv.Status(t.Context(), &jobworkerv1.StatusRequest{JobId: "job-1"})
		if err != nil {
			t.Fatalf("Status returned unexpected error: %v", err)
		}
		if resp.ExitCode != nil {
			t.Fatalf("expected nil ExitCode for running job, got %d", resp.GetExitCode())
		}
		if resp.GetFinishedAt() != 0 {
			t.Fatalf("expected finished_at == 0 for running job, got %d", resp.GetFinishedAt())
		}
	})
}

func TestStreamOutputWorkerError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode codes.Code
	}{
		{"not found", worker.ErrJobNotFound, codes.NotFound},
		{"not running", worker.ErrJobNotRunning, codes.FailedPrecondition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(&fakeWorker{streamErr: tc.err})
			stream := &fakeStream{ctx: t.Context()}

			err := srv.StreamOutput(&jobworkerv1.StreamOutputRequest{JobId: "missing"}, stream)
			if err == nil {
				t.Fatal("expected error")
			}

			st, ok := status.FromError(err)
			if !ok {
				t.Fatalf("expected gRPC status error, got %v", err)
			}
			if st.Code() != tc.wantCode {
				t.Fatalf("expected %v, got %v", tc.wantCode, st.Code())
			}
			if len(stream.Sent()) != 0 {
				t.Fatal("expected no chunks to be sent on worker error")
			}
		})
	}
}

func TestStreamOutputCancellationClosesReaderAndReturnsContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	reader := newBlockingReader()
	srv := NewServer(&fakeWorker{reader: reader})
	stream := &fakeStream{ctx: ctx}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.StreamOutput(&jobworkerv1.StreamOutputRequest{JobId: "job-1"}, stream)
	}()

	<-reader.readStarted
	cancel()

	err := <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if !reader.isClosed() {
		t.Fatalf("expected reader to be closed on stream cancellation")
	}
}

func TestStreamOutputReadsChunksAndReturnsNilOnEOF(t *testing.T) {
	reader := newChunkReader([]byte("hello "), []byte("world"))
	srv := NewServer(&fakeWorker{reader: reader})
	stream := &fakeStream{ctx: t.Context()}

	err := srv.StreamOutput(&jobworkerv1.StreamOutputRequest{JobId: "job-1"}, stream)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	sent := stream.Sent()
	if len(sent) != 2 {
		t.Fatalf("expected 2 chunks sent, got %d", len(sent))
	}

	got := string(sent[0]) + string(sent[1])
	if got != "hello world" {
		t.Fatalf("unexpected streamed output: got %q want %q", got, "hello world")
	}

	if reader.CloseCount() == 0 {
		t.Fatalf("expected reader.Close to be called via defer")
	}
}

func TestStreamOutputSendErrorClosesReader(t *testing.T) {
	sendErr := errors.New("send failed")
	reader := newChunkReader([]byte("data"))
	srv := NewServer(&fakeWorker{reader: reader})
	stream := &fakeStream{ctx: t.Context(), sendErr: sendErr}

	err := srv.StreamOutput(&jobworkerv1.StreamOutputRequest{JobId: "job-1"}, stream)
	if !errors.Is(err, sendErr) {
		t.Fatalf("expected sendErr, got %v", err)
	}
	if reader.CloseCount() == 0 {
		t.Fatalf("expected reader.Close to be called after send error")
	}
}

// TestStreamOutputCancellationStress runs the cancellation scenario repeatedly
// to surface races that a single execution would not reliably trigger. It
// complements TestStreamOutputCancellationClosesReaderAndReturnsContextCanceled,
// which serves as the readable specification of the expected behaviour.
func TestStreamOutputCancellationStress(t *testing.T) {
	const iterations = 100

	for i := 0; i < iterations; i++ {
		func() {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			reader := newBlockingReader()
			srv := NewServer(&fakeWorker{reader: reader})
			stream := &fakeStream{ctx: ctx}

			errCh := make(chan error, 1)
			go func() {
				errCh <- srv.StreamOutput(&jobworkerv1.StreamOutputRequest{JobId: "job-1"}, stream)
			}()

			<-reader.readStarted
			cancel()

			err := <-errCh
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("iteration %d: expected context.Canceled, got %v", i, err)
			}
			if !reader.isClosed() {
				t.Fatalf("iteration %d: expected reader to be closed", i)
			}
		}()
	}
}
