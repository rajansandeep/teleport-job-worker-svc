package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	jobworkerv1 "github.com/rajansandeep/teleport-job-worker-svc/gen/jobworker/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

const (
	defaultAddr = "localhost:50051"

	envCert = "JOBWORKER_CERT"
	envKey  = "JOBWORKER_KEY"
	envCA   = "JOBWORKER_CA"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	globalFlags := flag.NewFlagSet("jobworker-cli", flag.ContinueOnError)
	globalFlags.SetOutput(os.Stderr)

	addrFlag := globalFlags.String("addr", defaultAddr, "gRPC server address")
	certFlag := globalFlags.String("cert", "", "path to client certificate PEM file")
	keyFlag := globalFlags.String("key", "", "path to client private key PEM file")
	caFlag := globalFlags.String("ca", "", "path to CA certificate PEM file")

	globalFlags.Usage = func() {
		printUsage(os.Stderr)
	}

	if err := globalFlags.Parse(args); err != nil {
		// -h / --help: flag package already printed usage. Exit cleanly.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	rest := globalFlags.Args()
	if len(rest) == 0 {
		printUsage(os.Stderr)
		return 2
	}

	subcommand := rest[0]
	subArgs := rest[1:]

	if subcommand == "help" {
		printUsage(os.Stdout)
		return 0
	}

	certPath, keyPath, caPath, err := resolveTLSPaths(*certFlag, *keyFlag, *caFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	conn, err := newClientConn(*addrFlag, certPath, keyPath, caPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: connect: %v\n", err)
		return 1
	}
	defer conn.Close()

	client := jobworkerv1.NewJobWorkerClient(conn)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var cmdErr error
	switch subcommand {
	case "start":
		cmdErr = runStart(ctx, client, subArgs)
	case "stop":
		cmdErr = runStop(ctx, client, subArgs)
	case "status":
		cmdErr = runStatus(ctx, client, subArgs)
	case "stream":
		cmdErr = runStream(ctx, client, subArgs)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown subcommand %q\n\n", subcommand)
		printUsage(os.Stderr)
		return 2
	}

	if cmdErr != nil {
		// Ctrl-C or SIGTERM: the context was cancelled by the user. Exit cleanly.
		if ctx.Err() != nil {
			return 0
		}
		printCommandError(cmdErr)
		return 1
	}

	return 0
}

func runStart(ctx context.Context, client jobworkerv1.JobWorkerClient, args []string) error {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return errors.New(`start requires a command after "--"`)
	}

	resp, err := client.Start(ctx, &jobworkerv1.StartRequest{
		Command: args[0],
		Args:    args[1:],
	})
	if err != nil {
		return err
	}

	fmt.Println(resp.GetJobId())
	return nil
}

func runStop(ctx context.Context, client jobworkerv1.JobWorkerClient, args []string) error {
	if len(args) != 1 {
		return errors.New("stop requires exactly 1 argument: <job-id>")
	}

	_, err := client.Stop(ctx, &jobworkerv1.StopRequest{JobId: args[0]})
	return err
}

func runStatus(ctx context.Context, client jobworkerv1.JobWorkerClient, args []string) error {
	if len(args) != 1 {
		return errors.New("status requires exactly 1 argument: <job-id>")
	}

	resp, err := client.Status(ctx, &jobworkerv1.StatusRequest{JobId: args[0]})
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "ID:\t%s\n", resp.GetJobId())
	fmt.Fprintf(w, "Command:\t%s\n", joinCommand(resp.GetCommand(), resp.GetArgs()))
	fmt.Fprintf(w, "State:\t%s\n", formatState(resp.GetState()))
	fmt.Fprintf(w, "Exit Code:\t%s\n", formatExitCode(resp.GetState(), resp.ExitCode))
	if resp.GetError() != "" {
		fmt.Fprintf(w, "Error:\t%s\n", resp.GetError())
	}
	return w.Flush()
}

func runStream(ctx context.Context, client jobworkerv1.JobWorkerClient, args []string) error {
	if len(args) != 1 {
		return errors.New("stream requires exactly 1 argument: <job-id>")
	}

	stream, err := client.StreamOutput(ctx, &jobworkerv1.StreamOutputRequest{JobId: args[0]})
	if err != nil {
		return err
	}

	for {
		chunk, err := stream.Recv()
		if err == nil {
			if _, writeErr := os.Stdout.Write(chunk.GetData()); writeErr != nil {
				return writeErr
			}
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
}

func newClientConn(addr, certFile, keyFile, caFile string) (*grpc.ClientConn, error) {
	tlsConfig, err := clientTLSConfig(certFile, keyFile, caFile)
	if err != nil {
		return nil, err
	}

	return grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
	)
}

func clientTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	clientCert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client certificate/key: %w", err)
	}

	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA certificate: %w", err)
	}

	rootCAs := x509.NewCertPool()
	if !rootCAs.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("parse CA certificate: no valid PEM blocks found")
	}

	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      rootCAs,
		Certificates: []tls.Certificate{clientCert},
	}, nil
}

func resolveTLSPaths(certFlag, keyFlag, caFlag string) (string, string, string, error) {
	paths := defaultTLSPaths()

	certPath := resolvePath(certFlag, envCert, paths.cert)
	keyPath := resolvePath(keyFlag, envKey, paths.key)
	caPath := resolvePath(caFlag, envCA, paths.ca)

	if certPath == "" {
		return "", "", "", fmt.Errorf("missing client cert; checked --cert, %s, and %s", envCert, paths.cert)
	}
	if keyPath == "" {
		return "", "", "", fmt.Errorf("missing client key; checked --key, %s, and %s", envKey, paths.key)
	}
	if caPath == "" {
		return "", "", "", fmt.Errorf("missing CA cert; checked --ca, %s, and %s", envCA, paths.ca)
	}

	if err := validateReadableFile(certPath, "client cert"); err != nil {
		return "", "", "", err
	}
	if err := validateReadableFile(keyPath, "client key"); err != nil {
		return "", "", "", err
	}
	if err := validateReadableFile(caPath, "CA cert"); err != nil {
		return "", "", "", err
	}

	return certPath, keyPath, caPath, nil
}

func resolvePath(flagVal, envKey, defaultPath string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	if _, err := os.Stat(defaultPath); err == nil {
		return defaultPath
	}
	return ""
}

type tlsPaths struct {
	cert string
	key  string
	ca   string
}

func defaultTLSPaths() tlsPaths {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return tlsPaths{}
	}

	base := filepath.Join(home, ".jobworker", "certs")
	return tlsPaths{
		cert: filepath.Join(base, "client.crt"),
		key:  filepath.Join(base, "client.key"),
		ca:   filepath.Join(base, "ca.crt"),
	}
}

// validateReadableFile performs a best-effort pre-flight check before the TLS load.
// TODO: A TOCTOU window exists between this check and clientTLSConfig. It is
// acceptable for a CLI tool operating on local certificate files.
func validateReadableFile(path, label string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%s %q: %w", label, path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("%s %q: %w", label, path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s %q is a directory, not a file", label, path)
	}

	return nil
}

func joinCommand(command string, args []string) string {
	if len(args) == 0 {
		return command
	}
	return command + " " + strings.Join(args, " ")
}

func formatState(state jobworkerv1.JobState) string {
	name := state.String()
	name = strings.TrimPrefix(name, "JOB_STATE_")
	if name == "UNSPECIFIED" {
		return "-"
	}
	// Convert "RUNNING" -> "Running" for human-readable output.
	return strings.ToUpper(name[:1]) + strings.ToLower(name[1:])
}

// formatExitCode accepts the proto optional field directly so that a nil
// pointer (field not set by the server) is rendered as "?" rather than "0".
func formatExitCode(state jobworkerv1.JobState, exitCode *int32) string {
	switch state {
	case jobworkerv1.JobState_JOB_STATE_COMPLETED, jobworkerv1.JobState_JOB_STATE_FAILED:
		if exitCode == nil {
			return "?"
		}
		return fmt.Sprintf("%d", *exitCode)
	default:
		return "-"
	}
}

func printCommandError(err error) {
	// status.FromError returns ok=true for any error, mapping unknown errors to
	// codes.Unknown. Only use the gRPC status message for genuine gRPC errors.
	if st, ok := status.FromError(err); ok && st.Code() != codes.Unknown {
		fmt.Fprintf(os.Stderr, "error: %s\n", st.Message())
		return
	}
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `jobworker-cli [global flags] <command> [args]

Global flags:
  --addr   gRPC server address (default %q)
  --cert   path to client certificate PEM file
  --key    path to client private key PEM file
  --ca     path to CA certificate PEM file

Certificate resolution precedence:
  1. Explicit flags (--cert, --key, --ca)
  2. Environment variables (%s, %s, %s)
  3. Default paths under ~/.jobworker/certs/

Commands:
  start -- <command> [args...]   Start a new job
  stop <job-id>                  Stop a running job
  status <job-id>                Show job status
  stream <job-id>                Stream raw process output to stdout
  help                           Show this help

Examples:
  ./jobworker-cli --cert certs/admin.crt --key certs/admin.key --ca certs/ca.crt start -- ls -la /tmp
  ./jobworker-cli stream $(jobworker-cli start -- ls -la /tmp)
  ./jobworker-cli status 550e8400-e29b-41d4-a716-446655440000
  ./jobworker-cli stop 550e8400-e29b-41d4-a716-446655440000
`, defaultAddr, envCert, envKey, envCA)
}
