// Package hostauth authenticates hostd to the control plane over mutual
// TLS. Each host holds its own client certificate whose identity is a
// single URI SAN of the form
//
//	spiffe://<trust-domain>/host/<host_id>
//
// and the control plane accepts a hostlink stream only when that identity
// equals the host_id in the stream's Hello. See docs/hostlink-mtls.md for
// issuance and rotation.
package hostauth

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// DefaultTrustDomain is the SPIFFE trust domain used when none is set.
const DefaultTrustDomain = "tachyon"

const hostPathPrefix = "/host/"

// HostURI is the URI SAN that identifies hostID in trustDomain.
func HostURI(trustDomain, hostID string) *url.URL {
	return &url.URL{Scheme: "spiffe", Host: trustDomain, Path: hostPathPrefix + hostID}
}

// ValidHostID reports whether id is usable as a host identity: non-empty,
// at most 128 bytes, and only [A-Za-z0-9._-].
func ValidHostID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// HostID returns the host identity carried by cert. The certificate must
// have exactly one URI SAN, it must be a host URI in trustDomain, and it
// must be valid for client authentication.
func HostID(cert *x509.Certificate, trustDomain string) (string, error) {
	if cert == nil {
		return "", errors.New("no certificate")
	}
	if !hasClientAuth(cert) {
		return "", errors.New("certificate not valid for client auth")
	}
	if len(cert.URIs) != 1 {
		return "", fmt.Errorf("certificate has %d URI SANs, want exactly 1", len(cert.URIs))
	}
	u := cert.URIs[0]
	if u.Scheme != "spiffe" || u.Host != trustDomain || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("URI SAN %q is not in trust domain %q", u, trustDomain)
	}
	id, ok := strings.CutPrefix(u.Path, hostPathPrefix)
	if !ok || !ValidHostID(id) {
		return "", fmt.Errorf("URI SAN %q is not a host identity", u)
	}
	return id, nil
}

func hasClientAuth(cert *x509.Certificate) bool {
	if len(cert.ExtKeyUsage) == 0 {
		return false
	}
	for _, u := range cert.ExtKeyUsage {
		if u == x509.ExtKeyUsageClientAuth || u == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}

// PeerAuthenticator checks a gRPC stream's verified TLS client certificate
// against the host_id the stream claims in Hello.
type PeerAuthenticator struct {
	// TrustDomain defaults to DefaultTrustDomain.
	TrustDomain string
}

// AuthenticateHost returns nil only if ctx's peer presented a certificate
// chain that the TLS layer verified against the client CA and whose leaf
// identifies hostID. It also returns when the chain expires, so the caller
// can end the stream and make the host reconnect with a fresh certificate.
func (a PeerAuthenticator) AuthenticateHost(ctx context.Context, hostID string) (time.Time, error) {
	td := a.TrustDomain
	if td == "" {
		td = DefaultTrustDomain
	}
	p, ok := peer.FromContext(ctx)
	if !ok {
		return time.Time{}, errors.New("no peer on stream")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return time.Time{}, errors.New("stream is not TLS")
	}
	chains := info.State.VerifiedChains
	if len(chains) == 0 || len(chains[0]) == 0 {
		return time.Time{}, errors.New("no verified client certificate")
	}
	id, err := HostID(chains[0][0], td)
	if err != nil {
		return time.Time{}, err
	}
	if id != hostID {
		return time.Time{}, fmt.Errorf("certificate is for host %q, Hello claims %q", id, hostID)
	}
	expires := chains[0][0].NotAfter
	for _, c := range chains[0][1:] {
		if c.NotAfter.Before(expires) {
			expires = c.NotAfter
		}
	}
	return expires, nil
}
