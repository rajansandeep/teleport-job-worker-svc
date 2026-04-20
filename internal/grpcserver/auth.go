package grpcserver

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type role int

const (
	roleUnspecified role = iota
	roleAdmin
	roleViewer
)

func (r role) String() string {
	switch r {
	case roleUnspecified:
		return "unspecified"
	case roleAdmin:
		return "admin"
	case roleViewer:
		return "viewer"
	default:
		return fmt.Sprintf("Unknown[%d]", r)
	}
}

var roles = map[string]role{
	"admin":  roleAdmin,
	"viewer": roleViewer,
}

var methodPermissions = map[string]map[role]bool{
	"/jobworker.v1.JobWorker/Start": {
		roleAdmin: true,
	},
	"/jobworker.v1.JobWorker/Stop": {
		roleAdmin: true,
	},
	"/jobworker.v1.JobWorker/Status": {
		roleAdmin:  true,
		roleViewer: true,
	},
	"/jobworker.v1.JobWorker/StreamOutput": {
		roleAdmin:  true,
		roleViewer: true,
	},
}

// UnaryAuthInterceptor authenticates the client certificate and checks
// whether the caller is allowed to use the requested unary RPC.
func UnaryAuthInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		cn, err := authenticatedClientCN(ctx)
		if err != nil {
			return nil, err
		}
		if err := authorize(cn, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamAuthInterceptor authenticates the client certificate and checks
// whether the caller is allowed to use the requested streaming RPC.
func StreamAuthInterceptor() grpc.StreamServerInterceptor {
	return func(
		srv any,
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		cn, err := authenticatedClientCN(ss.Context())
		if err != nil {
			return err
		}
		if err := authorize(cn, info.FullMethod); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

func authenticatedClientCN(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", status.Error(codes.PermissionDenied, "missing peer information")
	}

	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", status.Error(codes.PermissionDenied, "missing TLS peer information")
	}

	cert, err := verifiedLeafCertificate(tlsInfo.State.VerifiedChains)
	if err != nil {
		return "", status.Error(codes.PermissionDenied, err.Error())
	}

	if cert.Subject.CommonName == "" {
		return "", status.Error(codes.PermissionDenied, "client certificate missing common name")
	}

	return cert.Subject.CommonName, nil
}

func verifiedLeafCertificate(verifiedChains [][]*x509.Certificate) (*x509.Certificate, error) {
	if len(verifiedChains) == 0 || len(verifiedChains[0]) == 0 {
		return nil, errors.New("no verified client certificate available")
	}
	return verifiedChains[0][0], nil
}

func authorize(cn, fullMethod string) error {
	r, ok := roles[cn]
	if !ok {
		r = roleUnspecified
	}

	allowedRoles, ok := methodPermissions[fullMethod]
	if !ok || !allowedRoles[r] {
		return status.Errorf(
			codes.PermissionDenied,
			"permission denied: role %q cannot call %s",
			r.String(),
			methodName(fullMethod),
		)
	}

	return nil
}

func methodName(fullMethod string) string {
	if i := strings.LastIndex(fullMethod, "/"); i >= 0 {
		return fullMethod[i+1:]
	}
	return fullMethod
}
