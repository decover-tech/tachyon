// Package abuse keeps runners from being used for mining or attacks: it
// gates free and trial accounts with limits and verification before a job
// is placed, and watches running jobs for CPU and network anomaly patterns
// (see "Security and isolation model" in docs/architecture.md).
package abuse

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

// Plan is a tenant's billing plan.
type Plan string

const (
	PlanFree       Plan = "free"
	PlanTrial      Plan = "trial"
	PlanPaid       Plan = "paid"
	PlanEnterprise Plan = "enterprise"
)

// Restricted reports whether the plan gets free/trial limits and the
// stricter anomaly response.
func (p Plan) Restricted() bool { return p == PlanFree || p == PlanTrial }

// Verification is what Tachyon knows about who is behind a tenant.
type Verification struct {
	EmailVerified bool
	// PaymentMethod is a card or invoice on file (not charged on trial).
	PaymentMethod bool
	// GitHubAccountCreated is when the installing GitHub account or
	// organization was created; zero if unknown.
	GitHubAccountCreated time.Time
}

// Account is a tenant's plan and standing.
type Account struct {
	Tenant       string
	Plan         Plan
	TrialEndsAt  time.Time // PlanTrial only
	Verification Verification
	Suspended    bool
	SuspendedFor string
}

// Limits apply per plan. Zero means unlimited.
type Limits struct {
	MaxConcurrentJobs int
	MaxVCPU           int // largest VM size a job may ask for
	MonthlyMinutes    int // billable runner minutes per calendar month (UTC)
	// RequireVerified: the email must be verified and either a payment
	// method is on file or the GitHub account is at least MinAccountAge old.
	RequireVerified bool
	MinAccountAge   time.Duration
}

// DefaultLimits are the starting limits per plan. Paid and enterprise plans
// are unlimited here; their capacity is governed by the scheduler.
var DefaultLimits = map[Plan]Limits{
	PlanFree: {
		MaxConcurrentJobs: 2,
		MaxVCPU:           4,
		MonthlyMinutes:    500,
		RequireVerified:   true,
		MinAccountAge:     30 * 24 * time.Hour,
	},
	PlanTrial: {
		MaxConcurrentJobs: 5,
		MaxVCPU:           8,
		MonthlyMinutes:    3000,
		RequireVerified:   true,
		MinAccountAge:     30 * 24 * time.Hour,
	},
	PlanPaid:       {},
	PlanEnterprise: {},
}

// Accounts is the account store.
type Accounts interface {
	Account(ctx context.Context, tenant string) (Account, error)
	Suspend(ctx context.Context, tenant, reason string) error
}

// ErrUnknownTenant is returned by Accounts for a tenant it has no record of.
var ErrUnknownTenant = errors.New("unknown tenant")

// DeniedError is returned by Gate.Admit when a job is refused.
type DeniedError struct {
	Tenant string
	Reason string
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("tenant %s: job refused: %s", e.Tenant, e.Reason)
}

// IsDenied reports whether err is a refusal by the gate.
func IsDenied(err error) bool {
	var d *DeniedError
	return errors.As(err, &d)
}

// Gate admits jobs against their tenant's plan limits and verification.
// Admit, Release and Finish are idempotent per job ID. Safe for concurrent
// use. Usage is kept in memory: a restart forgets this month's minutes until
// persistence lands.
type Gate struct {
	Accounts Accounts
	// Limits per plan; nil uses DefaultLimits.
	Limits map[Plan]Limits
	// UnknownPlan is the plan assumed for a tenant Accounts has no record
	// of. Empty refuses unknown tenants.
	UnknownPlan Plan
	Now         func() time.Time

	mu      sync.Mutex
	running map[string]admitted // job ID ->
	minutes map[string]usage    // tenant ->
}

type admitted struct {
	tenant string
	vcpu   int
}

type usage struct {
	month   string // 2026-11
	seconds float64
}

func (g *Gate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *Gate) limits(p Plan) Limits {
	if g.Limits != nil {
		return g.Limits[p]
	}
	return DefaultLimits[p]
}

func (g *Gate) account(ctx context.Context, tenant string) (Account, error) {
	a, err := g.Accounts.Account(ctx, tenant)
	if errors.Is(err, ErrUnknownTenant) && g.UnknownPlan != "" {
		return Account{Tenant: tenant, Plan: g.UnknownPlan}, nil
	}
	return a, err
}

// PlanOf returns a tenant's plan, or "" if it cannot be looked up.
func (g *Gate) PlanOf(ctx context.Context, tenant string) Plan {
	a, err := g.account(ctx, tenant)
	if err != nil {
		return ""
	}
	return a.Plan
}

// Admit checks a submission against its tenant's account and limits and,
// if allowed, counts it as running. A job already admitted (a deferred job
// being retried) is admitted again without being counted twice.
func (g *Gate) Admit(ctx context.Context, sub jobapi.Submission) error {
	spec := sub.Spec
	g.mu.Lock()
	_, again := g.running[spec.JobID]
	g.mu.Unlock()
	if again {
		return nil
	}

	acct, err := g.account(ctx, spec.Tenant)
	if errors.Is(err, ErrUnknownTenant) {
		return &DeniedError{Tenant: spec.Tenant, Reason: "no account"}
	}
	if err != nil {
		return fmt.Errorf("look up account %s: %w", spec.Tenant, err)
	}
	now := g.now()
	deny := func(format string, a ...any) error {
		return &DeniedError{Tenant: spec.Tenant, Reason: fmt.Sprintf(format, a...)}
	}
	if acct.Suspended {
		return deny("account suspended: %s", acct.SuspendedFor)
	}
	if acct.Plan == PlanTrial && !acct.TrialEndsAt.IsZero() && !now.Before(acct.TrialEndsAt) {
		return deny("trial ended %s", acct.TrialEndsAt.UTC().Format(time.DateOnly))
	}
	lim := g.limits(acct.Plan)
	if lim.RequireVerified {
		if reason := unverified(acct.Verification, lim.MinAccountAge, now); reason != "" {
			return deny("%s plan requires verification: %s", acct.Plan, reason)
		}
	}
	if lim.MaxVCPU > 0 && spec.Hardware.VCPU > lim.MaxVCPU {
		return deny("%s plan allows up to %d vCPU, job asks for %d", acct.Plan, lim.MaxVCPU, spec.Hardware.VCPU)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if _, again := g.running[spec.JobID]; again {
		return nil
	}
	if lim.MonthlyMinutes > 0 {
		u := g.minutes[spec.Tenant]
		if u.month == monthOf(now) && u.seconds >= float64(lim.MonthlyMinutes)*60 {
			return deny("%s plan's %d minutes for this month are used up", acct.Plan, lim.MonthlyMinutes)
		}
	}
	if lim.MaxConcurrentJobs > 0 {
		n := 0
		for _, r := range g.running {
			if r.tenant == spec.Tenant {
				n++
			}
		}
		if n >= lim.MaxConcurrentJobs {
			return deny("%s plan allows %d concurrent jobs", acct.Plan, lim.MaxConcurrentJobs)
		}
	}
	if g.running == nil {
		g.running = map[string]admitted{}
	}
	g.running[spec.JobID] = admitted{tenant: spec.Tenant, vcpu: spec.Hardware.VCPU}
	return nil
}

func unverified(v Verification, minAge time.Duration, now time.Time) string {
	if !v.EmailVerified {
		return "email not verified"
	}
	if v.PaymentMethod {
		return ""
	}
	if !v.GitHubAccountCreated.IsZero() && now.Sub(v.GitHubAccountCreated) >= minAge {
		return ""
	}
	return fmt.Sprintf("add a payment method or use a GitHub account at least %d days old", int(minAge.Hours()/24))
}

// Release forgets an admitted job that never ran (placement failed).
func (g *Gate) Release(jobID string) {
	g.mu.Lock()
	delete(g.running, jobID)
	g.mu.Unlock()
}

// Finish ends an admitted job and charges its billable time to the tenant's
// monthly minutes.
func (g *Gate) Finish(jobID string, billable time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.running[jobID]
	if !ok {
		return
	}
	delete(g.running, jobID)
	if g.minutes == nil {
		g.minutes = map[string]usage{}
	}
	month := monthOf(g.now())
	u := g.minutes[r.tenant]
	if u.month != month {
		u = usage{month: month}
	}
	u.seconds += billable.Seconds()
	g.minutes[r.tenant] = u
}

// MinutesUsed returns a tenant's billable minutes this month.
func (g *Gate) MinutesUsed(tenant string) float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	u := g.minutes[tenant]
	if u.month != monthOf(g.now()) {
		return 0
	}
	return u.seconds / 60
}

// Suspend suspends a tenant: its queued and future jobs are refused.
func (g *Gate) Suspend(ctx context.Context, tenant, reason string) error {
	return g.Accounts.Suspend(ctx, tenant, reason)
}

func monthOf(t time.Time) string { return t.UTC().Format("2006-01") }

// MemAccounts is an in-memory Accounts for development and tests.
type MemAccounts struct {
	mu sync.Mutex
	m  map[string]Account
}

func (s *MemAccounts) Put(a Account) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]Account{}
	}
	s.m[a.Tenant] = a
}

func (s *MemAccounts) Account(_ context.Context, tenant string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.m[tenant]
	if !ok {
		return Account{}, fmt.Errorf("%s: %w", tenant, ErrUnknownTenant)
	}
	return a, nil
}

func (s *MemAccounts) Suspend(_ context.Context, tenant, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.m[tenant]
	if !ok {
		a = Account{Tenant: tenant}
	}
	a.Suspended, a.SuspendedFor = true, reason
	if s.m == nil {
		s.m = map[string]Account{}
	}
	s.m[tenant] = a
	return nil
}
