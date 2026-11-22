package images

import (
	"crypto/ed25519"
	"reflect"
	"testing"
	"time"
)

type rolloutEnv struct {
	t    *testing.T
	r    *Rollout
	priv ed25519.PrivateKey
	now  time.Time
}

func newRollout(t *testing.T) *rolloutEnv {
	pub, priv := keypair(t)
	e := &rolloutEnv{t: t, priv: priv, now: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)}
	e.r = &Rollout{
		Policy:  Policy{CanaryHosts: []string{"canary"}, MinCanaryJobs: 5, MinSoak: time.Hour},
		Trusted: []ed25519.PublicKey{pub},
		Now:     func() time.Time { return e.now },
	}
	return e
}

func (e *rolloutEnv) publish(version string) {
	e.t.Helper()
	s, err := Sign(testManifest(e.t, "", version), e.priv)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.r.Publish(s); err != nil {
		e.t.Fatal(err)
	}
}

func (e *rolloutEnv) stage(version string) Stage {
	for _, s := range e.r.Status("ubuntu-24.04") {
		if s.Version == version {
			return s.Stage
		}
	}
	return ""
}

func (e *rolloutEnv) pin() string {
	e.t.Helper()
	p, err := e.r.Pin("ubuntu-24.04")
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

const (
	v1 = "ubuntu-24.04@2026-11-16"
	v2 = "ubuntu-24.04@2026-11-23"
)

// bootstrap makes 2026-11-16 live on hosts canary and h1.
func (e *rolloutEnv) bootstrap() {
	e.publish("2026-11-16")
	e.r.ObserveHost("canary", []string{v1}, false)
	e.r.ObserveHost("h1", nil, false)
	if tr := e.r.Advance(); len(tr) != 0 {
		e.t.Fatalf("went live before every host held it: %v", tr)
	}
	e.r.ObserveHost("h1", []string{v1}, false)
	if tr := e.r.Advance(); len(tr) != 1 || tr[0].To != Live {
		e.t.Fatalf("bootstrap: %v", tr)
	}
}

func TestRolloutCanaryThenFleet(t *testing.T) {
	e := newRollout(t)
	if _, err := e.r.Pin("ubuntu-24.04"); err == nil {
		t.Fatal("pinned an image with no live version")
	}
	e.bootstrap()
	if e.pin() != v1 {
		t.Fatal(e.pin())
	}
	if p, _ := e.r.Pin(v1); p != v1 {
		t.Fatal("pinned ref not passed through")
	}

	e.publish("2026-11-23")
	e.r.Advance()
	if e.stage("2026-11-23") != Staged {
		t.Fatal("canary started before canary host held the image")
	}
	// Staged versions are never handed to jobs.
	if got := e.r.AssignImage("j0", v1, "canary"); got != v1 {
		t.Fatalf("staged version assigned: %s", got)
	}
	e.r.ObserveResult("j0", false)

	e.r.ObserveHost("canary", []string{v1, v2}, false)
	if tr := e.r.Advance(); len(tr) != 1 || tr[0].To != Canary {
		t.Fatalf("%v", tr)
	}
	// Jobs on the canary host run the canary; others stay on live.
	if got := e.r.AssignImage("j1", v1, "canary"); got != v2 {
		t.Fatalf("canary host got %s", got)
	}
	if got := e.r.AssignImage("j2", v1, "h1"); got != v1 {
		t.Fatalf("fleet host got %s", got)
	}
	// An explicitly pinned older version is honored.
	if got := e.r.AssignImage("j3", "ubuntu-24.04@2026-11-09", "canary"); got != "ubuntu-24.04@2026-11-09" {
		t.Fatalf("explicit pin rewritten: %s", got)
	}
	e.r.ObserveResult("j1", false)
	e.r.ObserveResult("j2", true) // an error on the live version doesn't count against the canary
	e.r.ObserveResult("j3", false)

	for i := 0; i < 5; i++ {
		id := "c" + string(rune('a'+i))
		e.r.AssignImage(id, v1, "canary")
		e.r.ObserveResult(id, false)
	}
	e.r.Advance()
	if e.stage("2026-11-23") != Canary {
		t.Fatal("left canary before the soak time")
	}
	e.now = e.now.Add(2 * time.Hour)
	e.r.Advance()
	if e.stage("2026-11-23") != Canary {
		t.Fatal("went live before every host held it")
	}
	e.r.ObserveHost("h1", []string{v1, v2}, false)
	tr := e.r.Advance()
	if len(tr) != 2 || tr[0].To != Retired || tr[1].To != Live {
		t.Fatalf("%v", tr)
	}
	if e.pin() != v2 {
		t.Fatalf("live is %s", e.pin())
	}
	st := e.r.Status("ubuntu-24.04")
	if st[1].CanaryOK != 6 || st[1].CanaryErr != 0 {
		t.Fatalf("%+v", st)
	}
}

func TestRolloutCanaryRollsBackOnInfraErrors(t *testing.T) {
	e := newRollout(t)
	e.bootstrap()
	e.publish("2026-11-23")
	e.r.ObserveHost("canary", []string{v1, v2}, false)
	e.r.Advance()
	e.r.AssignImage("j1", v1, "canary")
	e.r.ObserveResult("j1", true)
	tr := e.r.Advance()
	if len(tr) != 1 || tr[0].To != RolledBack {
		t.Fatalf("%v", tr)
	}
	if got := e.r.AssignImage("j2", v1, "canary"); got != v1 {
		t.Fatalf("rolled back version still assigned: %s", got)
	}
	if e.pin() != v1 {
		t.Fatal("live changed")
	}
}

func TestRolloutDrainingHostsExemptAndSupersede(t *testing.T) {
	e := newRollout(t)
	e.bootstrap()
	e.publish("2026-11-23")
	e.publish("2026-11-30")
	if e.stage("2026-11-23") != RolledBack {
		t.Fatal("older staged version not superseded")
	}
	v3 := "ubuntu-24.04@2026-11-30"
	e.r.ObserveHost("canary", []string{v1, v3}, false)
	e.r.ObserveHost("h1", []string{v1, v3}, false)
	e.r.ObserveHost("h2", []string{v1}, true) // draining: not waited for
	e.r.Advance()
	for i := 0; i < 5; i++ {
		id := string(rune('a' + i))
		e.r.AssignImage(id, v1, "canary")
		e.r.ObserveResult(id, false)
	}
	e.now = e.now.Add(2 * time.Hour)
	e.r.Advance()
	if e.pin() != v3 {
		t.Fatalf("live %s", e.pin())
	}

	// Publishing something not newer than live is refused.
	s, _ := Sign(testManifest(t, "", "2026-11-23.1"), e.priv)
	if _, err := e.r.Publish(s); err == nil {
		t.Fatal("published an older version")
	}
}

func TestRolloutRequiresTrustedSignature(t *testing.T) {
	e := newRollout(t)
	_, other := keypair(t)
	s, _ := Sign(testManifest(t, "", "2026-11-16"), other)
	if _, err := e.r.Publish(s); err == nil {
		t.Fatal("published an image signed by an untrusted key")
	}
}

func TestRolloutRetainedAndRollback(t *testing.T) {
	e := newRollout(t)
	e.r.Policy.KeepRetired = 1
	e.r.Policy.MinCanaryJobs = 1
	e.r.Policy.MinSoak = time.Nanosecond
	e.bootstrap()
	versions := []string{"2026-11-23", "2026-11-30", "2026-12-07"}
	held := []string{v1}
	for i, ver := range versions {
		ref := "ubuntu-24.04@" + ver
		held = append(held, ref)
		e.publish(ver)
		e.r.ObserveHost("canary", held, false)
		e.r.ObserveHost("h1", held, false)
		e.r.Advance()
		if i == 0 {
			// A long job on the first version keeps it around after it retires.
			if got := e.r.AssignImage("long", "ubuntu-24.04@2026-11-16", "h1"); got != v1 {
				t.Fatal(got)
			}
		}
		e.r.AssignImage("c"+ver, "ubuntu-24.04@"+mustLive(t, e), "canary")
		e.r.ObserveResult("c"+ver, false)
		e.now = e.now.Add(time.Second)
		e.r.Advance()
	}
	if e.pin() != "ubuntu-24.04@2026-12-07" {
		t.Fatal(e.pin())
	}
	got := e.r.Retained("ubuntu-24.04")
	want := []string{"2026-11-16", "2026-11-30", "2026-12-07"} // in use, newest retired, live
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("retained %v want %v", got, want)
	}
	e.r.ObserveResult("long", false)
	if got := e.r.Retained("ubuntu-24.04"); !reflect.DeepEqual(got, want[1:]) {
		t.Fatalf("retained after job finished %v", got)
	}

	ref, err := e.r.Rollback("ubuntu-24.04", "broken toolchain")
	if err != nil || ref.Version != "2026-11-30" || e.pin() != "ubuntu-24.04@2026-11-30" {
		t.Fatalf("rollback %v %v %s", ref, err, e.pin())
	}
	if e.stage("2026-12-07") != RolledBack {
		t.Fatal("rolled back version not marked")
	}
}

func mustLive(t *testing.T, e *rolloutEnv) string {
	r, err := ParseRef(e.pin())
	if err != nil {
		t.Fatal(err)
	}
	return r.Version
}
