package scheduler

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

func sub(id string, vcpu int) jobapi.Submission {
	return jobapi.Submission{
		Spec: jobapi.Spec{
			JobID:    id,
			Tenant:   "t1",
			Repo:     "github.com/acme/api",
			Hardware: jobapi.NewHardware(jobapi.ArchX86_64, vcpu),
			Prebuild: jobapi.PrebuildRef{Repo: "acme/api", Policy: jobapi.PrebuildLatestTrusted},
		},
		Profile: "acme/api/CI/test",
	}
}

func host(id, region string, vcpu, mem int) Host {
	return Host{ID: id, Region: region, FailureDomain: id, Arch: jobapi.ArchX86_64, VCPU: vcpu, MemGB: mem}
}

func TestProfilePercentilesAndWindow(t *testing.T) {
	p := &Profiles{Window: 100, MinSamples: 5}
	for i := 1; i <= 4; i++ {
		p.Observe("k", Usage{CPU: float64(i), MemGB: float64(i)})
	}
	if _, ok := p.Get("k"); ok {
		t.Fatal("profile trusted before MinSamples")
	}
	for i := 5; i <= 100; i++ {
		p.Observe("k", Usage{CPU: float64(i), MemGB: float64(i)})
	}
	prof, ok := p.Get("k")
	if !ok || prof.Samples != 100 || prof.P95CPU != 95 || prof.P99MemGB != 99 || prof.PeakMemGB != 100 {
		t.Fatalf("profile = %+v ok=%v", prof, ok)
	}
	// Window drops the oldest samples.
	for i := 0; i < 100; i++ {
		p.Observe("k", Usage{CPU: 1, MemGB: 1})
	}
	prof, _ = p.Get("k")
	if prof.P95CPU != 1 || prof.PeakMemGB != 1 {
		t.Fatalf("window not applied: %+v", prof)
	}
}

func TestPlacePrefersPrebuildHostThenRegion(t *testing.T) {
	s := New(nil)
	a := host("a", "eu", 32, 128)
	b := host("b", "us", 32, 128)
	c := host("c", "us", 32, 128)
	c.Snapshots = map[string]bool{"acme/api": true}
	for _, h := range []Host{a, b, c} {
		s.UpsertHost(h)
	}
	got, err := s.Place(sub("j1", 4))
	if err != nil || got.HostID != "c" || got.Locality != LocalityHost {
		t.Fatalf("got %+v err %v", got, err)
	}

	// Without c available, b (same region as a prebuild) beats a.
	s2 := New(nil)
	c.Draining = true
	for _, h := range []Host{a, b, c} {
		s2.UpsertHost(h)
	}
	got, err = s2.Place(sub("j1", 4))
	if err != nil || got.HostID != "b" || got.Locality != LocalityRegion {
		t.Fatalf("got %+v err %v", got, err)
	}

	// tachyon-clean ignores locality.
	cl := sub("j2", 4)
	cl.Spec.Prebuild.Policy = jobapi.PrebuildNone
	got, err = s2.Place(cl)
	if err != nil || got.Locality != LocalityNone {
		t.Fatalf("clean job got %+v err %v", got, err)
	}
}

func TestPlaceHardConstraints(t *testing.T) {
	arm := host("arm", "us", 32, 128)
	arm.Arch = jobapi.ArchARM64
	eu := host("eu", "eu", 32, 128)
	ded := host("ded", "us", 32, 128)
	ded.DedicatedTenant = "bigco"
	small := host("small", "us", 4, 16)
	drain := host("drain", "us", 32, 128)
	drain.Draining = true

	s := New(nil)
	for _, h := range []Host{arm, eu, ded, small, drain} {
		s.UpsertHost(h)
	}
	j := sub("j1", 8)
	j.Region = "us"
	if _, err := s.Place(j); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("want ErrNoCapacity, got %v", err)
	}
	j.Spec.Tenant = "bigco"
	got, err := s.Place(j)
	if err != nil || got.HostID != "ded" {
		t.Fatalf("dedicated tenant: got %+v err %v", got, err)
	}
	if _, err := s.Place(j); err == nil {
		t.Fatal("duplicate placement accepted")
	}
}

func TestPlaceCPUOversubscriptionAndMemory(t *testing.T) {
	s := New(nil)
	s.UpsertHost(host("h", "us", 16, 1000)) // memory plentiful
	// 16 vCPU * 1.25 = 20: five 4-vCPU jobs fit, the sixth does not.
	for i := 0; i < 5; i++ {
		if _, err := s.Place(sub(fmt.Sprint("c", i), 4)); err != nil {
			t.Fatalf("job %d: %v", i, err)
		}
	}
	if _, err := s.Place(sub("c5", 4)); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("want ErrNoCapacity at 1.25x, got %v", err)
	}

	m := New(nil)
	m.UpsertHost(host("h", "us", 1000, 64)) // CPU plentiful
	// 64 GB: four 4-vCPU (16 GB) jobs, never a fifth.
	for i := 0; i < 4; i++ {
		if _, err := m.Place(sub(fmt.Sprint("m", i), 4)); err != nil {
			t.Fatalf("job %d: %v", i, err)
		}
	}
	if _, err := m.Place(sub("m4", 4)); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("memory oversubscribed: %v", err)
	}
}

func TestPlaceUsesProfileDemand(t *testing.T) {
	p := &Profiles{MinSamples: 3}
	for i := 0; i < 3; i++ {
		p.Observe("acme/api/CI/test", Usage{CPU: 1, MemGB: 3})
	}
	s := New(p)
	s.UpsertHost(host("h", "us", 16, 64))
	a, err := s.Place(sub("j", 8))
	if err != nil {
		t.Fatal(err)
	}
	if a.CPU != 1 || a.MemGB != 3+MemMarginGB {
		t.Fatalf("reservation = %+v, want profile-based", a)
	}
	// Profile never exceeds the VM size.
	p.Observe("big", Usage{CPU: 50, MemGB: 500})
	p.Observe("big", Usage{CPU: 50, MemGB: 500})
	p.Observe("big", Usage{CPU: 50, MemGB: 500})
	b := sub("big1", 2)
	b.Profile = "big"
	got, err := s.Place(b)
	if err != nil || got.CPU != 2 || got.MemGB != 8 {
		t.Fatalf("capped reservation = %+v err %v", got, err)
	}
}

func TestReleaseFreesCapacityAndFeedsProfile(t *testing.T) {
	p := &Profiles{MinSamples: 1}
	s := New(p)
	s.UpsertHost(host("h", "us", 4, 16))
	if _, err := s.Place(sub("j1", 4)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Place(sub("j2", 4)); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("want full, got %v", err)
	}
	s.Release("j1", &Usage{CPU: 2, MemGB: 5})
	if _, err := s.Place(sub("j2", 4)); err != nil {
		t.Fatalf("after release: %v", err)
	}
	if prof, ok := p.Get("acme/api/CI/test"); !ok || prof.P95CPU != 2 {
		t.Fatalf("profile not fed: %+v", prof)
	}
	s.Release("unknown", nil) // no-op
}

func TestDeferrable(t *testing.T) {
	s := New(nil)
	s.UpsertHost(host("h", "us", 16, 1000))
	d := sub("d1", 4)
	d.Deferrable = true
	if _, err := s.Place(d); err != nil {
		t.Fatalf("quiet fleet should admit deferrable: %v", err)
	}
	if _, err := s.Place(sub("busy", 8)); err != nil {
		t.Fatal(err)
	}
	// 12/16 = 75% > 50% default threshold.
	d2 := sub("d2", 2)
	d2.Deferrable = true
	if _, err := s.Place(d2); !errors.Is(err, ErrDeferred) {
		t.Fatalf("want ErrDeferred, got %v (util %.2f)", err, s.Utilization())
	}
	if New(nil).Utilization() != 1 {
		t.Error("empty fleet should report full utilization")
	}
}

func TestReportSlowdown(t *testing.T) {
	s := New(nil)
	s.UpsertHost(host("h", "us", 16, 64))
	if s.Ratio("h") != StartRatio {
		t.Fatalf("start ratio = %v", s.Ratio("h"))
	}
	s.ReportSlowdown("h", 1.2, false)
	if r := s.Ratio("h"); math.Abs(r-(StartRatio-2*RatioStep)) > 1e-9 {
		t.Fatalf("step back ratio = %v", r)
	}
	for i := 0; i < 20; i++ {
		s.ReportSlowdown("h", 1.5, false)
	}
	if s.Ratio("h") != MinRatio {
		t.Fatalf("ratio below min: %v", s.Ratio("h"))
	}
	s.ReportSlowdown("h", 1.0, false) // no sustained evidence: no rise
	if s.Ratio("h") != MinRatio {
		t.Fatal("rose without sustained evidence")
	}
	for i := 0; i < 100; i++ {
		s.ReportSlowdown("h", 1.0, true)
	}
	if s.Ratio("h") != MaxRatio {
		t.Fatalf("ratio above max: %v", s.Ratio("h"))
	}
	s.ReportSlowdown("missing", 2, false) // no panic
	if s.Ratio("missing") != 0 {
		t.Error("unknown host ratio")
	}
	// Heartbeats must not reset the ratio.
	s.UpsertHost(host("h", "us", 16, 64))
	if s.Ratio("h") != MaxRatio {
		t.Error("UpsertHost reset ratio")
	}
}

func TestRemoveHost(t *testing.T) {
	s := New(nil)
	s.UpsertHost(host("h", "us", 4, 16))
	if _, err := s.Place(sub("j", 4)); err != nil {
		t.Fatal(err)
	}
	s.RemoveHost("h")
	s.UpsertHost(host("h", "us", 4, 16))
	if _, err := s.Place(sub("j", 4)); err != nil {
		t.Fatalf("job id should be free after host removal: %v", err)
	}
}
