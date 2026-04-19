package grpcserver

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

var (
	// ErrLoadServerCertificate is returned when the server certificate/key
	// pair cannot be loaded from disk.
	ErrLoadServerCertificate = errors.New("load server certificate/key")
	// ErrReadCACertificate is returned when the CA certificate file cannot
	// be read from disk.
	ErrReadCACertificate = errors.New("read CA certificate")
	// ErrParseCACertificate is returned when the CA certificate file contains
	// no valid PEM blocks.
	ErrParseCACertificate = errors.New("parse CA certificate")
)

// ServerTLSConfig builds a *tls.Config for a mutual-TLS server. Clients must
// present a certificate signed by the CA at caFile; TLS 1.3 is the minimum
// version.
func ServerTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	serverCert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLoadServerCertificate, err)
	}

	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrReadCACertificate, err)
	}

	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("%w: no valid PEM blocks found", ErrParseCACertificate)
	}

	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		Certificates: []tls.Certificate{serverCert},
	}, nil
}
