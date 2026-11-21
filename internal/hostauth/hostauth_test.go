package hostauth_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/controlplane"
	"github.com/decover-tech/tachyon/internal/hostauth"
	"github.com/decover-tech/tachyon/internal/scheduler"
)

// must panics on err; test setup only.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func parse(t *testing.T, p hostauth.PEMPair) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode(p.Cert)
	return must(x509.ParseCertificate(b.Bytes))
}

func TestHostIDFromIssuedCert(t *testing.T) {
	now := time.Now()
	ca := must(hostauth.NewCA("ca", time.Hour, now))
	host := must(hostauth.IssueHost(ca, "", "host-1.fra", time.Hour, now))
	cert := parse(t, host)
	if got := must(hostauth.HostID(cert, hostauth.DefaultTrustDomain)); got != "host-1.fra" {
		t.Fatalf("HostID = %q", got)
	}
	if _, err := hostauth.HostID(cert, "other"); err == nil {
		t.Error("wrong trust domain accepted")
	}
	// A server certificate is not a host identity.
	srv := parse(t, must(hostauth.IssueServer(ca, []string{"cp.internal"}, time.Hour, now)))
	if _, err := hostauth.HostID(srv, hostauth.DefaultTrustDomain); err == nil {
		t.Error("server cert accepted as host identity")
	}
}

func TestHostIDRejectsMalformedSANs(t *testing.T) {
	base := func(uris ...string) *x509.Certificate {
		c := &x509.Certificate{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
		for _, u := range uris {
			c.URIs = append(c.URIs, must(url.Parse(u)))
		}
		return c
	}
	for name, c := range map[string]*x509.Certificate{
		"no SAN":       base(),
		"two SANs":     base("spiffe://tachyon/host/a", "spiffe://tachyon/host/b"),
		"wrong scheme": base("https://tachyon/host/a"),
		"not a host":   base("spiffe://tachyon/workload/a"),
		"empty id":     base("spiffe://tachyon/host/"),
		"nested id":    base("spiffe://tachyon/host/a/b"),
		"query":        base("spiffe://tachyon/host/a?x=1"),
		"no EKU":       {URIs: base("spiffe://tachyon/host/a").URIs},
	} {
		if id, err := hostauth.HostID(c, "tachyon"); err == nil {
			t.Errorf("%s: accepted as %q", name, id)
		}
	}
}

func TestIssueValidation(t *testing.T) {
	now := time.Now()
	ca := must(hostauth.NewCA("ca", time.Hour, now))
	if _, err := hostauth.IssueHost(ca, "", "bad/id", time.Hour, now); err == nil {
		t.Error("bad host id issued")
	}
	if _, err := hostauth.IssueServer(ca, nil, time.Hour, now); err == nil {
		t.Error("server cert without names issued")
	}
	leaf := must(hostauth.IssueHost(ca, "", "h", time.Hour, now))
	if _, err := hostauth.IssueHost(leaf, "", "h2", time.Hour, now); err == nil {
		t.Error("non-CA used as issuer")
	}
	// Leaf lifetime is capped at the CA's.
	long := parse(t, must(hostauth.IssueHost(ca, "", "h", 1000*time.Hour, now)))
	if long.NotAfter.After(parse(t, ca).NotAfter) {
		t.Error("leaf outlives CA")
	}
}

// pki writes a CA, a server cert and returns paths plus the CA itself.
type pki struct {
	dir string
	ca  hostauth.PEMPair
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	now := time.Now()
	p := &pki{dir: t.TempDir(), ca: must(hostauth.NewCA("ca", time.Hour, now))}
	srv := must(hostauth.IssueServer(p.ca, []string{"localhost", "127.0.0.1"}, time.Hour, now))
	p.write(t, "ca.crt", p.ca.Cert)
	p.write(t, "server.crt", srv.Cert)
	p.write(t, "server.key", srv.Key)
	return p
}

func (p *pki) write(t *testing.T, name string, b []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(p.dir, name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (p *pki) path(name string) string { return filepath.Join(p.dir, name) }

// recordingAuth wraps the real authenticator and reports each decision.
type recordingAuth struct {
	hostauth.PeerAuthenticator
	got chan error
}

func (a recordingAuth) AuthenticateHost(ctx context.Context, id string) (time.Time, error) {
	exp, err := a.PeerAuthenticator.AuthenticateHost(ctx, id)
	a.got <- err
	return exp, err
}

func serve(t *testing.T, p *pki) (addr string, decisions chan error) {
	t.Helper()
	cfg := must(hostauth.ServerTLS(hostauth.ServerFiles{
		Cert: p.path("server.crt"), Key: p.path("server.key"), ClientCA: p.path("ca.crt"),
	}))
	decisions = make(chan error, 4)
	link := &controlplane.HostLink{
		Core: &controlplane.Core{Scheduler: scheduler.New(nil)},
		Auth: recordingAuth{got: decisions},
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(cfg)))
	hostlinkv1.RegisterHostLinkServiceServer(srv, link)
	lis := must(net.Listen("tcp", "127.0.0.1:0"))
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), decisions
}

// dial opens a Link stream with the given client cert and sends Hello.
func dial(t *testing.T, addr string, p *pki, client *hostauth.PEMPair, hostID string) (hostlinkv1.HostLinkService_LinkClient, error) {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(p.ca.Cert)
	cfg := &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13}
	if client != nil {
		kp := must(tls.X509KeyPair(client.Cert, client.Key))
		cfg.Certificates = []tls.Certificate{kp}
	}
	conn := must(grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg))))
	t.Cleanup(func() { conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := hostlinkv1.NewHostLinkServiceClient(conn).Link(ctx)
	if err != nil {
		return nil, err
	}
	err = stream.Send(&hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_Hello{Hello: &hostlinkv1.Hello{
		HostId: hostID, Region: "r", Vcpu: 8, MemoryGb: 32,
	}}})
	return stream, err
}

func TestMTLSHostLink(t *testing.T) {
	p := newPKI(t)
	addr, decisions := serve(t, p)
	now := time.Now()
	h1 := must(hostauth.IssueHost(p.ca, "", "h1", time.Hour, now))

	t.Run("matching identity accepted", func(t *testing.T) {
		if _, err := dial(t, addr, p, &h1, "h1"); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-decisions:
			if err != nil {
				t.Fatalf("rejected: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no auth decision")
		}
	})

	t.Run("cert for another host rejected", func(t *testing.T) {
		stream, err := dial(t, addr, p, &h1, "h2")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("Recv = %v, want PermissionDenied", err)
		}
		if err := <-decisions; err == nil {
			t.Fatal("mismatch accepted")
		}
	})

	t.Run("no client cert fails handshake", func(t *testing.T) {
		stream, err := dial(t, addr, p, nil, "h1")
		if err == nil {
			_, err = stream.Recv()
		}
		if err == nil {
			t.Fatal("stream without client cert succeeded")
		}
	})

	t.Run("cert from another CA fails handshake", func(t *testing.T) {
		other := must(hostauth.NewCA("other", time.Hour, now))
		rogue := must(hostauth.IssueHost(other, "", "h1", time.Hour, now))
		stream, err := dial(t, addr, p, &rogue, "h1")
		if err == nil {
			_, err = stream.Recv()
		}
		if err == nil {
			t.Fatal("stream with untrusted cert succeeded")
		}
	})

	select {
	case err := <-decisions:
		t.Fatalf("handshake failures reached the authenticator: %v", err)
	default:
	}
}

func TestServerTLSReloadsClientCA(t *testing.T) {
	p := newPKI(t)
	addr, decisions := serve(t, p)
	now := time.Now()

	// Rotate to a new CA bundle that trusts both old and new client CAs.
	next := must(hostauth.NewCA("next", time.Hour, now))
	p.write(t, "ca.crt", append(append([]byte{}, p.ca.Cert...), next.Cert...))
	h := must(hostauth.IssueHost(next, "", "h1", time.Hour, now))
	if _, err := dial(t, addr, p, &h, "h1"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-decisions:
		if err != nil {
			t.Fatalf("cert from rotated CA rejected: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no auth decision; rotated CA bundle not picked up")
	}
}

func TestServerTLSNeedsFiles(t *testing.T) {
	if _, err := hostauth.ServerTLS(hostauth.ServerFiles{}); err == nil {
		t.Error("empty config accepted")
	}
	if _, err := hostauth.ServerTLS(hostauth.ServerFiles{Cert: "x", Key: "y", ClientCA: "z"}); err == nil {
		t.Error("missing files accepted")
	}
}
