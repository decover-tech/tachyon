package billing

import (
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/telemetry"
)

func TestOffPeakApply(t *testing.T) {
	cases := []struct {
		name       string
		o          OffPeak
		amount     int64
		deferrable bool
		want       int64
	}{
		{"default discount", OffPeak{}, 1000, true, 600},
		{"not deferrable", OffPeak{}, 1000, false, 1000},
		{"custom discount", OffPeak{DiscountBP: 2500}, 1000, true, 750},
		{"rounds half up", OffPeak{DiscountBP: 5000}, 3, true, 2},
		{"free", OffPeak{DiscountBP: 10000}, 1000, true, 0},
		{"disabled", OffPeak{DiscountBP: NoDiscount}, 1000, true, 1000},
		{"zero amount", OffPeak{}, 0, true, 0},
	}
	for _, c := range cases {
		if got := c.o.Apply(c.amount, c.deferrable); got != c.want {
			t.Errorf("%s: Apply(%d) = %d, want %d", c.name, c.amount, got, c.want)
		}
	}
}

func TestOffPeakValidate(t *testing.T) {
	for _, bp := range []int{0, 1, 4000, 10000, NoDiscount} {
		if err := (OffPeak{DiscountBP: bp}).Validate(); err != nil {
			t.Errorf("%d: %v", bp, err)
		}
	}
	for _, bp := range []int{10001, -2} {
		if (OffPeak{DiscountBP: bp}).Validate() == nil {
			t.Errorf("%d accepted", bp)
		}
	}
}

func TestOffPeakJobCharge(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	rec := telemetry.NewRecord("j1")
	if _, ok := (OffPeak{}).JobCharge(rec, 100); ok {
		t.Fatal("unfinished job charged")
	}
	rec.Mark(telemetry.VMResumed, t0)
	rec.Mark(telemetry.JobFinished, t0.Add(10*time.Minute))
	if got, ok := (OffPeak{}).JobCharge(rec, 100); !ok || got != 60000 {
		t.Fatalf("on-demand charge = %d, %v", got, ok)
	}
	rec.Deferrable = true
	if got, _ := (OffPeak{}).JobCharge(rec, 100); got != 36000 {
		t.Fatalf("deferrable charge = %d", got)
	}
}
