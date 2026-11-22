package github

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func TestBudgetTokenBucket(t *testing.T) {
	b := &Budget{PerHour: 3600, Burst: 2} // one call per second
	for i := range 2 {
		if err := b.Take(1, t0); err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
	}
	err := b.Take(1, t0)
	var te *ThrottledError
	if !errors.As(err, &te) || te.Installation != 1 || !te.Until.Equal(t0.Add(time.Second)) {
		t.Fatalf("third take = %v, want throttled until +1s", err)
	}
	if !IsThrottled(fmt.Errorf("wrapped: %w", err)) {
		t.Error("IsThrottled(wrapped ThrottledError) = false")
	}
	if b.Ready(1, t0.Add(500*time.Millisecond)) {
		t.Error("ready before a token refilled")
	}
	if !b.Ready(1, t0.Add(time.Second)) {
		t.Error("not ready after a token refilled")
	}
	// Other installations have their own bucket.
	if err := b.Take(2, t0); err != nil {
		t.Errorf("installation 2 throttled by installation 1: %v", err)
	}
	// The bucket never holds more than Burst.
	later := t0.Add(time.Hour)
	for range 2 {
		if err := b.Take(1, later); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Take(1, later); err == nil {
		t.Error("bucket refilled past Burst")
	}
}

func TestBudgetPrimaryRateLimitWaitsForReset(t *testing.T) {
	b := &Budget{}
	reset := t0.Add(17 * time.Minute)
	b.Observe(1, t0, &APIError{Status: 403, Remaining: 0, Reset: reset})
	if b.Ready(1, reset.Add(-time.Second)) {
		t.Error("ready before the rate limit reset")
	}
	if err := b.Take(1, t0); !IsThrottled(err) {
		t.Errorf("take while limited = %v", err)
	}
	// Tokens were drained; by the reset they have refilled.
	if !b.Ready(1, reset) {
		t.Error("not ready at the reset")
	}
}

func TestBudgetSecondaryRateLimit(t *testing.T) {
	b := &Budget{BackoffBase: time.Second}
	b.Observe(1, t0, &APIError{Status: 429, Remaining: -1, RetryAfter: 90 * time.Second})
	if b.Ready(1, t0.Add(89*time.Second)) || !b.Ready(1, t0.Add(90*time.Second+time.Second)) {
		t.Error("Retry-After not honoured")
	}
	// No hint at all: wait at least a minute.
	b.Observe(2, t0, &APIError{Status: 403, Remaining: -1, Body: "You have exceeded a secondary rate limit"})
	if b.Ready(2, t0.Add(59*time.Second)) || !b.Ready(2, t0.Add(time.Minute+time.Second)) {
		t.Error("secondary limit without Retry-After should wait a minute")
	}
}

func TestBudgetBackoffOnErrors(t *testing.T) {
	b := &Budget{BackoffBase: time.Second, BackoffMax: 3 * time.Second}
	now := t0
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 3 * time.Second} {
		b.Observe(1, now, &APIError{Status: 502, Remaining: -1})
		if b.Ready(1, now.Add(want-time.Millisecond)) || !b.Ready(1, now.Add(want)) {
			t.Fatalf("failure %d: backoff not %v", i+1, want)
		}
		now = now.Add(want)
	}
	// Transport errors back off too; success resets the streak.
	b.Observe(1, now, nil)
	b.Observe(1, now, errors.New("connection reset"))
	if !b.Ready(1, now.Add(time.Second)) {
		t.Error("success did not reset the backoff")
	}
}

func TestBudgetIgnoresClientErrorsAndCancellation(t *testing.T) {
	b := &Budget{}
	b.Observe(1, t0, &APIError{Status: 404, Remaining: 4000})
	b.Observe(1, t0, &APIError{Status: 422, Remaining: 4000})
	b.Observe(1, t0, fmt.Errorf("call: %w", context.Canceled))
	if !b.Ready(1, t0) {
		t.Error("4xx or cancellation caused a backoff")
	}
}

func TestAPIErrorRateLimited(t *testing.T) {
	for _, tc := range []struct {
		e    APIError
		want bool
	}{
		{APIError{Status: 429, Remaining: -1}, true},
		{APIError{Status: 403, Remaining: 0}, true},
		{APIError{Status: 403, Remaining: -1, RetryAfter: time.Second}, true},
		{APIError{Status: 403, Remaining: -1, Body: `{"message":"API rate limit exceeded"}`}, true},
		{APIError{Status: 403, Remaining: 10, Body: `{"message":"Resource not accessible by integration"}`}, false},
		{APIError{Status: 500, Remaining: 0}, false},
	} {
		if got := tc.e.RateLimited(); got != tc.want {
			t.Errorf("%+v.RateLimited() = %v", tc.e, got)
		}
		if IsThrottled(&tc.e) != tc.want {
			t.Errorf("IsThrottled(%+v) != %v", tc.e, tc.want)
		}
	}
}
