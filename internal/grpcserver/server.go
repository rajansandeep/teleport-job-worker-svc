package grpcserver

import (
	"context"
	"errors"
	"io"
	"slices"

	jobworkerv1 "github.com/rajansandeep/teleport-job-worker-svc/gen/jobworker/v1"
	"github.com/rajansandeep/teleport-job-worker-svc/internal/worker"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// workerService is the subset of worker.Worker used by Server, extracted as an interface
type workerService interface {
	Start(cmd string, args []string) (string, error)
	Stop(jobID string) error
	Status(jobID string) (worker.JobInfo, error)
	StreamOutput(jobID string) (io.ReadCloser, error)
}

type Server struct {
	jobworkerv1.UnimplementedJobWorkerServer
	worker workerService
}

func NewServer(workerSvc workerService) *Server {
	return &Server{worker: workerSvc}
}

func (s *Server) Start(ctx context.Context, req *jobworkerv1.StartRequest) (*jobworkerv1.StartResponse, error) {
	jobID, err := s.worker.Start(req.GetCommand(), req.GetArgs())
	if err != nil {
		return nil, mapWorkerError(err)
	}
	return &jobworkerv1.StartResponse{JobId: jobID}, nil
}

func (s *Server) Stop(ctx context.Context, req *jobworkerv1.StopRequest) (*jobworkerv1.StopResponse, error) {
	if err := s.worker.Stop(req.GetJobId()); err != nil {
		return nil, mapWorkerError(err)
	}
	return &jobworkerv1.StopResponse{}, nil
}

func (s *Server) Status(ctx context.Context, req *jobworkerv1.StatusRequest) (*jobworkerv1.StatusResponse, error) {
	info, err := s.worker.Status(req.GetJobId())
	if err != nil {
		return nil, mapWorkerError(err)
	}
	return statusResponseFromJobInfo(info), nil
}

func (s *Server) StreamOutput(req *jobworkerv1.StreamOutputRequest, stream jobworkerv1.JobWorker_StreamOutputServer) error {
	reader, err := s.worker.StreamOutput(req.GetJobId())
	if err != nil {
		return mapWorkerError(err)
	}
	// defer reader.Close() is the primary closer. The cancellation goroutine
	// below may also call reader.Close().
	// outputBuffer.reader.Close is idempotent so the double-close is safe.
	defer reader.Close()

	done := make(chan struct{})
	defer close(done)

	go func() {
		select {
		case <-stream.Context().Done():
			_ = reader.Close()
		case <-done:
		}
	}()

	buf := make([]byte, 32*1024)

	for {
		n, err := reader.Read(buf)
		if n > 0 {
			// Copy before Send: buf is reused on the next iteration.
			// gRPC-Go serializes synchronously, not relying on that
			// implementation detail here.
			data := slices.Clone(buf[:n])
			if sendErr := stream.Send(&jobworkerv1.OutputChunk{Data: data}); sendErr != nil {
				return sendErr
			}
		}

		if errors.Is(err, io.EOF) {
			return nil
		}
		if errors.Is(err, io.ErrClosedPipe) {
			// Reader was closed before the job finished.
			// Surface the context error if cancellation triggered the close
			// otherwise treat as an interrupted read.
			if ctxErr := stream.Context().Err(); ctxErr != nil {
				return ctxErr
			}
			return status.Error(codes.Canceled, "stream interrupted")
		}
		if err != nil {
			// Check context before mapping: an interrupted read from a future
			// reader implementation may not return io.ErrClosedPipe specifically,
			// but the cause is still client disconnection.
			if ctxErr := stream.Context().Err(); ctxErr != nil {
				return ctxErr
			}
			return mapWorkerError(err)
		}
	}
}

func statusResponseFromJobInfo(info worker.JobInfo) *jobworkerv1.StatusResponse {
	resp := &jobworkerv1.StatusResponse{
		JobId:   info.ID,
		Command: info.Command,
		Args:    slices.Clone(info.Args),
		State:   protoState(info.State),
		Error:   info.ErrMsg,
	}

	if info.ExitCode != nil {
		// Process exit codes are [0, 255] on Linux; int32 is safe here.
		v := int32(*info.ExitCode)
		resp.ExitCode = &v
	}
	if !info.StartedAt.IsZero() {
		resp.StartedAt = info.StartedAt.Unix()
	}
	if !info.FinishedAt.IsZero() {
		resp.FinishedAt = info.FinishedAt.Unix()
	}

	return resp
}

func protoState(state worker.JobState) jobworkerv1.JobState {
	switch state {
	case worker.JobStateRunning:
		return jobworkerv1.JobState_JOB_STATE_RUNNING
	case worker.JobStateCompleted:
		return jobworkerv1.JobState_JOB_STATE_COMPLETED
	case worker.JobStateFailed:
		return jobworkerv1.JobState_JOB_STATE_FAILED
	case worker.JobStateStopped:
		return jobworkerv1.JobState_JOB_STATE_STOPPED
	default:
		return jobworkerv1.JobState_JOB_STATE_UNSPECIFIED
	}
}

func mapWorkerError(err error) error {
	switch {
	case errors.Is(err, worker.ErrJobNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, worker.ErrJobNotRunning):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, worker.ErrInvalidCommand):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}
