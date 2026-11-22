package main

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/hostauth"
)

func TestHostcertIssuesChain(t *testing.T) {
	dir := t.TempDir()
	at := func(n string) string { return filepath.Join(dir, n) }
	now := time.Now()
	if err := runHostcert("ca", at("ca"), "", "", "", "", time.Hour, now); err != nil {
		t.Fatal(err)
	}
	if err := runHostcert("host", at("h1"), at("ca"), "h1", "", "tachyon", time.Hour, now); err != nil {
		t.Fatal(err)
	}
	if err := runHostcert("server", at("srv"), at("ca"), "", "cp.internal, 10.0.0.1", "", time.Hour, now); err != nil {
		t.Fatal(err)
	}
	// Never overwrites.
	if err := runHostcert("ca", at("ca"), "", "", "", "", time.Hour, now); err == nil {
		t.Fatal("overwrote existing CA")
	}
	if fi, err := os.Stat(at("h1.key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode: %v %v", fi.Mode(), err)
	}

	ca, _ := readPair(at("ca"))
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.Cert)
	host, _ := readPair(at("h1"))
	b, _ := pem.Decode(host.Cert)
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatal(err)
	}
	if id, err := hostauth.HostID(cert, "tachyon"); err != nil || id != "h1" {
		t.Fatalf("HostID = %q, %v", id, err)
	}
}
