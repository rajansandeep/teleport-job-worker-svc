package integration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	jobworkerv1 "github.com/rajansandeep/teleport-job-worker-svc/gen/jobworker/v1"
	"github.com/rajansandeep/teleport-job-worker-svc/internal/grpcserver"
	"github.com/rajansandeep/teleport-job-worker-svc/internal/worker"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// testCA holds a self-signed CA that can sign server and client certificates
// for use in tests.
type testCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

// newTestCA generates a fresh ECDSA P-256 CA, failing the test on error.
func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}

	return &testCA{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// issue signs a new leaf certificate with the given common name.
// Pass isServer=true to add server-auth usage and a localhost SAN
// pass isServer=false for a client-auth certificate.
func (ca *testCA) issue(t *testing.T, cn string, serial int64, isServer bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key for %q: %v", cn, err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if isServer {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.DNSNames = []string{"localhost"}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create cert for %q: %v", cn, err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key for %q: %v", cn, err)
	}

	tlsCert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatalf("assemble TLS cert for %q: %v", cn, err)
	}
	return tlsCert
}

// serverTLSConfig returns a mutual-TLS config for the server side.
func (ca *testCA) serverTLSConfig(serverCert tls.Certificate) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		Certificates: []tls.Certificate{serverCert},
	}
}

// clientTLSConfig returns a mutual-TLS config for a client presenting clientCert.
func (ca *testCA) clientTLSConfig(clientCert tls.Certificate) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      pool,
		Certificates: []tls.Certificate{clientCert},
		ServerName:   "localhost",
	}
}

// grpcEnv is a live gRPC server backed by a real Worker.
// Create one per test with newGRPCEnv.
type grpcEnv struct {
	addr        string
	ca          *testCA
	adminCert   tls.Certificate
	viewerCert  tls.Certificate
	unknownCert tls.Certificate
}

// newGRPCEnv starts a gRPC server with mTLS on a random port and registers a
// cleanup to stop it when the test ends.
func newGRPCEnv(t *testing.T) *grpcEnv {
	t.Helper()

	ca := newTestCA(t)
	serverCert := ca.issue(t, "localhost", 2, true)
	adminCert := ca.issue(t, "admin", 3, false)
	viewerCert := ca.issue(t, "viewer", 4, false)
	unknownCert := ca.issue(t, "unknown", 5, false)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := grpcserver.NewServer(worker.NewWorker())
	grpcSrv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(ca.serverTLSConfig(serverCert))),
		grpc.UnaryInterceptor(grpcserver.UnaryAuthInterceptor()),
		grpc.StreamInterceptor(grpcserver.StreamAuthInterceptor()),
	)
	jobworkerv1.RegisterJobWorkerServer(grpcSrv, srv)

	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)

	return &grpcEnv{
		addr:        lis.Addr().String(),
		ca:          ca,
		adminCert:   adminCert,
		viewerCert:  viewerCert,
		unknownCert: unknownCert,
	}
}

// newClient dials the test server with the given client certificate.
// The connection is closed when the test ends.
func (e *grpcEnv) newClient(t *testing.T, cert tls.Certificate) jobworkerv1.JobWorkerClient {
	t.Helper()
	conn, err := grpc.NewClient(
		e.addr,
		grpc.WithTransportCredentials(credentials.NewTLS(e.ca.clientTLSConfig(cert))),
	)
	if err != nil {
		t.Fatalf("dial %s: %v", e.addr, err)
	}
	t.Cleanup(func() { conn.Close() })
	return jobworkerv1.NewJobWorkerClient(conn)
}

// waitForGRPCState polls Status over gRPC until the job reaches the wanted
// state or the test context (5 s) expires.
func waitForGRPCState(t *testing.T, client jobworkerv1.JobWorkerClient, jobID string, want jobworkerv1.JobState) *jobworkerv1.StatusResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	for {
		resp, err := client.Status(ctx, &jobworkerv1.StatusRequest{JobId: jobID})
		if err != nil {
			t.Fatalf("Status(%q): %v", jobID, err)
		}
		if resp.GetState() == want {
			return resp
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for gRPC state %s; last=%s", want, resp.GetState())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// drainStream reads all chunks from a gRPC stream to completion and returns
// the concatenated output. It fails the test on any error other than io.EOF.
func drainStream(t *testing.T, stream jobworkerv1.JobWorker_StreamOutputClient) string {
	t.Helper()
	var sb strings.Builder
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return sb.String()
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		sb.Write(chunk.GetData())
	}
}

// TestGRPCAdminFullRoundTrip exercises the complete happy path: Start -> wait
// for Completed -> Status -> StreamOutput (replay), all via an admin client.
func TestGRPCAdminFullRoundTrip(t *testing.T) {
	t.Parallel()
	env := newGRPCEnv(t)
	sh := shellPath(t)
	client := env.newClient(t, env.adminCert)

	startResp, err := client.Start(t.Context(), &jobworkerv1.StartRequest{
		Command: sh,
		Args:    []string{"-c", `echo "hello from grpc"`},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	jobID := startResp.GetJobId()
	if jobID == "" {
		t.Fatal("Start returned empty job ID")
	}

	statusResp := waitForGRPCState(t, client, jobID, jobworkerv1.JobState_JOB_STATE_COMPLETED)
	// Check the optional field directly: GetExitCode() returns 0 for both nil
	// and explicitly-zero, so checking nil vs zero distinguishes "server omitted
	// the field" from "process exited with code 0".
	if statusResp.ExitCode == nil {
		t.Fatal("expected non-nil ExitCode for completed job")
	}
	if statusResp.GetExitCode() != 0 {
		t.Fatalf("expected exit code 0, got %d", statusResp.GetExitCode())
	}

	stream, err := client.StreamOutput(t.Context(), &jobworkerv1.StreamOutputRequest{JobId: jobID})
	if err != nil {
		t.Fatalf("StreamOutput: %v", err)
	}
	out := drainStream(t, stream)
	if !strings.Contains(out, "hello from grpc") {
		t.Fatalf("expected 'hello from grpc' in output, got %q", out)
	}
}

// TestGRPCAdminStopRunningJob verifies that an admin can stop a running job
// and the job transitions to Stopped.
func TestGRPCAdminStopRunningJob(t *testing.T) {
	t.Parallel()
	env := newGRPCEnv(t)
	sh := shellPath(t)
	client := env.newClient(t, env.adminCert)

	startResp, err := client.Start(t.Context(), &jobworkerv1.StartRequest{
		Command: sh,
		Args:    []string{"-c", "sleep 300"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	jobID := startResp.GetJobId()

	waitForGRPCState(t, client, jobID, jobworkerv1.JobState_JOB_STATE_RUNNING)

	_, err = client.Stop(t.Context(), &jobworkerv1.StopRequest{JobId: jobID})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}

	statusResp := waitForGRPCState(t, client, jobID, jobworkerv1.JobState_JOB_STATE_STOPPED)
	if statusResp.ExitCode != nil {
		t.Fatalf("expected nil exit code for stopped job, got %d", statusResp.GetExitCode())
	}
}

// TestGRPCViewerReadOnlyAccess verifies that a viewer can call Status and
// StreamOutput but is denied access to Start and Stop.
func TestGRPCViewerReadOnlyAccess(t *testing.T) {
	t.Parallel()
	env := newGRPCEnv(t)
	sh := shellPath(t)

	// Use admin to create a job the viewer can inspect.
	admin := env.newClient(t, env.adminCert)
	startResp, err := admin.Start(t.Context(), &jobworkerv1.StartRequest{
		Command: sh,
		Args:    []string{"-c", `echo "viewer-test"`},
	})
	if err != nil {
		t.Fatalf("admin Start: %v", err)
	}
	jobID := startResp.GetJobId()
	waitForGRPCState(t, admin, jobID, jobworkerv1.JobState_JOB_STATE_COMPLETED)

	viewer := env.newClient(t, env.viewerCert)

	t.Run("Status allowed", func(t *testing.T) {
		t.Parallel()
		_, err := viewer.Status(t.Context(), &jobworkerv1.StatusRequest{JobId: jobID})
		if err != nil {
			t.Fatalf("viewer Status: %v", err)
		}
	})

	t.Run("StreamOutput allowed", func(t *testing.T) {
		t.Parallel()
		stream, err := viewer.StreamOutput(t.Context(), &jobworkerv1.StreamOutputRequest{JobId: jobID})
		if err != nil {
			t.Fatalf("viewer StreamOutput: %v", err)
		}
		out := drainStream(t, stream)
		if !strings.Contains(out, "viewer-test") {
			t.Fatalf("expected output to contain 'viewer-test', got %q", out)
		}
	})

	t.Run("Start denied", func(t *testing.T) {
		t.Parallel()
		_, err := viewer.Start(t.Context(), &jobworkerv1.StartRequest{
			Command: sh, Args: []string{"-c", "echo hi"},
		})
		assertPermissionDenied(t, err)
	})

	t.Run("Stop denied", func(t *testing.T) {
		t.Parallel()
		_, err := viewer.Stop(t.Context(), &jobworkerv1.StopRequest{JobId: jobID})
		assertPermissionDenied(t, err)
	})
}

// TestGRPCUnknownCNPermissionDenied verifies that a client with a certificate
// whose CN is not a known role is rejected with PermissionDenied on every RPC.
func TestGRPCUnknownCNPermissionDenied(t *testing.T) {
	t.Parallel()
	env := newGRPCEnv(t)
	sh := shellPath(t)
	unknown := env.newClient(t, env.unknownCert)

	t.Run("Start", func(t *testing.T) {
		t.Parallel()
		_, err := unknown.Start(t.Context(), &jobworkerv1.StartRequest{
			Command: sh, Args: []string{"-c", "echo hi"},
		})
		assertPermissionDenied(t, err)
	})

	t.Run("Stop", func(t *testing.T) {
		t.Parallel()
		_, err := unknown.Stop(t.Context(), &jobworkerv1.StopRequest{JobId: "fake-id"})
		assertPermissionDenied(t, err)
	})

	t.Run("Status", func(t *testing.T) {
		t.Parallel()
		_, err := unknown.Status(t.Context(), &jobworkerv1.StatusRequest{JobId: "fake-id"})
		assertPermissionDenied(t, err)
	})

	t.Run("StreamOutput", func(t *testing.T) {
		t.Parallel()
		stream, err := unknown.StreamOutput(t.Context(), &jobworkerv1.StreamOutputRequest{JobId: "fake-id"})
		// With streaming RPCs the interceptor error may surface at first Recv.
		if err == nil && stream != nil {
			_, err = stream.Recv()
		}
		assertPermissionDenied(t, err)
	})
}

// TestGRPCNotFoundErrors verifies that Status, Stop, and StreamOutput return
// a NotFound status for a job ID that does not exist.
func TestGRPCNotFoundErrors(t *testing.T) {
	t.Parallel()
	env := newGRPCEnv(t)
	admin := env.newClient(t, env.adminCert)
	const missingID = "00000000-0000-0000-0000-000000000000"

	t.Run("Status", func(t *testing.T) {
		t.Parallel()
		_, err := admin.Status(t.Context(), &jobworkerv1.StatusRequest{JobId: missingID})
		assertCode(t, err, codes.NotFound)
	})

	t.Run("Stop", func(t *testing.T) {
		t.Parallel()
		_, err := admin.Stop(t.Context(), &jobworkerv1.StopRequest{JobId: missingID})
		assertCode(t, err, codes.NotFound)
	})

	t.Run("StreamOutput", func(t *testing.T) {
		t.Parallel()
		stream, err := admin.StreamOutput(t.Context(), &jobworkerv1.StreamOutputRequest{JobId: missingID})
		if err == nil && stream != nil {
			_, err = stream.Recv()
		}
		assertCode(t, err, codes.NotFound)
	})
}

// TestGRPCStopCompletedJob verifies that calling Stop on a job that has already
// completed returns FailedPrecondition which is the gRPC mapping of ErrJobNotRunning.
// This exercises a deliberate API contract: "stop" on a terminal job is a
// precondition failure, not a "not found" error.
func TestGRPCStopCompletedJob(t *testing.T) {
	t.Parallel()
	env := newGRPCEnv(t)
	sh := shellPath(t)
	admin := env.newClient(t, env.adminCert)

	startResp, err := admin.Start(t.Context(), &jobworkerv1.StartRequest{
		Command: sh,
		Args:    []string{"-c", "echo done"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	jobID := startResp.GetJobId()

	waitForGRPCState(t, admin, jobID, jobworkerv1.JobState_JOB_STATE_COMPLETED)

	_, err = admin.Stop(t.Context(), &jobworkerv1.StopRequest{JobId: jobID})
	assertCode(t, err, codes.FailedPrecondition)
}

// TestGRPCInvalidCommand verifies that Start returns InvalidArgument when the
// command is empty.
func TestGRPCInvalidCommand(t *testing.T) {
	t.Parallel()
	env := newGRPCEnv(t)
	admin := env.newClient(t, env.adminCert)

	_, err := admin.Start(t.Context(), &jobworkerv1.StartRequest{Command: ""})
	assertCode(t, err, codes.InvalidArgument)
}

// TestGRPCStreamCancellation verifies that cancelling the client-side context
// terminates the streaming RPC with a Canceled or DeadlineExceeded error.
func TestGRPCStreamCancellation(t *testing.T) {
	t.Parallel()
	env := newGRPCEnv(t)
	sh := shellPath(t)
	admin := env.newClient(t, env.adminCert)

	// Start a long-running job.
	startResp, err := admin.Start(t.Context(), &jobworkerv1.StartRequest{
		Command: sh,
		Args:    []string{"-c", "sleep 300"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	jobID := startResp.GetJobId()

	// Stop the job when the test ends, regardless of outcome.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = admin.Stop(ctx, &jobworkerv1.StopRequest{JobId: jobID})
	})

	waitForGRPCState(t, admin, jobID, jobworkerv1.JobState_JOB_STATE_RUNNING)

	// Open a stream with a cancellable context.
	streamCtx, cancel := context.WithCancel(t.Context())
	stream, err := admin.StreamOutput(streamCtx, &jobworkerv1.StreamOutputRequest{JobId: jobID})
	if err != nil {
		t.Fatalf("StreamOutput: %v", err)
	}

	// Start a background reader, then cancel.
	recvErr := make(chan error, 1)
	go func() {
		for {
			_, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
		}
	}()

	cancel()

	select {
	case err := <-recvErr:
		// gRPC-Go always surfaces a gRPC status error here.
		// An explicit context.Canceled cancel maps to codes.Canceled.
		if errors.Is(err, context.Canceled) {
			return
		}
		assertCode(t, err, codes.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stream to return after context cancellation")
	}
}

func assertPermissionDenied(t *testing.T, err error) {
	t.Helper()
	assertCode(t, err, codes.PermissionDenied)
}

func assertCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %v, got nil", want)
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error with code %v, got %v", want, err)
	}
	if st.Code() != want {
		t.Fatalf("expected status code %v, got %v: %s", want, st.Code(), st.Message())
	}
}
