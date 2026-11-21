package mirrors

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

type fakeProber struct {
	mu   sync.Mutex
	down map[string]bool
}

func (f *fakeProber) set(base string, down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down[base] = down
}

func (f *fakeProber) Probe(_ context.Context, base string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down[base] {
		return errors.New("down")
	}
	return nil
}

func TestMonitorFailover(t *testing.T) {
	const a, b = "https://mirror-a", "https://mirror-b"
	p := &fakeProber{down: map[string]bool{}}
	m := &Monitor{Primary: a, Secondary: b, Prober: p, Fall: 2, Rise: 2}
	ctx := context.Background()

	if got := m.Endpoints(); got != (Endpoints{}) {
		t.Errorf("before any probe = %+v, want upstream", got)
	}
	m.Check(ctx)
	if got := m.Endpoints(); got != EndpointsFor(a) {
		t.Errorf("both up = %+v, want primary", got)
	}

	// One failed probe is not enough to fail over.
	p.set(a, true)
	m.Check(ctx)
	if got := m.Pair().Pick(); got != a {
		t.Errorf("after one failure serving %q, want primary", got)
	}
	m.Check(ctx)
	if got := m.Pair().Pick(); got != b {
		t.Errorf("after Fall failures serving %q, want secondary", got)
	}

	// Both down: guests go to upstream.
	p.set(b, true)
	m.Check(ctx)
	m.Check(ctx)
	if got := m.Endpoints(); got != (Endpoints{}) {
		t.Errorf("both down = %+v, want upstream", got)
	}
	if env := m.Endpoints().Env(); len(env) != 0 {
		t.Errorf("both down env = %v", env)
	}

	// The primary must pass Rise probes before it serves again.
	p.set(a, false)
	m.Check(ctx)
	if got := m.Pair().Pick(); got != "" {
		t.Errorf("after one success serving %q, want upstream", got)
	}
	m.Check(ctx)
	if got := m.Pair().Pick(); got != a {
		t.Errorf("after Rise successes serving %q, want primary", got)
	}
}

func TestMonitorFirstProbeDecides(t *testing.T) {
	p := &fakeProber{down: map[string]bool{"https://a": true}}
	m := &Monitor{Primary: "https://a", Secondary: "https://b", Prober: p}
	m.Check(context.Background())
	if got := m.Pair(); got.PrimaryUp || !got.SecondaryUp {
		t.Errorf("first probe: %+v", got)
	}
}

func TestMonitorSingleMember(t *testing.T) {
	m := &Monitor{Primary: "https://a", Prober: &fakeProber{down: map[string]bool{}}}
	m.Check(context.Background())
	if got := m.Pair(); !got.PrimaryUp || got.SecondaryUp {
		t.Errorf("pair = %+v", got)
	}
}

func TestHTTPProber(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		if !healthy.Load() {
			http.Error(w, "disk full", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	p := HTTPProber{}
	if err := p.Probe(context.Background(), srv.URL+"/"); err != nil {
		t.Errorf("healthy probe: %v", err)
	}
	healthy.Store(false)
	if err := p.Probe(context.Background(), srv.URL); err == nil {
		t.Error("unhealthy probe succeeded")
	}
}

func TestEndpointsFor(t *testing.T) {
	e := EndpointsFor("https://m.r1/")
	want := Endpoints{
		Registry: "https://m.r1", NPM: "https://m.r1/npm/", PyPI: "https://m.r1/pypi/simple",
		GoProxy: "https://m.r1/go", Maven: "https://m.r1/maven", Apt: "https://m.r1", Git: "https://m.r1/git/",
	}
	if e != want {
		t.Errorf("EndpointsFor = %+v", e)
	}
	if EndpointsFor("") != (Endpoints{}) {
		t.Error("empty base should mean upstream")
	}
}

func TestContainerdHosts(t *testing.T) {
	got := Endpoints{Registry: "https://m.r1"}.ContainerdHosts()
	if len(got) != 3 {
		t.Fatalf("hosts = %v", got)
	}
	want := "server = \"https://ghcr.io\"\n\n[host.\"https://m.r1\"]\n  capabilities = [\"pull\", \"resolve\"]\n"
	if got["/etc/containerd/certs.d/ghcr.io/hosts.toml"] != want {
		t.Errorf("ghcr hosts.toml = %q", got["/etc/containerd/certs.d/ghcr.io/hosts.toml"])
	}
	if (Endpoints{}).ContainerdHosts() != nil {
		t.Error("no registry mirror should write no hosts.toml")
	}
}
