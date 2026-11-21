package mirrors

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Prober checks one mirror instance.
type Prober interface {
	Probe(ctx context.Context, base string) error
}

// HTTPProber probes GET <base>/healthz, which tachyon-mirror answers 200
// while its cache is writable.
type HTTPProber struct {
	Client *http.Client
}

func (p HTTPProber) Probe(ctx context.Context, base string) error {
	c := p.Client
	if c == nil {
		c = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}

// Monitor health-checks a region's mirror pair and says which instance
// guests should use: the primary while it is up, else the secondary, else
// none (guests are pointed at the upstream registries).
//
// A member is marked down after Fall consecutive failed probes and up again
// after Rise consecutive successes, so one dropped probe doesn't flap every
// new job between instances. Until a member has been probed it counts as
// down.
type Monitor struct {
	Primary, Secondary string // base URLs of the two tachyon-mirror instances
	Prober             Prober
	Fall, Rise         int           // default 3 and 2
	Timeout            time.Duration // per probe; default 2s
	Log                *slog.Logger

	mu         sync.Mutex
	prim, secd member
}

type member struct {
	known      bool
	up         bool
	okRun, bad int
}

func (m *member) observe(ok bool, fall, rise int) (changed bool) {
	if !m.known {
		m.known, m.up = true, ok
		return true
	}
	if ok {
		m.bad = 0
		m.okRun++
		if !m.up && m.okRun >= rise {
			m.up = true
			return true
		}
	} else {
		m.okRun = 0
		m.bad++
		if m.up && m.bad >= fall {
			m.up = false
			return true
		}
	}
	return false
}

func (m *Monitor) params() (fall, rise int, timeout time.Duration) {
	fall, rise, timeout = m.Fall, m.Rise, m.Timeout
	if fall <= 0 {
		fall = 3
	}
	if rise <= 0 {
		rise = 2
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return
}

// Check probes both members once, concurrently.
func (m *Monitor) Check(ctx context.Context) {
	fall, rise, timeout := m.params()
	probe := func(base string) bool {
		if base == "" {
			return false
		}
		pctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		err := m.Prober.Probe(pctx, base)
		if err != nil && m.Log != nil {
			m.Log.Debug("mirror probe failed", "mirror", base, "err", err)
		}
		return err == nil
	}
	var pOK, sOK bool
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pOK = probe(m.Primary) }()
	go func() { defer wg.Done(); sOK = probe(m.Secondary) }()
	wg.Wait()

	m.mu.Lock()
	before := m.pickLocked()
	pc := m.prim.observe(pOK, fall, rise)
	sc := m.secd.observe(sOK, fall, rise)
	after := m.pickLocked()
	pair := m.pairLocked()
	m.mu.Unlock()

	if m.Log != nil && (pc || sc) {
		m.Log.Info("mirror pair health", "primary", m.Primary, "primary_up", pair.PrimaryUp,
			"secondary", m.Secondary, "secondary_up", pair.SecondaryUp, "serving", after)
	}
	if m.Log != nil && before != after && after == "" {
		m.Log.Warn("both mirrors down; guests fall back to upstream registries")
	}
}

// Run checks every interval until ctx is done, starting immediately.
func (m *Monitor) Run(ctx context.Context, interval time.Duration) {
	m.Check(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Check(ctx)
		}
	}
}

func (m *Monitor) pairLocked() Pair {
	return Pair{
		Primary: m.Primary, Secondary: m.Secondary,
		PrimaryUp:   m.Primary != "" && m.prim.up,
		SecondaryUp: m.Secondary != "" && m.secd.up,
	}
}

func (m *Monitor) pickLocked() string { return m.pairLocked().Pick() }

// Pair returns the pair's current health.
func (m *Monitor) Pair() Pair {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pairLocked()
}

// Endpoints returns what new guests should use: the healthy instance's
// endpoints, or none (upstream) if both are down.
func (m *Monitor) Endpoints() Endpoints {
	return EndpointsFor(m.Pair().Pick())
}
