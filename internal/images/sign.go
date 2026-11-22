package images

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
)

// Signed is a manifest with an ed25519 signature over its exact bytes.
// The manifest is carried as bytes (base64 in JSON), so reformatting the
// envelope can't change what was signed and hostd (Rust) verifies the same
// bytes without a shared canonical form. Hosts and the control plane only
// use images whose signature verifies against a trusted key, and whose
// files match the manifest.
type Signed struct {
	Manifest  []byte `json:"manifest"`
	KeyID     string `json:"key_id"`
	Signature []byte `json:"signature"`
}

// signingContext separates image signatures from any other use of the key.
const signingContext = "tachyon-base-image-manifest-v1\x00"

// KeyID names a public key: the first 16 hex digits of its SHA-256.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// Sign validates m and signs its encoding.
func Sign(m *Manifest, key ed25519.PrivateKey) (*Signed, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	body, err := m.Canonical()
	if err != nil {
		return nil, err
	}
	pub := key.Public().(ed25519.PublicKey)
	return &Signed{
		Manifest:  body,
		KeyID:     KeyID(pub),
		Signature: ed25519.Sign(key, append([]byte(signingContext), body...)),
	}, nil
}

// Verify checks the signature against the trusted keys (by KeyID) and
// returns the validated manifest.
func (s *Signed) Verify(trusted []ed25519.PublicKey) (*Manifest, error) {
	var pub ed25519.PublicKey
	for _, k := range trusted {
		if KeyID(k) == s.KeyID {
			pub = k
			break
		}
	}
	if pub == nil {
		return nil, fmt.Errorf("image signed by untrusted key %q", s.KeyID)
	}
	if !ed25519.Verify(pub, append([]byte(signingContext), s.Manifest...), s.Signature) {
		return nil, errors.New("image manifest signature does not verify")
	}
	// Unknown fields are rejected: everything signed is understood.
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(s.Manifest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("signed manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// ParsePrivateKey reads a PKCS#8 PEM ed25519 private key
// (openssl genpkey -algorithm ed25519).
func ParsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	b, _ := pem.Decode(data)
	if b == nil {
		return nil, errors.New("no PEM block in private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is %T, want ed25519", k)
	}
	return ek, nil
}

// ParsePublicKeys reads one or more PKIX PEM ed25519 public keys
// (openssl pkey -pubout). Several keys allow rotation.
func ParsePublicKeys(data []byte) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	for {
		var b *pem.Block
		b, data = pem.Decode(data)
		if b == nil {
			break
		}
		k, err := x509.ParsePKIXPublicKey(b.Bytes)
		if err != nil {
			return nil, err
		}
		ek, ok := k.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("public key is %T, want ed25519", k)
		}
		keys = append(keys, ek)
	}
	if len(keys) == 0 {
		return nil, errors.New("no PEM public keys found")
	}
	return keys, nil
}
