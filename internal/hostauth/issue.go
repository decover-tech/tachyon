package hostauth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"time"
)

// This file issues hostlink certificates. It backs `tachyon hostcert` for
// development and bootstrap; a production fleet keeps the CA key in a
// managed key store and issues through it (docs/hostlink-mtls.md).

// PEMPair is a PEM certificate and its PEM (PKCS#8) private key.
type PEMPair struct {
	Cert []byte
	Key  []byte
}

// clockSkew backdates NotBefore so hosts with slightly slow clocks accept
// a certificate issued just now.
const clockSkew = 5 * time.Minute

// NewCA creates a self-signed ECDSA P-256 CA for hostlink.
func NewCA(name string, ttl time.Duration, now time.Time) (PEMPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return PEMPair{}, err
	}
	tmpl, err := template(name, ttl, now)
	if err != nil {
		return PEMPair{}, err
	}
	tmpl.IsCA = true
	tmpl.BasicConstraintsValid = true
	tmpl.MaxPathLenZero = true
	tmpl.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return PEMPair{}, err
	}
	return encode(der, key)
}

// IssueHost signs a client certificate identifying hostID in trustDomain.
func IssueHost(ca PEMPair, trustDomain, hostID string, ttl time.Duration, now time.Time) (PEMPair, error) {
	if !ValidHostID(hostID) {
		return PEMPair{}, fmt.Errorf("invalid host id %q", hostID)
	}
	if trustDomain == "" {
		trustDomain = DefaultTrustDomain
	}
	return issue(ca, hostID, ttl, now, func(t *x509.Certificate) {
		t.URIs = []*url.URL{HostURI(trustDomain, hostID)}
		t.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	})
}

// IssueServer signs the control plane's hostlink server certificate for
// the given DNS names and IP addresses.
func IssueServer(ca PEMPair, names []string, ttl time.Duration, now time.Time) (PEMPair, error) {
	if len(names) == 0 {
		return PEMPair{}, errors.New("server certificate needs at least one name")
	}
	return issue(ca, names[0], ttl, now, func(t *x509.Certificate) {
		for _, n := range names {
			if ip := net.ParseIP(n); ip != nil {
				t.IPAddresses = append(t.IPAddresses, ip)
			} else {
				t.DNSNames = append(t.DNSNames, n)
			}
		}
		t.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	})
}

func issue(ca PEMPair, cn string, ttl time.Duration, now time.Time, set func(*x509.Certificate)) (PEMPair, error) {
	pair, err := tls.X509KeyPair(ca.Cert, ca.Key)
	if err != nil {
		return PEMPair{}, fmt.Errorf("CA: %w", err)
	}
	caCert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return PEMPair{}, fmt.Errorf("CA: %w", err)
	}
	if !caCert.IsCA {
		return PEMPair{}, errors.New("CA certificate is not a CA")
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return PEMPair{}, errors.New("CA key cannot sign")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return PEMPair{}, err
	}
	tmpl, err := template(cn, ttl, now)
	if err != nil {
		return PEMPair{}, err
	}
	if tmpl.NotAfter.After(caCert.NotAfter) {
		tmpl.NotAfter = caCert.NotAfter
	}
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	set(tmpl)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, signer)
	if err != nil {
		return PEMPair{}, err
	}
	return encode(der, key)
}

func template(cn string, ttl time.Duration, now time.Time) (*x509.Certificate, error) {
	if ttl <= 0 {
		return nil, errors.New("ttl must be positive")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	return &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     now.Add(ttl),
	}, nil
}

func encode(der []byte, key *ecdsa.PrivateKey) (PEMPair, error) {
	k, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return PEMPair{}, err
	}
	return PEMPair{
		Cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Key:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: k}),
	}, nil
}
