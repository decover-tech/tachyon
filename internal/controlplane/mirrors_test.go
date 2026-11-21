package controlplane

import (
	"context"
	"testing"

	"github.com/decover-tech/tachyon/internal/mirrors"
)

type fixedPair struct{ base string }

func (f *fixedPair) Endpoints() mirrors.Endpoints { return mirrors.EndpointsFor(f.base) }

func TestGuestUsesHealthyMirrorThenUpstream(t *testing.T) {
	e := newEnv()
	pair := &fixedPair{base: "https://mirror-b.r1"}
	e.core.Mirrors = mirrors.Endpoints{NPM: "https://static.example/"} // overridden by the pair
	e.core.MirrorPair = pair
	s := e.connect(t, "h1", "us", 16, 64)
	s.push(t, heartbeat(false))

	if err := e.core.Submit(context.Background(), e.submission(t, "j1", 2)); err != nil {
		t.Fatal(err)
	}
	g := s.next(t).GetAssign().GetGuest()
	if got := g.GetEnv()["npm_config_registry"]; got != "https://mirror-b.r1/npm/" {
		t.Errorf("npm registry = %q", got)
	}
	for _, f := range []string{
		"/etc/docker/daemon.json",
		"/etc/containerd/certs.d/ghcr.io/hosts.toml",
		"/etc/apt/apt.conf.d/01tachyon-proxy",
		"/etc/gitconfig.d/tachyon",
	} {
		if _, ok := g.GetFiles()[f]; !ok {
			t.Errorf("missing guest file %s", f)
		}
	}

	// Both mirrors down: the next guest gets no mirror config at all.
	pair.base = ""
	if err := e.core.Submit(context.Background(), e.submission(t, "j2", 2)); err != nil {
		t.Fatal(err)
	}
	g = s.next(t).GetAssign().GetGuest()
	for _, k := range []string{"npm_config_registry", "PIP_INDEX_URL", "GOPROXY"} {
		if v, ok := g.GetEnv()[k]; ok {
			t.Errorf("both mirrors down but %s=%q", k, v)
		}
	}
	if len(g.GetFiles()) != 0 {
		t.Errorf("both mirrors down but files %v", g.GetFiles())
	}
}
