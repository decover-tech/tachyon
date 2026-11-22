// Package burst runs jobs on rented capacity when owned hosts are full. It
// implements controlplane.Burst.
//
// Peaks overflow to on-demand cloud instances or hourly bare metal. Spot
// instances take only jobs whose usage profile says they are short, since an
// interruption kills the job. When every offer in a region is sold out the
// job spills to the next region (docs/architecture.md, "Scheduler and
// capacity" and "Reliability and operations").
//
// Real cloud and bare-metal APIs live behind Provider; this package only
// decides what to ask for and in which order.
package burst

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
	"github.com/decover-tech/tachyon/internal/scheduler"
)

// Market is how burst capacity is rented.
type Market string

const (
	OnDemand        Market = "on-demand"
	BareMetalHourly Market = "bare-metal-hourly"
	// Spot is interruptible: short jobs only.
	Spot Market = "spot"
)

var (
	// ErrSoldOut is returned by a Provider when an offer has no capacity
	// right now. The pool moves on to the next offer.
	ErrSoldOut = errors.New("offer sold out")
	// ErrNoCapacity means no offer in any region could take the job.
	ErrNoCapacity = errors.New("no burst capacity in any region")
)

// Offer is one kind of capacity a provider rents in one region.
type Offer struct {
	Market Market
	Region string
	Arch   jobapi.Arch
	// MaxVCPU is the largest VM size this offer can host.
	MaxVCPU int
	// PricePerVCPUHour orders offers; cheapest first.
	PricePerVCPUHour float64
}

// Request asks a provider to run one job on one offer.
type Request struct {
	Submission jobapi.Submission
	Offer      Offer
}

// Instance is the rented machine a job was launched on.
type Instance struct {
	ID       string
	Provider string
	Region   string
	Market   Market
}

// Provider is one source of rented capacity (a cloud, a bare-metal vendor).
type Provider interface {
	Name() string
	// Offers lists what this provider can rent.
	Offers() []Offer
	// Launch rents capacity for the job and starts it there. It returns
	// ErrSoldOut (possibly wrapped) when the offer has no capacity.
	Launch(ctx context.Context, req Request) (Instance, error)
}

// ProfileSource returns a job's usage profile; *scheduler.Profiles
// satisfies it.
type ProfileSource interface {
	Get(key string) (scheduler.Profile, bool)
}

// DefaultSpotMaxDuration is the p95 run time under which a job counts as
// short enough for spot.
const DefaultSpotMaxDuration = 10 * time.Minute

// Pool tries providers' offers region by region. Safe for concurrent use
// if its providers are.
type Pool struct {
	Providers []Provider
	// Regions is the spill order. A job's own region (if set) is tried
	// first, then the regions after it in this list, wrapping around.
	Regions []string
	// Profiles decides whether a job is short; nil means no job is.
	Profiles ProfileSource
	// SpotMaxDuration defaults to DefaultSpotMaxDuration.
	SpotMaxDuration time.Duration
	Log             *slog.Logger
}

// Submit implements controlplane.Burst.
func (p *Pool) Submit(ctx context.Context, sub jobapi.Submission) error {
	_, err := p.Launch(ctx, sub)
	return err
}

// Launch runs the job on the cheapest available offer, spilling to the
// next region when a region is full.
func (p *Pool) Launch(ctx context.Context, sub jobapi.Submission) (Instance, error) {
	spot := p.Short(sub.Profile)
	var lastErr error
	for _, region := range p.regionOrder(sub.Region) {
		for _, c := range p.candidates(region, sub.Spec.Hardware, spot) {
			if err := ctx.Err(); err != nil {
				return Instance{}, err
			}
			inst, err := c.provider.Launch(ctx, Request{Submission: sub, Offer: c.offer})
			if err == nil {
				if inst.Provider == "" {
					inst.Provider = c.provider.Name()
				}
				if inst.Region == "" {
					inst.Region = region
				}
				if inst.Market == "" {
					inst.Market = c.offer.Market
				}
				p.log().Info("burst launch", "job", sub.Spec.JobID, "provider", inst.Provider,
					"region", inst.Region, "market", inst.Market, "instance", inst.ID)
				return inst, nil
			}
			if ctx.Err() != nil {
				return Instance{}, ctx.Err()
			}
			if !errors.Is(err, ErrSoldOut) {
				lastErr = fmt.Errorf("%s %s %s: %w", c.provider.Name(), region, c.offer.Market, err)
				p.log().Warn("burst launch failed", "job", sub.Spec.JobID, "err", lastErr)
			}
		}
		p.log().Info("burst region full", "job", sub.Spec.JobID, "region", region)
	}
	if lastErr != nil {
		return Instance{}, fmt.Errorf("%w (last error: %v)", ErrNoCapacity, lastErr)
	}
	return Instance{}, ErrNoCapacity
}

// Short reports whether a profile's jobs are short enough for spot: the
// profile is trusted and its p95 run time is known and within the limit.
func (p *Pool) Short(profile string) bool {
	if p.Profiles == nil || profile == "" {
		return false
	}
	prof, ok := p.Profiles.Get(profile)
	if !ok || prof.P95Duration <= 0 {
		return false
	}
	max := p.SpotMaxDuration
	if max <= 0 {
		max = DefaultSpotMaxDuration
	}
	return prof.P95Duration <= max
}

// regionOrder puts home first, then the rest of Regions after it, wrapping.
func (p *Pool) regionOrder(home string) []string {
	if home == "" {
		return append([]string(nil), p.Regions...)
	}
	start := -1
	for i, r := range p.Regions {
		if r == home {
			start = i
			break
		}
	}
	out := []string{home}
	if start < 0 {
		return append(out, p.Regions...)
	}
	for i := 1; i < len(p.Regions); i++ {
		out = append(out, p.Regions[(start+i)%len(p.Regions)])
	}
	return out
}

type candidate struct {
	provider Provider
	offer    Offer
}

// candidates lists offers in region that fit hw, cheapest first. Spot
// offers are included only when spot is true.
func (p *Pool) candidates(region string, hw jobapi.Hardware, spot bool) []candidate {
	var out []candidate
	for _, pr := range p.Providers {
		for _, o := range pr.Offers() {
			if o.Region != region || o.Arch != hw.Arch || hw.VCPU > o.MaxVCPU {
				continue
			}
			if o.Market == Spot && !spot {
				continue
			}
			out = append(out, candidate{pr, o})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].offer.PricePerVCPUHour < out[j].offer.PricePerVCPUHour
	})
	return out
}

func (p *Pool) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}
