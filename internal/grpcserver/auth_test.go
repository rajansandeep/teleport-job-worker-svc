package grpcserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// assertPermissionDenied is a test helper that fails the test if err is nil
// or is not a gRPC PermissionDenied status error.
func assertPermissionDenied(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected PermissionDenied error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", st.Code())
	}
}

func TestVerifiedLeafCertificate(t *testing.T) {
	t.Run("NilChains", func(t *testing.T) {
		_, err := verifiedLeafCertificate(nil)
		if err == nil {
			t.Fatal("expected error for nil verified chains")
		}
	})

	t.Run("EmptyInnerSlice", func(t *testing.T) {
		_, err := verifiedLeafCertificate([][]*x509.Certificate{{}})
		if err == nil {
			t.Fatal("expected error for chain with empty inner slice")
		}
	})

	t.Run("Success", func(t *testing.T) {
		cert := &x509.Certificate{
			Subject: pkix.Name{CommonName: "admin"},
		}
		got, err := verifiedLeafCertificate([][]*x509.Certificate{{cert}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != cert {
			t.Fatal("expected returned certificate pointer to match input")
		}
	})
}

func TestAuthorize(t *testing.T) {
	cases := []struct {
		name    string
		cn      string
		method  string
		wantErr bool
	}{
		// admin can call all methods
		{"admin/Start", "admin", "/jobworker.v1.JobWorker/Start", false},
		{"admin/Stop", "admin", "/jobworker.v1.JobWorker/Stop", false},
		{"admin/Status", "admin", "/jobworker.v1.JobWorker/Status", false},
		{"admin/StreamOutput", "admin", "/jobworker.v1.JobWorker/StreamOutput", false},
		// viewer is blocked from write methods
		{"viewer/Start", "viewer", "/jobworker.v1.JobWorker/Start", true},
		{"viewer/Stop", "viewer", "/jobworker.v1.JobWorker/Stop", true},
		// viewer can call read methods
		{"viewer/Status", "viewer", "/jobworker.v1.JobWorker/Status", false},
		{"viewer/StreamOutput", "viewer", "/jobworker.v1.JobWorker/StreamOutput", false},
		// unknown CN is blocked from all methods
		{"unknown/Start", "nobody", "/jobworker.v1.JobWorker/Start", true},
		{"unknown/Stop", "nobody", "/jobworker.v1.JobWorker/Stop", true},
		{"unknown/Status", "nobody", "/jobworker.v1.JobWorker/Status", true},
		{"unknown/StreamOutput", "nobody", "/jobworker.v1.JobWorker/StreamOutput", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := authorize(tc.cn, tc.method)
			if tc.wantErr {
				assertPermissionDenied(t, err)
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestMethodName(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"/jobworker.v1.JobWorker/Start", "Start"}, // normal full method path
		{"Start", "Start"},                         // no slash: returns the input unchanged
		{"", ""},                                   // empty string
		{"/", ""},                                  // trailing slash only
		{"/a/b/c", "c"},                            // multiple slashes: last segment wins
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got := methodName(tc.input)
			if got != tc.want {
				t.Fatalf("methodName(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f *fakeServerStream) Context() context.Context {
	return f.ctx
}

func TestInterceptorRejectsMissingPeer(t *testing.T) {
	t.Run("Unary", func(t *testing.T) {
		interceptor := UnaryAuthInterceptor()

		handlerCalled := false
		_, err := interceptor(
			t.Context(),
			struct{}{},
			&grpc.UnaryServerInfo{FullMethod: "/jobworker.v1.JobWorker/Start"},
			func(ctx context.Context, req any) (any, error) {
				handlerCalled = true
				return nil, nil
			},
		)

		if handlerCalled {
			t.Fatal("handler should not be called when peer info is missing")
		}
		assertPermissionDenied(t, err)
	})

	t.Run("Stream", func(t *testing.T) {
		interceptor := StreamAuthInterceptor()

		handlerCalled := false
		err := interceptor(
			nil,
			&fakeServerStream{ctx: t.Context()},
			&grpc.StreamServerInfo{FullMethod: "/jobworker.v1.JobWorker/StreamOutput"},
			func(srv any, ss grpc.ServerStream) error {
				handlerCalled = true
				return nil
			},
		)

		if handlerCalled {
			t.Fatal("handler should not be called when peer info is missing")
		}
		assertPermissionDenied(t, err)
	})
}

func TestAuthenticatedClientCNMissingTLSInfo(t *testing.T) {
	ctx := peer.NewContext(t.Context(), &peer.Peer{})
	_, err := authenticatedClientCN(ctx)
	assertPermissionDenied(t, err)
}

func TestAuthenticatedClientCNEmptyCN(t *testing.T) {
	// Construct a peer with valid TLS info but a certificate that has no CN.
	cert := &x509.Certificate{Subject: pkix.Name{}}
	tlsInfo := credentials.TLSInfo{
		State: tls.ConnectionState{
			VerifiedChains: [][]*x509.Certificate{{cert}},
		},
	}
	ctx := peer.NewContext(t.Context(), &peer.Peer{AuthInfo: tlsInfo})
	_, err := authenticatedClientCN(ctx)
	assertPermissionDenied(t, err)
}
