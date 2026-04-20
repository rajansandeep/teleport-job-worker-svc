package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os/signal"
	"syscall"

	jobworkerv1 "github.com/rajansandeep/teleport-job-worker-svc/gen/jobworker/v1"
	"github.com/rajansandeep/teleport-job-worker-svc/internal/grpcserver"
	"github.com/rajansandeep/teleport-job-worker-svc/internal/worker"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	var (
		listenAddr = flag.String("listen", ":50051", "address to listen on")
		certFile   = flag.String("cert", "", "path to server certificate PEM file")
		keyFile    = flag.String("key", "", "path to server private key PEM file")
		caFile     = flag.String("ca", "", "path to CA certificate PEM file")
	)
	flag.Parse()

	if *certFile == "" || *keyFile == "" || *caFile == "" {
		log.Fatal("all of --cert, --key, and --ca are required")
	}

	tlsConfig, err := grpcserver.ServerTLSConfig(*certFile, *keyFile, *caFile)
	if err != nil {
		log.Fatalf("build TLS config: %v", err)
	}

	lis, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("listen on %q: %v", *listenAddr, err)
	}
	defer lis.Close()

	w := worker.NewWorker()
	srv := grpcserver.NewServer(w)

	grpcSrv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsConfig)),
		grpc.UnaryInterceptor(grpcserver.UnaryAuthInterceptor()),
		grpc.StreamInterceptor(grpcserver.StreamAuthInterceptor()),
	)

	jobworkerv1.RegisterJobWorkerServer(grpcSrv, srv)

	// Shut down the server when SIGTERM or SIGINT is received.
	// GracefulStop lets active RPCs finish, but a StreamOutput call can stay
	// open as long as its job is still running or the client is still connected.
	// That means shutdown can wait forever in that case. SIGKILL will still
	// force the process to exit. Running jobs are not cleaned up yet because
	// Worker.Shutdown is not implemented (see TODO in worker.go).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	serveReturned := make(chan struct{})
	defer close(serveReturned)

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-ctx.Done():
			log.Println("shutdown signal received; stopping server")
			grpcSrv.GracefulStop()
		case <-serveReturned:
			// Serve returned on its own. Nothing to do.
		}
	}()

	log.Printf("jobworker-server listening on %s", *listenAddr)
	if err := grpcSrv.Serve(lis); err != nil {
		log.Fatalf("serve gRPC: %v", err)
	}
	<-shutdownDone
	log.Println("server stopped")
}
