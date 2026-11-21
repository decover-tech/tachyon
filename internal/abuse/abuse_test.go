package abuse

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func sub(id, tenant string, vcpu int) jobapi.Submission {
	return jobapi.Submission{Spec: jobapi.Spec{
		JobID: id, Tenant: tenant, Repo: "github.com/acme/api", Commit: "abc",
		Hardware: jobapi.NewHardware(jobapi.ArchX86_64, vcpu), Image: "ubuntu-24.04@v1",
		Trust: jobapi.Trusted,
	}}
}

func verified() Verification {
	return Verification{EmailVerified: true, PaymentMethod: true}
}

func newGate(accts ...Account) (*Gate, *MemAccounts, *time.Time) {
	m := &MemAccounts{}
	for _, a := range accts {
		m.Put(a)
	}
	now := t0
	return &Gate{Accounts: m, Now: func() time.Time { return now }}, m, &now
}

func denied(t *testing.T, err error, want string) {
	t.Helper()
	if !IsDenied(err) {
		t.Fatalf("err = %v, want denial", err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("err = %q, want it to mention %q", err, want)
	}
}

func TestGateVerification(t *testing.T) {
	ctx := context.Background()
	g, _, _ := newGate(
		Account{Tenant: "noemail", Plan: PlanFree, Verification: Verification{PaymentMethod: true}},
		Account{Tenant: "young", Plan: PlanFree, Verification: Verification{EmailVerified: true, GitHubAccountCreated: t0.Add(-24 * time.Hour)}},
		Account{Tenant: "old", Plan: PlanFree, Verification: Verification{EmailVerified: true, GitHubAccountCreated: t0.Add(-365 * 24 * time.Hour)}},
		Account{Tenant: "card", Plan: PlanTrial, TrialEndsAt: t0.Add(time.Hour), Verification: verified()},
		Account{Tenant: "paid", Plan: PlanPaid},
	)
	denied(t, g.Admit(ctx, sub("j1", "noemail", 2)), "email not verified")
	denied(t, g.Admit(ctx, sub("j2", "young", 2)), "payment method")
	for i, tenant := range []string{"old", "card", "paid"} {
		if err := g.Admit(ctx, sub("ok"+string(rune('a'+i)), tenant, 2)); err != nil {
			t.Errorf("%s: %v", tenant, err)
		}
	}
	denied(t, g.Admit(ctx, sub("j3", "stranger", 2)), "no account")
}

func TestGateUnknownPlan(t *testing.T) {
	g, _, _ := newGate()
	g.UnknownPlan = PlanPaid
	if err := g.Admit(context.Background(), sub("j1", "stranger", 16)); err != nil {
		t.Fatal(err)
	}
}

func TestGateTrialExpiry(t *testing.T) {
	g, _, now := newGate(Account{Tenant: "t", Plan: PlanTrial, TrialEndsAt: t0.Add(time.Hour), Verification: verified()})
	if err := g.Admit(context.Background(), sub("j1", "t", 2)); err != nil {
		t.Fatal(err)
	}
	*now = t0.Add(time.Hour)
	denied(t, g.Admit(context.Background(), sub("j2", "t", 2)), "trial ended")
}

func TestGateSizeAndConcurrency(t *testing.T) {
	ctx := context.Background()
	g, _, _ := newGate(Account{Tenant: "f", Plan: PlanFree, Verification: verified()})
	denied(t, g.Admit(ctx, sub("big", "f", 8)), "up to 4 vCPU")
	for _, id := range []string{"j1", "j2"} {
		if err := g.Admit(ctx, sub(id, "f", 4)); err != nil {
			t.Fatal(err)
		}
	}
	// A deferred job retried is not counted twice.
	if err := g.Admit(ctx, sub("j1", "f", 4)); err != nil {
		t.Fatalf("re-admit: %v", err)
	}
	denied(t, g.Admit(ctx, sub("j3", "f", 2)), "2 concurrent jobs")
	g.Release("j1")
	if err := g.Admit(ctx, sub("j3", "f", 2)); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestGateMonthlyMinutes(t *testing.T) {
	ctx := context.Background()
	g, _, now := newGate(Account{Tenant: "f", Plan: PlanFree, Verification: verified()})
	if err := g.Admit(ctx, sub("j1", "f", 2)); err != nil {
		t.Fatal(err)
	}
	g.Finish("j1", 499*time.Minute)
	g.Finish("j1", 499*time.Minute) // idempotent
	if got := g.MinutesUsed("f"); got != 499 {
		t.Fatalf("minutes = %v", got)
	}
	if err := g.Admit(ctx, sub("j2", "f", 2)); err != nil {
		t.Fatal(err)
	}
	g.Finish("j2", 2*time.Minute)
	denied(t, g.Admit(ctx, sub("j3", "f", 2)), "500 minutes")

	*now = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	if g.MinutesUsed("f") != 0 {
		t.Error("minutes should reset with the month")
	}
	if err := g.Admit(ctx, sub("j3", "f", 2)); err != nil {
		t.Fatalf("new month: %v", err)
	}
}

func TestGateSuspended(t *testing.T) {
	ctx := context.Background()
	g, _, _ := newGate(Account{Tenant: "p", Plan: PlanPaid})
	if err := g.Suspend(ctx, "p", "mining"); err != nil {
		t.Fatal(err)
	}
	denied(t, g.Admit(ctx, sub("j1", "p", 2)), "suspended: mining")
}

type recAlerter struct{ got []Alert }

func (r *recAlerter) Alert(_ context.Context, a Alert) { r.got = append(r.got, a) }

type recSuspender struct{ tenants []string }

func (r *recSuspender) Suspend(_ context.Context, tenant, _ string) error {
	r.tenants = append(r.tenants, tenant)
	return nil
}

func newMonitor() (*Monitor, *recAlerter, *recSuspender) {
	a, s := &recAlerter{}, &recSuspender{}
	return &Monitor{Alerter: a, Suspender: s}, a, s
}

// feed sends n samples of interval d starting at t0, each built by f.
func feed(m *Monitor, n int, d time.Duration, f func(*Sample)) []Alert {
	var out []Alert
	for i := 1; i <= n; i++ {
		s := Sample{JobID: "j1", At: t0.Add(time.Duration(i) * d), Interval: d}
		f(&s)
		out = append(out, m.Observe(context.Background(), s)...)
	}
	return out
}

func TestCPUSustainedPaidAlertsOnly(t *testing.T) {
	m, al, su := newMonitor()
	m.Track(Job{ID: "j1", Tenant: "p", VCPU: 4, Plan: PlanPaid})
	// 29 minutes saturated: nothing yet.
	if got := feed(m, 29, time.Minute, func(s *Sample) { s.BusyVCPU = 3.9 }); len(got) != 0 {
		t.Fatalf("early alerts: %+v", got)
	}
	got := m.Observe(context.Background(), Sample{JobID: "j1", At: t0.Add(30 * time.Minute), Interval: time.Minute, BusyVCPU: 3.8})
	if len(got) != 1 || got[0].Kind != CPUSustained || got[0].Severity != Warning {
		t.Fatalf("alerts = %+v", got)
	}
	if got[0].Action.CancelJob || got[0].Action.SuspendTenant {
		t.Errorf("paid warning should only alert: %+v", got[0].Action)
	}
	// Fires once per job.
	if again := m.Observe(context.Background(), Sample{JobID: "j1", At: t0.Add(31 * time.Minute), Interval: time.Minute, BusyVCPU: 4}); len(again) != 0 {
		t.Errorf("repeat alert: %+v", again)
	}
	if len(al.got) != 1 || len(su.tenants) != 0 {
		t.Errorf("delivered %d alerts, suspended %v", len(al.got), su.tenants)
	}
}

func TestCPUStreakResets(t *testing.T) {
	m, _, _ := newMonitor()
	m.Track(Job{ID: "j1", Tenant: "f", VCPU: 2, Plan: PlanTrial})
	i := 0
	got := feed(m, 30, time.Minute, func(s *Sample) {
		i++
		s.BusyVCPU = 2
		if i%9 == 0 { // a dip every 9 minutes: the job is compiling, then testing
			s.BusyVCPU = 0.5
		}
	})
	if len(got) != 0 {
		t.Fatalf("alerts = %+v", got)
	}
}

func TestCPUSustainedTrialCancels(t *testing.T) {
	m, _, su := newMonitor()
	m.Track(Job{ID: "j1", Tenant: "t", VCPU: 2, Plan: PlanTrial})
	got := feed(m, 10, time.Minute, func(s *Sample) { s.BusyVCPU = 1.95 })
	if len(got) != 1 || got[0].Kind != CPUSustained {
		t.Fatalf("alerts = %+v", got)
	}
	if !got[0].Action.CancelJob || got[0].Action.SuspendTenant {
		t.Errorf("trial warning should cancel the job only: %+v", got[0].Action)
	}
	if len(su.tenants) != 0 {
		t.Errorf("suspended %v", su.tenants)
	}
}

func TestPacketFloodSuspendsTrial(t *testing.T) {
	m, _, su := newMonitor()
	m.Track(Job{ID: "j1", Tenant: "t", VCPU: 2, Plan: PlanTrial})
	got := feed(m, 1, 10*time.Second, func(s *Sample) {
		s.TxPackets = 500_000 // 50k pps
		s.TxBytes = 500_000 * 64
	})
	if len(got) != 1 || got[0].Kind != PacketFlood || got[0].Severity != Critical {
		t.Fatalf("alerts = %+v", got)
	}
	if !got[0].Action.CancelJob || !got[0].Action.SuspendTenant {
		t.Errorf("action = %+v", got[0].Action)
	}
	if len(su.tenants) != 1 || su.tenants[0] != "t" {
		t.Errorf("suspended %v", su.tenants)
	}
}

func TestLargePacketsAreNotAFlood(t *testing.T) {
	m, _, _ := newMonitor()
	m.Track(Job{ID: "j1", Tenant: "p", VCPU: 8, Plan: PlanPaid})
	// 60k pps of full-size frames (a docker push) for one sample.
	got := feed(m, 1, 10*time.Second, func(s *Sample) {
		s.TxPackets = 600_000
		s.TxBytes = 600_000 * 1500
	})
	if len(got) != 0 {
		t.Fatalf("alerts = %+v", got)
	}
}

func TestEgressSustained(t *testing.T) {
	m, _, _ := newMonitor()
	m.Track(Job{ID: "j1", Tenant: "p", VCPU: 8, Plan: PlanPaid})
	got := feed(m, 10, 30*time.Second, func(s *Sample) {
		s.TxBytes = 60 << 20 * 30 // 60 MiB/s
		s.TxPackets = s.TxBytes / 1500
	})
	if len(got) != 1 || got[0].Kind != EgressSustained || got[0].Job.Tenant != "p" {
		t.Fatalf("alerts = %+v", got)
	}
	if got[0].Action.CancelJob {
		t.Errorf("paid egress warning should not cancel")
	}
}

func TestScan(t *testing.T) {
	for name, f := range map[string]func(*Sample){
		"connections":  func(s *Sample) { s.NewConnections = 3000 }, // 300/s
		"destinations": func(s *Sample) { s.DistinctDestinations = 1500 },
	} {
		t.Run(name, func(t *testing.T) {
			m, _, su := newMonitor()
			m.Track(Job{ID: "j1", Tenant: "p", VCPU: 2, Plan: PlanPaid})
			got := feed(m, 1, 10*time.Second, f)
			if len(got) != 1 || got[0].Kind != Scan || !got[0].Action.CancelJob || got[0].Action.SuspendTenant {
				t.Fatalf("alerts = %+v", got)
			}
			if len(su.tenants) != 0 {
				t.Errorf("paid tenant suspended automatically")
			}
		})
	}
}

func TestNormalBuildIsQuiet(t *testing.T) {
	m, _, _ := newMonitor()
	m.Track(Job{ID: "j1", Tenant: "t", VCPU: 4, Plan: PlanTrial})
	i := 0
	got := feed(m, 60, 15*time.Second, func(s *Sample) {
		i++
		s.BusyVCPU = 3.9 // 8 minutes compiling, then tests
		if i > 32 {
			s.BusyVCPU = 1.2
		}
		s.TxBytes, s.TxPackets = 2<<20, 3000
		s.RxBytes = 200 << 20
		s.NewConnections, s.DistinctDestinations = 20, 6
	})
	if len(got) != 0 {
		t.Fatalf("alerts = %+v", got)
	}
}

func TestFinishCatchesWholeRunSaturation(t *testing.T) {
	ctx := context.Background()
	m, al, _ := newMonitor()
	m.Track(Job{ID: "j1", Tenant: "t", VCPU: 8, Plan: PlanFree})
	got := m.Finish(ctx, "j1", 7.6, 45*time.Minute)
	if len(got) != 1 || got[0].Kind != CPUSustained || len(al.got) != 1 {
		t.Fatalf("alerts = %+v", got)
	}
	if m.Tracked("j1") {
		t.Error("still tracked after Finish")
	}
	// Short busy jobs, and untracked jobs, are fine.
	m.Track(Job{ID: "j2", Tenant: "t", VCPU: 8, Plan: PlanFree})
	if got := m.Finish(ctx, "j2", 8, 5*time.Minute); len(got) != 0 {
		t.Errorf("short job alerts = %+v", got)
	}
	if got := m.Finish(ctx, "nope", 8, time.Hour); len(got) != 0 {
		t.Errorf("untracked alerts = %+v", got)
	}
}

func TestUntrackedSamplesIgnored(t *testing.T) {
	m, _, _ := newMonitor()
	if got := feed(m, 1, time.Second, func(s *Sample) { s.NewConnections = 1e6 }); len(got) != 0 {
		t.Fatalf("alerts = %+v", got)
	}
}

func TestGateSuspendsViaMonitor(t *testing.T) {
	ctx := context.Background()
	g, _, _ := newGate(Account{Tenant: "t", Plan: PlanTrial, TrialEndsAt: t0.Add(24 * time.Hour), Verification: verified()})
	m := &Monitor{Suspender: g}
	if err := g.Admit(ctx, sub("j1", "t", 2)); err != nil {
		t.Fatal(err)
	}
	m.Track(Job{ID: "j1", Tenant: "t", VCPU: 2, Plan: g.PlanOf(ctx, "t")})
	m.Observe(ctx, Sample{JobID: "j1", At: t0.Add(10 * time.Second), Interval: 10 * time.Second, NewConnections: 10_000})
	denied(t, g.Admit(ctx, sub("j2", "t", 2)), "suspended: scan")
}
