package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Budget limits GitHub API calls per App installation. Each installation
// gets a token bucket sized below GitHub's own per-installation limit, and
// after errors it backs off: until the limit resets when GitHub says it is
// rate limited, exponentially when the API is failing. While an
// installation is throttled the adapter makes no calls for it; queued jobs
// stay pending and the reconcile loop dispatches them once it recovers.
//
// The zero value is ready to use with the defaults below.
type Budget struct {
	// PerHour is the sustained call rate per installation. Default 4000,
	// leaving headroom under GitHub's 5000/hour installation limit.
	PerHour int
	// Burst is the bucket size. Default 100.
	Burst int
	// BackoffBase is the wait after the first consecutive error. Default 5s.
	BackoffBase time.Duration
	// BackoffMax caps the exponential backoff. Default 10m.
	BackoffMax time.Duration

	mu   sync.Mutex
	inst map[int64]*instBudget
}

type instBudget struct {
	tokens   float64
	last     time.Time
	until    time.Time // no calls before this
	failures int       // consecutive errors
}

// minRateLimitWait is how long to wait after a rate-limit response that
// says nothing about when to retry (GitHub's advice for secondary limits).
const minRateLimitWait = time.Minute

// ThrottledError means a call was not made because the installation's
// budget is spent or it is backing off after errors.
type ThrottledError struct {
	Installation int64
	Until        time.Time
}

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("github: installation %d throttled until %s", e.Installation, e.Until.Format(time.RFC3339))
}

// IsThrottled reports whether err is a rate limit: from the local budget or
// from GitHub itself. Throttled work should be retried later, not dropped.
func IsThrottled(err error) bool {
	var te *ThrottledError
	if errors.As(err, &te) {
		return true
	}
	var ae *APIError
	return errors.As(err, &ae) && ae.RateLimited()
}

// IsTransient reports whether err is a GitHub server error (5xx), which is
// worth retrying.
func IsTransient(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status >= 500
}

func (b *Budget) perHour() float64 {
	if b.PerHour > 0 {
		return float64(b.PerHour)
	}
	return 4000
}

func (b *Budget) burst() float64 {
	if b.Burst > 0 {
		return float64(b.Burst)
	}
	return 100
}

func (b *Budget) backoff(failures int) time.Duration {
	base, maxWait := b.BackoffBase, b.BackoffMax
	if base <= 0 {
		base = 5 * time.Second
	}
	if maxWait <= 0 {
		maxWait = 10 * time.Minute
	}
	d := base
	for i := 1; i < failures && d < maxWait; i++ {
		d *= 2
	}
	return min(d, maxWait)
}

// state returns inst's bucket, refilled to now. b.mu must be held.
func (b *Budget) state(inst int64, now time.Time) *instBudget {
	if b.inst == nil {
		b.inst = map[int64]*instBudget{}
	}
	s, ok := b.inst[inst]
	if !ok {
		s = &instBudget{tokens: b.burst(), last: now}
		b.inst[inst] = s
		return s
	}
	if el := now.Sub(s.last); el > 0 {
		s.tokens = min(b.burst(), s.tokens+el.Hours()*b.perHour())
		s.last = now
	}
	return s
}

// wait returns when s can next make a call. b.mu must be held.
func (b *Budget) wait(s *instBudget, now time.Time) time.Time {
	at := now
	if s.until.After(at) {
		at = s.until
	}
	if s.tokens < 1 {
		refill := time.Duration((1 - s.tokens) / b.perHour() * float64(time.Hour))
		if t := now.Add(refill); t.After(at) {
			at = t
		}
	}
	return at
}

// Ready reports whether a call for inst could be made now, without
// spending budget.
func (b *Budget) Ready(inst int64, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.wait(b.state(inst, now), now).After(now)
}

// Take spends one call for inst, or returns a *ThrottledError.
func (b *Budget) Take(inst int64, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.state(inst, now)
	if at := b.wait(s, now); at.After(now) {
		return &ThrottledError{Installation: inst, Until: at}
	}
	s.tokens--
	return nil
}

// Observe records the outcome of a call for inst.
func (b *Budget) Observe(inst int64, now time.Time, err error) {
	if errors.Is(err, context.Canceled) {
		return // our shutdown, not GitHub's health
	}
	var ae *APIError
	isAPI := errors.As(err, &ae)
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.state(inst, now)
	switch {
	case err == nil:
		s.failures = 0
	case isAPI && ae.RateLimited():
		s.failures++
		s.tokens = 0
		var until time.Time
		switch {
		case ae.Remaining == 0 && !ae.Reset.IsZero():
			until = ae.Reset
		case ae.RetryAfter > 0:
			until = now.Add(ae.RetryAfter)
		default:
			until = now.Add(max(b.backoff(s.failures), minRateLimitWait))
		}
		if until.After(s.until) {
			s.until = until
		}
	case isAPI && ae.Status < http.StatusInternalServerError:
		// A plain 4xx: the API is answering; nothing to back off from.
	default: // 5xx or transport error
		s.failures++
		if until := now.Add(b.backoff(s.failures)); until.After(s.until) {
			s.until = until
		}
	}
}
