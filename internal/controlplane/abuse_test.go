package controlplane

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/abuse"
)

type countAlerter struct{ n int }

func (c *countAlerter) Alert(context.Context, abuse.Alert) { c.n++ }

func (e *env) withAbuse(accts ...abuse.Account) (*abuse.Gate, *countAlerter) {
	m := &abuse.MemAccounts{}
	for _, a := range accts {
		m.Put(a)
	}
	g := &abuse.Gate{Accounts: m}
	al := &countAlerter{}
	e.core.Limits = g
	e.core.Anomalies = &abuse.Monitor{Alerter: al, Suspender: g}
	return g, al
}

func trialAccount() abuse.Account {
	return abuse.Account{
		Tenant: "gh_inst_1", Plan: abuse.PlanTrial, TrialEndsAt: time.Now().Add(24 * time.Hour),
		Verification: abuse.Verification{EmailVerified: true, PaymentMethod: true},
	}
}

func TestSubmitRefusedByLimits(t *testing.T) {
	e := newEnv()
	unverified := trialAccount()
	unverified.Verification = abuse.Verification{}
	e.withAbuse(unverified)
	s := e.connect(t, "h1", "us", 16, 64)

	err := e.core.Submit(context.Background(), e.submission(t, "j1", 2))
	if !abuse.IsDenied(err) {
		t.Fatalf("err = %v, want denial", err)
	}
	s.none(t)
	if e.core.Record("j1") != nil {
		t.Error("refused job should have no record")
	}
}

func TestSubmitReleasesSlotWhenPlacementFails(t *testing.T) {
	e := newEnv()
	free := trialAccount()
	free.Plan = abuse.PlanFree // 2 concurrent jobs
	g, _ := e.withAbuse(free)
	// No hosts: every submit fails with no capacity and no burst.
	for _, id := range []string{"j1", "j2", "j3"} {
		if err := e.core.Submit(context.Background(), e.submission(t, id, 2)); err == nil || abuse.IsDenied(err) {
			t.Fatalf("%s: err = %v, want no capacity", id, err)
		}
	}
	if err := g.Admit(context.Background(), e.submission(t, "probe", 2)); err != nil {
		t.Fatalf("slots leaked: %v", err)
	}
}

func TestUsageAnomalyCancelsTrialJobAndSuspends(t *testing.T) {
	e := newEnv()
	g, al := e.withAbuse(trialAccount())
	s := e.connect(t, "h1", "us", 16, 64)
	if err := e.core.Submit(context.Background(), e.submission(t, "j1", 2)); err != nil {
		t.Fatal(err)
	}
	if s.next(t).GetAssign() == nil {
		t.Fatal("expected AssignJob")
	}

	// A normal sample: nothing happens.
	usage := func(conns uint32) *hostlinkv1.LinkRequest {
		return &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobUsage{JobUsage: &hostlinkv1.JobUsage{
			JobId: "j1", At: timestamppb.Now(), IntervalMs: 10_000, BusyVcpu: 1, NewConnections: conns,
		}}}
	}
	s.push(t, usage(5))
	s.none(t)

	// A scan: the job is cancelled and the tenant suspended.
	s.push(t, usage(10_000))
	if c := s.next(t).GetCancel(); c == nil || c.GetJobId() != "j1" {
		t.Fatalf("expected CancelJob for j1")
	}
	if al.n != 1 {
		t.Errorf("alerts = %d", al.n)
	}
	if err := g.Admit(context.Background(), e.submission(t, "j2", 2)); !abuse.IsDenied(err) {
		t.Errorf("tenant not suspended: %v", err)
	}
}

func TestJobResultChargesMinutesAndChecksCPU(t *testing.T) {
	e := newEnv()
	g, al := e.withAbuse(trialAccount())
	s := e.connect(t, "h1", "us", 16, 64)
	if err := e.core.Submit(context.Background(), e.submission(t, "j1", 2)); err != nil {
		t.Fatal(err)
	}
	s.next(t)
	start := time.Now().Add(-40 * time.Minute)
	for _, ev := range []struct {
		p  hostlinkv1.Phase
		at time.Time
	}{
		{hostlinkv1.Phase_PHASE_VM_RESUMED, start},
		{hostlinkv1.Phase_PHASE_JOB_FINISHED, start.Add(30 * time.Minute)},
	} {
		s.push(t, &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobEvent{JobEvent: &hostlinkv1.JobEvent{
			JobId: "j1", Phase: ev.p, At: timestamppb.New(ev.at),
		}}})
	}
	s.push(t, &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobResult{JobResult: &hostlinkv1.JobResult{
		JobId: "j1", Outcome: hostlinkv1.JobResult_OUTCOME_SUCCEEDED, AvgBusyVcpu: 1.98,
	}}})
	if got := g.MinutesUsed("gh_inst_1"); got != 30 {
		t.Errorf("minutes used = %v, want 30", got)
	}
	if al.n != 1 {
		t.Errorf("whole-run CPU alert count = %d, want 1", al.n)
	}
	if e.core.Anomalies.Tracked("j1") {
		t.Error("job still tracked")
	}
}

func TestUsageIgnoredWithoutMonitor(t *testing.T) {
	e := newEnv()
	s := e.connect(t, "h1", "us", 16, 64)
	s.push(t, &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobUsage{JobUsage: &hostlinkv1.JobUsage{
		JobId: "j1", IntervalMs: 1000, NewConnections: 1 << 20,
	}}})
	s.none(t)
}
