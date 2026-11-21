package hostauth

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// ServerFiles are the PEM files the control plane serves hostlink with.
type ServerFiles struct {
	Cert     string // server certificate chain
	Key      string // server private key
	ClientCA string // CA bundle that host certificates must chain to
}

// ServerTLS returns a TLS config for the hostlink listener that requires
// and verifies a client certificate. The files are read again on every
// handshake, so replacing them on disk rotates the server certificate or
// the client CA bundle without a restart. They are also read once here so a
// bad path fails at startup.
func ServerTLS(f ServerFiles) (*tls.Config, error) {
	if f.Cert == "" || f.Key == "" || f.ClientCA == "" {
		return nil, errors.New("hostlink TLS needs a server cert, key and client CA")
	}
	if _, err := f.load(); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return f.load()
		},
	}, nil
}

func (f ServerFiles) load() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(f.Cert, f.Key)
	if err != nil {
		return nil, fmt.Errorf("hostlink server cert: %w", err)
	}
	pool, err := loadPool(f.ClientCA)
	if err != nil {
		return nil, fmt.Errorf("hostlink client CA: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		NextProtos:   []string{"h2"},
	}, nil
}

func loadPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s: no PEM certificates", path)
	}
	return pool, nil
}
