// Package billing meters usage and prices it into invoices. It bills from
// the same per-job telemetry.Record that drives pickup latency and the
// "minutes saved" report, so billed runner time and measured runner time
// can never disagree.
//
// The model (docs/architecture.md, "Observability, metering and billing"):
//
//   - Runner time is billed per second, priced by VM size.
//   - Prebuild runs are included up to a per-repo allowance each billing
//     period; prebuild time above the allowance is billed as runner time.
//   - Storage above a per-repo quota is billed per GB-month.
//
// Prices are an open decision (D5), so they are configuration (PriceBook),
// not constants.
package billing

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/decover-tech/tachyon/internal/telemetry"
)

// Money is an amount in micro-USD (1 USD = 1_000_000).
type Money int64

func (m Money) String() string {
	sign := ""
	if m < 0 {
		sign, m = "-", -m
	}
	return fmt.Sprintf("%s$%d.%06d", sign, int64(m)/1e6, int64(m)%1e6)
}

// GB is the unit of storage billing (binary gigabyte, as cloud providers
// bill GB-months).
const GB = 1 << 30

// Month is the length of one GB-month: 730 hours, the average month.
const Month = 730 * time.Hour

// PriceBook holds prices and allowances.
type PriceBook struct {
	// RunnerPerMinute is the price of one minute of runner time, by vCPU
	// count. Usage is metered per second and priced pro rata.
	RunnerPerMinute map[int]Money
	// PrebuildAllowance is the prebuild runner time included per repo per
	// billing period.
	PrebuildAllowance time.Duration
	// StorageQuotaBytes is the storage included per repo. Storage above it
	// is billed.
	StorageQuotaBytes int64
	// StoragePerGBMonth is the price of one GB held above quota for one
	// Month.
	StoragePerGBMonth Money
}

// Period is a calendar month in UTC, formatted "2006-01".
type Period string

// PeriodOf returns the billing period containing t.
func PeriodOf(t time.Time) Period { return Period(t.UTC().Format("2006-01")) }

func periodStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// Kind is what a usage line is for.
type Kind string

const (
	// KindRunner is per-second runner time for ordinary jobs.
	KindRunner Kind = "runner"
	// KindPrebuildIncluded is prebuild runner time within the allowance;
	// it is shown on the invoice at no charge.
	KindPrebuildIncluded Kind = "prebuild_included"
	// KindPrebuildOverage is prebuild runner time above the allowance,
	// priced as runner time.
	KindPrebuildOverage Kind = "prebuild_overage"
	// KindStorage is storage above quota, in GB-months.
	KindStorage Kind = "storage"
)

var (
	ErrNotFinished = errors.New("billing: job has no billable runner time yet")
	ErrUnpriced    = errors.New("billing: no price for VM size")
	ErrOutOfOrder  = errors.New("billing: storage sample older than the previous one")
)

type repoKey struct{ tenant, repo string }

type runnerKey struct {
	tenant, repo string
	period       Period
	vcpu         int
	kind         Kind
}

type periodKey struct {
	tenant, repo string
	period       Period
}

type storageSample struct {
	bytes int64
	at    time.Time
}

// Meter accumulates usage. Safe for concurrent use. State is in memory,
// like the rest of the Phase 1 control plane.
type Meter struct {
	Prices PriceBook

	mu       sync.Mutex
	seen     map[string]bool
	runner   map[runnerKey]time.Duration
	prebuild map[periodKey]time.Duration // prebuild time used, all of it
	last     map[repoKey]storageSample
	storage  map[periodKey]float64 // GB-seconds above quota
}

// NewMeter returns a meter using prices.
func NewMeter(prices PriceBook) *Meter {
	return &Meter{Prices: prices}
}

func (m *Meter) init() {
	if m.seen == nil {
		m.seen = map[string]bool{}
		m.runner = map[runnerKey]time.Duration{}
		m.prebuild = map[periodKey]time.Duration{}
		m.last = map[repoKey]storageSample{}
		m.storage = map[periodKey]float64{}
	}
}

// RecordJob meters a finished job from its telemetry record: billable
// runner time (VM resumed to job finished, whole seconds) at the record's
// VM size, in the period the job finished. Prebuild runs draw down the
// repo's allowance first. Recording the same job twice is a no-op, so
// callers may record on every terminal event.
func (m *Meter) RecordJob(rec *telemetry.Record) error {
	d, ok := rec.Billable()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFinished, rec.JobID)
	}
	if _, ok := m.Prices.RunnerPerMinute[rec.VCPU]; !ok {
		return fmt.Errorf("%w: %d vcpu (job %s)", ErrUnpriced, rec.VCPU, rec.JobID)
	}
	finished, _ := rec.At(telemetry.JobFinished)
	period := PeriodOf(finished)
	if d < 0 {
		d = 0
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	if m.seen[rec.JobID] {
		return nil
	}
	m.seen[rec.JobID] = true

	key := runnerKey{tenant: rec.Tenant, repo: rec.Repo, period: period, vcpu: rec.VCPU, kind: KindRunner}
	if !rec.Prebuild {
		m.runner[key] += d
		return nil
	}
	pk := periodKey{tenant: rec.Tenant, repo: rec.Repo, period: period}
	remaining := max(m.Prices.PrebuildAllowance-m.prebuild[pk], 0)
	included := min(d, remaining)
	m.prebuild[pk] += d
	if included > 0 {
		key.kind = KindPrebuildIncluded
		m.runner[key] += included
	}
	if over := d - included; over > 0 {
		key.kind = KindPrebuildOverage
		m.runner[key] += over
	}
	return nil
}

// PrebuildUsed returns the prebuild runner time a repo has used in a
// period and how much of its allowance is left.
func (m *Meter) PrebuildUsed(tenant, repo string, p Period) (used, remaining time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	used = m.prebuild[periodKey{tenant: tenant, repo: repo, period: p}]
	return used, max(m.Prices.PrebuildAllowance-used, 0)
}

// ObserveStorage records a repo's total stored bytes at a time. Storage
// above quota is integrated between samples: the previous sample's excess
// is held constant until this one, split across period boundaries.
func (m *Meter) ObserveStorage(tenant, repo string, bytes int64, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	k := repoKey{tenant, repo}
	prev, ok := m.last[k]
	if ok && at.Before(prev.at) {
		return fmt.Errorf("%w: %s/%s at %s", ErrOutOfOrder, tenant, repo, at)
	}
	if ok {
		m.accrueLocked(k, prev, at)
	}
	m.last[k] = storageSample{bytes: bytes, at: at}
	return nil
}

// Accrue extends every repo's last storage sample to at, so an invoice
// covers storage up to that instant. Call it at the end of a period
// before invoicing.
func (m *Meter) Accrue(at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	for k, prev := range m.last {
		if at.After(prev.at) {
			m.accrueLocked(k, prev, at)
			m.last[k] = storageSample{bytes: prev.bytes, at: at}
		}
	}
}

func (m *Meter) accrueLocked(k repoKey, prev storageSample, to time.Time) {
	excess := prev.bytes - m.Prices.StorageQuotaBytes
	if excess <= 0 {
		return
	}
	gb := float64(excess) / GB
	from := prev.at
	for from.Before(to) {
		end := periodStart(from).AddDate(0, 1, 0)
		if end.After(to) {
			end = to
		}
		pk := periodKey{tenant: k.tenant, repo: k.repo, period: PeriodOf(from)}
		m.storage[pk] += gb * end.Sub(from).Seconds()
		from = end
	}
}

// Line is one invoice line.
type Line struct {
	Repo     string
	Kind     Kind
	VCPU     int     // runner and prebuild lines
	Quantity float64 // seconds for runner and prebuild lines, GB-months for storage
	Unit     string  // "second" or "GB-month"
	Amount   Money
}

// Invoice is a tenant's charges for one period.
type Invoice struct {
	Tenant string
	Period Period
	Lines  []Line
	Total  Money
}

// Invoice prices a tenant's usage for a period. Lines are sorted by repo,
// kind and VM size. Each line's amount is rounded to the nearest micro-USD.
func (m *Meter) Invoice(tenant string, p Period) Invoice {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv := Invoice{Tenant: tenant, Period: p}
	for k, d := range m.runner {
		if k.tenant != tenant || k.period != p || d <= 0 {
			continue
		}
		secs := int64(d / time.Second)
		var amt Money
		if k.kind != KindPrebuildIncluded {
			per := int64(m.Prices.RunnerPerMinute[k.vcpu])
			amt = Money((secs*per + 30) / 60)
		}
		inv.Lines = append(inv.Lines, Line{Repo: k.repo, Kind: k.kind, VCPU: k.vcpu, Quantity: float64(secs), Unit: "second", Amount: amt})
	}
	for k, gbs := range m.storage {
		if k.tenant != tenant || k.period != p || gbs <= 0 {
			continue
		}
		gbm := gbs / Month.Seconds()
		amt := Money(math.Round(gbm * float64(m.Prices.StoragePerGBMonth)))
		inv.Lines = append(inv.Lines, Line{Repo: k.repo, Kind: KindStorage, Quantity: gbm, Unit: "GB-month", Amount: amt})
	}
	sort.Slice(inv.Lines, func(i, j int) bool {
		a, b := inv.Lines[i], inv.Lines[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.VCPU < b.VCPU
	})
	for _, l := range inv.Lines {
		inv.Total += l.Amount
	}
	return inv
}
