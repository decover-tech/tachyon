package billing

import (
	"fmt"

	"github.com/decover-tech/tachyon/internal/telemetry"
)

// DefaultOffPeakDiscountBP is the deferrable class's discount off the
// runner rate, in basis points (4000 = 40% off). It is a placeholder:
// prices are open decision D5, set once Phase 0 gives a real cost per
// job-minute.
const DefaultOffPeakDiscountBP = 4000

// OffPeak prices the deferrable off-peak class (the tachyon-deferrable
// label). A deferrable job is discounted whether it ran in a trough or was
// started at its deferral bound: the customer accepted the wait either way.
type OffPeak struct {
	// DiscountBP is the discount in basis points, 0..10000. Zero means
	// DefaultOffPeakDiscountBP; use NoDiscount to turn it off.
	DiscountBP int
}

// NoDiscount disables the off-peak discount.
const NoDiscount = -1

func (o OffPeak) discountBP() int {
	switch {
	case o.DiscountBP == 0:
		return DefaultOffPeakDiscountBP
	case o.DiscountBP < 0:
		return 0
	}
	return o.DiscountBP
}

// Validate rejects a discount above 100%.
func (o OffPeak) Validate() error {
	if o.DiscountBP > 10000 || (o.DiscountBP < 0 && o.DiscountBP != NoDiscount) {
		return fmt.Errorf("off-peak discount %d bp out of range 0..10000", o.DiscountBP)
	}
	return nil
}

// Apply returns amount (any unit, e.g. micro-USD) after the class discount,
// rounded half up. Amounts for non-deferrable jobs pass through unchanged.
func (o OffPeak) Apply(amount int64, deferrable bool) int64 {
	if !deferrable || amount <= 0 {
		return amount
	}
	keep := int64(10000 - o.discountBP())
	return (amount*keep + 5000) / 10000
}

// JobCharge prices a finished job's billable runner time at ratePerSecond
// (micro-USD per second for its VM size), discounted if the job ran in
// the deferrable class. ok is false if the job has no billable time yet.
func (o OffPeak) JobCharge(rec *telemetry.Record, ratePerSecond int64) (microUSD int64, ok bool) {
	d, ok := rec.Billable()
	if !ok {
		return 0, false
	}
	return o.Apply(int64(d.Seconds())*ratePerSecond, rec.Deferrable), true
}
