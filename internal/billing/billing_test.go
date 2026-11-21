package billing

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/telemetry"
)

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func prices() PriceBook {
	return PriceBook{
		RunnerPerMinute:   map[int]Money{2: 3_000, 4: 6_000, 8: 12_000},
		PrebuildAllowance: 10 * time.Minute,
		StorageQuotaBytes: 50 * GB,
		StoragePerGBMonth: 100_000, // $0.10
	}
}

func job(id, repo string, vcpu int, runFor time.Duration, prebuild bool) *telemetry.Record {
	r := telemetry.NewRecord(id)
	r.Tenant, r.Repo, r.VCPU, r.Prebuild = "acme", repo, vcpu, prebuild
	r.Mark(telemetry.WebhookReceived, t0.Add(-time.Second))
	r.Mark(telemetry.VMResumed, t0)
	r.Mark(telemetry.JobFinished, t0.Add(runFor))
	return r
}

func TestRunnerTimePerSecondBySize(t *testing.T) {
	m := NewMeter(prices())
	for _, r := range []*telemetry.Record{
		job("a", "api", 2, 90*time.Second, false),
		job("b", "api", 2, 31*time.Second, false),
		job("c", "api", 8, 61*time.Second+400*time.Millisecond, false), // rounds to 61s
	} {
		if err := m.RecordJob(r); err != nil {
			t.Fatal(err)
		}
	}
	// Recording a job again does not double-bill.
	if err := m.RecordJob(job("a", "api", 2, 90*time.Second, false)); err != nil {
		t.Fatal(err)
	}
	inv := m.Invoice("acme", "2026-11")
	if len(inv.Lines) != 2 {
		t.Fatalf("lines = %+v", inv.Lines)
	}
	l2, l8 := inv.Lines[0], inv.Lines[1]
	if l2.VCPU != 2 || l2.Kind != KindRunner || l2.Quantity != 121 || l2.Amount != 6_050 {
		t.Errorf("2 vcpu line = %+v, want 121s at $0.003/min = 6050", l2)
	}
	if l8.VCPU != 8 || l8.Quantity != 61 || l8.Amount != 12_200 {
		t.Errorf("8 vcpu line = %+v, want 61s = 12200", l8)
	}
	if inv.Total != 18_250 || inv.Total.String() != "$0.018250" {
		t.Errorf("total = %v", inv.Total)
	}
	if got := m.Invoice("acme", "2026-12"); len(got.Lines) != 0 {
		t.Errorf("other period billed: %+v", got)
	}
	if got := m.Invoice("other", "2026-11"); len(got.Lines) != 0 {
		t.Errorf("other tenant billed: %+v", got)
	}
}

func TestRecordJobErrors(t *testing.T) {
	m := NewMeter(prices())
	unfinished := telemetry.NewRecord("u")
	unfinished.VCPU = 2
	unfinished.Mark(telemetry.VMResumed, t0)
	if err := m.RecordJob(unfinished); !errors.Is(err, ErrNotFinished) {
		t.Errorf("unfinished: %v", err)
	}
	if err := m.RecordJob(job("x", "api", 16, time.Minute, false)); !errors.Is(err, ErrUnpriced) {
		t.Errorf("unpriced: %v", err)
	}
	// A rejected job can be recorded once it is billable.
	unfinished.Mark(telemetry.JobFinished, t0.Add(time.Minute))
	if err := m.RecordJob(unfinished); err != nil {
		t.Errorf("after finish: %v", err)
	}
}

func TestPrebuildAllowancePerRepo(t *testing.T) {
	m := NewMeter(prices())
	for _, r := range []*telemetry.Record{
		job("p1", "api", 4, 6*time.Minute, true),
		job("p2", "api", 4, 6*time.Minute, true), // 4m included, 2m over
		job("p3", "web", 4, 3*time.Minute, true), // separate allowance
		job("j1", "api", 4, time.Minute, false),  // ordinary job, does not draw down
	} {
		if err := m.RecordJob(r); err != nil {
			t.Fatal(err)
		}
	}
	used, left := m.PrebuildUsed("acme", "api", "2026-11")
	if used != 12*time.Minute || left != 0 {
		t.Errorf("api used=%v left=%v", used, left)
	}
	used, left = m.PrebuildUsed("acme", "web", "2026-11")
	if used != 3*time.Minute || left != 7*time.Minute {
		t.Errorf("web used=%v left=%v", used, left)
	}

	want := []Line{
		{Repo: "api", Kind: KindPrebuildIncluded, VCPU: 4, Quantity: 600, Unit: "second", Amount: 0},
		{Repo: "api", Kind: KindPrebuildOverage, VCPU: 4, Quantity: 120, Unit: "second", Amount: 12_000},
		{Repo: "api", Kind: KindRunner, VCPU: 4, Quantity: 60, Unit: "second", Amount: 6_000},
		{Repo: "web", Kind: KindPrebuildIncluded, VCPU: 4, Quantity: 180, Unit: "second", Amount: 0},
	}
	inv := m.Invoice("acme", "2026-11")
	if len(inv.Lines) != len(want) {
		t.Fatalf("lines = %+v", inv.Lines)
	}
	for i := range want {
		if inv.Lines[i] != want[i] {
			t.Errorf("line %d = %+v, want %+v", i, inv.Lines[i], want[i])
		}
	}
	if inv.Total != 18_000 {
		t.Errorf("total = %v", inv.Total)
	}

	// The allowance resets in the next period.
	m2 := NewMeter(prices())
	r := telemetry.NewRecord("p5")
	r.Tenant, r.Repo, r.VCPU, r.Prebuild = "acme", "api", 4, true
	dec := time.Date(2026, 12, 1, 0, 0, 30, 0, time.UTC)
	r.Mark(telemetry.VMResumed, dec.Add(-time.Minute))
	r.Mark(telemetry.JobFinished, dec)
	if err := m2.RecordJob(job("p1", "api", 4, 10*time.Minute, true)); err != nil {
		t.Fatal(err)
	}
	if err := m2.RecordJob(r); err != nil {
		t.Fatal(err)
	}
	if _, left := m2.PrebuildUsed("acme", "api", "2026-12"); left != 9*time.Minute {
		t.Errorf("december allowance left = %v", left)
	}
}

func TestStorageAboveQuotaPerGBMonth(t *testing.T) {
	m := NewMeter(prices())
	// 60 GB (10 over quota) for half a Month, then 40 GB (under) for the rest.
	if err := m.ObserveStorage("acme", "api", 60*GB, t0); err != nil {
		t.Fatal(err)
	}
	if err := m.ObserveStorage("acme", "api", 40*GB, t0.Add(Month/2)); err != nil {
		t.Fatal(err)
	}
	m.Accrue(t0.Add(Month / 2).Add(48 * time.Hour))
	if err := m.ObserveStorage("acme", "api", 1, t0); !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("out of order: %v", err)
	}
	inv := m.Invoice("acme", "2026-11")
	if len(inv.Lines) != 1 {
		t.Fatalf("lines = %+v", inv.Lines)
	}
	l := inv.Lines[0]
	if l.Kind != KindStorage || l.Unit != "GB-month" || math.Abs(l.Quantity-5) > 1e-9 || l.Amount != 500_000 {
		t.Errorf("storage line = %+v, want 5 GB-months = $0.50", l)
	}
}

func TestStorageSplitsAtPeriodBoundary(t *testing.T) {
	m := NewMeter(prices())
	end := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	if err := m.ObserveStorage("acme", "api", 51*GB, end.Add(-73*time.Hour)); err != nil {
		t.Fatal(err)
	}
	m.Accrue(end.Add(146 * time.Hour))
	nov := m.Invoice("acme", "2026-11").Lines
	dec := m.Invoice("acme", "2026-12").Lines
	if len(nov) != 1 || len(dec) != 1 {
		t.Fatalf("nov=%+v dec=%+v", nov, dec)
	}
	if math.Abs(nov[0].Quantity-0.1) > 1e-9 || math.Abs(dec[0].Quantity-0.2) > 1e-9 {
		t.Errorf("nov=%v dec=%v GB-months, want 0.1 and 0.2", nov[0].Quantity, dec[0].Quantity)
	}
	if nov[0].Amount != 10_000 || dec[0].Amount != 20_000 {
		t.Errorf("amounts nov=%v dec=%v", nov[0].Amount, dec[0].Amount)
	}
}

func TestMoneyString(t *testing.T) {
	for m, want := range map[Money]string{0: "$0.000000", 1_500_000: "$1.500000", -25: "-$0.000025"} {
		if got := m.String(); got != want {
			t.Errorf("%d: %q, want %q", m, got, want)
		}
	}
}
