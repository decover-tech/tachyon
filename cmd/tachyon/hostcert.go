package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/decover-tech/tachyon/internal/hostauth"
)

// hostcert issues hostlink mTLS material for development and bootstrap.
// It writes <prefix>.crt and <prefix>.key (mode 0600) and never overwrites
// an existing file.
func hostcert(cmd string, args []string) {
	defaultTTL, ok := map[string]time.Duration{
		"ca":     365 * 24 * time.Hour,
		"host":   30 * 24 * time.Hour,
		"server": 90 * 24 * time.Hour,
	}[cmd]
	if !ok {
		usage()
	}
	fs := flag.NewFlagSet("hostcert "+cmd, flag.ExitOnError)
	out := fs.String("out", "", "output path prefix (writes <prefix>.crt and <prefix>.key)")
	caPrefix := fs.String("ca", "", "CA path prefix (reads <prefix>.crt and <prefix>.key)")
	id := fs.String("id", "", "host id (must equal hostd's TACHYON_HOST_ID)")
	names := fs.String("name", "", "comma-separated DNS names or IPs for the server certificate")
	trustDomain := fs.String("trust-domain", hostauth.DefaultTrustDomain, "SPIFFE trust domain")
	ttl := fs.Duration("ttl", defaultTTL, "certificate lifetime")
	fs.Parse(args)
	if *out == "" || (cmd != "ca" && *caPrefix == "") {
		usage()
	}
	if err := runHostcert(cmd, *out, *caPrefix, *id, *names, *trustDomain, *ttl, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s.crt and %s.key\n", *out, *out)
}

func runHostcert(cmd, out, caPrefix, id, names, trustDomain string, ttl time.Duration, now time.Time) error {
	var pair hostauth.PEMPair
	var err error
	if cmd == "ca" {
		pair, err = hostauth.NewCA("tachyon hostlink CA", ttl, now)
	} else {
		var ca hostauth.PEMPair
		if ca, err = readPair(caPrefix); err != nil {
			return err
		}
		if cmd == "host" {
			pair, err = hostauth.IssueHost(ca, trustDomain, id, ttl, now)
		} else {
			pair, err = hostauth.IssueServer(ca, splitNames(names), ttl, now)
		}
	}
	if err != nil {
		return err
	}
	return writePair(out, pair)
}

func splitNames(s string) []string {
	var out []string
	for _, n := range strings.Split(s, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

func readPair(prefix string) (hostauth.PEMPair, error) {
	c, err := os.ReadFile(prefix + ".crt")
	if err != nil {
		return hostauth.PEMPair{}, err
	}
	k, err := os.ReadFile(prefix + ".key")
	if err != nil {
		return hostauth.PEMPair{}, err
	}
	return hostauth.PEMPair{Cert: c, Key: k}, nil
}

func writePair(prefix string, p hostauth.PEMPair) error {
	if err := writeNew(prefix+".key", p.Key, 0o600); err != nil {
		return err
	}
	return writeNew(prefix+".crt", p.Cert, 0o644)
}

func writeNew(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
