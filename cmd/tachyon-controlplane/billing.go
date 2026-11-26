package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/decover-tech/tachyon/internal/billing"
)

// parseRunnerPrices parses "2=0.004,4=0.008": USD per runner minute by
// VM vCPUs.
func parseRunnerPrices(s string) (map[int]billing.Money, error) {
	out := map[int]billing.Money{}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			return nil, fmt.Errorf("%q: want vcpu=usd", kv)
		}
		vcpu, err := strconv.Atoi(k)
		if err != nil || vcpu <= 0 {
			return nil, fmt.Errorf("%q: vcpu must be a positive integer", kv)
		}
		price, err := strconv.ParseFloat(v, 64)
		if err != nil || price < 0 || math.IsInf(price, 0) {
			return nil, fmt.Errorf("%q: price must be a non-negative number", kv)
		}
		if _, dup := out[vcpu]; dup {
			return nil, fmt.Errorf("%q: vcpu %d priced twice", kv, vcpu)
		}
		out[vcpu] = usd(price)
	}
	return out, nil
}

func usd(f float64) billing.Money { return billing.Money(math.Round(f * 1e6)) }

// previousMonth returns the start of the UTC month before t's.
func previousMonth(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month()-1, 1, 0, 0, 0, 0, time.UTC)
}
