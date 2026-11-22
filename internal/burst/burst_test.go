package burst

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/decover-tech/tachyon/internal/controlplane"
	"github.com/decover-tech/tachyon/internal/jobapi"
	"github.com/decover-tech/tachyon/internal/scheduler"
)

var _ controlplane.Burst = (*Pool)(nil)

// fakeProvider records launch attempts; offers listed in soldOut (keyed
// region/market) fail with ErrSoldOut, those in failing with another error.
type fakeProvider struct {
	name     string
	offers   []Offer
	soldOut  map[string]bool
	failing  map[string]bool
	attempts []string
}

func (f *fakeProvider) Name() string    { return f.name }
func (f *fakeProvider) Offers() []Offer { return f.offers }

func (f *fakeProvider) Launch(_ context.Context, r Request) (Instance, error) {
	k := r.Offer.Region + "/" + string(r.Offer.Market)
	f.attempts = append(f.attempts, f.name+":"+k)
	if f.soldOut[k] {
		return Instance{}, fmt.Errorf("capacity: %w", ErrSoldOut)
	}
	if f.failing[k] {
		return Instance{}, errors.New("api error")
	}
	return Instance{ID: "i-" + r.Submission.Spec.JobID}, nil
}

func offer(region string, m Market, price float64) Offer {
	return Offer{Market: m, Region: region, Arch: jobapi.ArchX86_64, MaxVCPU: 16, PricePerVCPUHour: price}
}

func sub(id, profile, region string) jobapi.Submission {
	return jobapi.Submission{
		Spec:    jobapi.Spec{JobID: id, Hardware: jobapi.NewHardware(jobapi.ArchX86_64, 8)},
		Profile: profile,
		Region:  region,
	}
}

// profiles returns Profiles where "short" runs ~2m and "long" ~40m.
func profiles() *scheduler.Profiles {
	p := &scheduler.Profiles{MinSamples: 3}
	for i := 0; i < 5; i++ {
		p.Observe("short", scheduler.Usage{CPU: 1, MemGB: 1, Duration: 2 * time.Minute})
		p.Observe("long", scheduler.Usage{CPU: 1, MemGB: 1, Duration: 40 * time.Minute})
		p.Observe("unknown-duration", scheduler.Usage{CPU: 1, MemGB: 1})
	}
	p.Observe("few-samples", scheduler.Usage{CPU: 1, MemGB: 1, Duration: time.Minute})
	return p
}

func cloud() *fakeProvider {
	return &fakeProvider{name: "cloud", offers: []Offer{
		offer("us-east", OnDemand, 0.05),
		offer("us-east", Spot, 0.015),
		offer("eu-west", OnDemand, 0.06),
		offer("eu-west", Spot, 0.02),
	}}
}

func TestShortOnlyForProfilesWithShortKnownDuration(t *testing.T) {
	p := &Pool{Profiles: profiles()}
	for profile, want := range map[string]bool{
		"short":            true,
		"long":             false,
		"unknown-duration": false,
		"few-samples":      false, // profile not trusted yet
		"never-seen":       false,
		"":                 false,
	} {
		if got := p.Short(profile); got != want {
			t.Errorf("Short(%q) = %v, want %v", profile, got, want)
		}
	}
	if (&Pool{}).Short("short") {
		t.Error("no profile source: nothing is short")
	}
	p.SpotMaxDuration = time.Hour
	if !p.Short("long") {
		t.Error("SpotMaxDuration not honored")
	}
}

func TestSpotOnlyForShortJobs(t *testing.T) {
	c := cloud()
	p := &Pool{Providers: []Provider{c}, Regions: []string{"us-east"}, Profiles: profiles()}

	inst, err := p.Launch(context.Background(), sub("j1", "short", ""))
	if err != nil || inst.Market != Spot || inst.Provider != "cloud" || inst.Region != "us-east" {
		t.Fatalf("short job: %+v, %v", inst, err)
	}
	for _, prof := range []string{"long", "unknown-duration", "never-seen"} {
		c.attempts = nil
		inst, err := p.Launch(context.Background(), sub("j-"+prof, prof, ""))
		if err != nil || inst.Market != OnDemand {
			t.Fatalf("%s job: %+v, %v", prof, inst, err)
		}
		if !reflect.DeepEqual(c.attempts, []string{"cloud:us-east/on-demand"}) {
			t.Fatalf("%s job tried spot: %v", prof, c.attempts)
		}
	}
}

func TestCheapestOfferAcrossProvidersAndFallbackWhenSoldOut(t *testing.T) {
	c := cloud()
	metal := &fakeProvider{name: "metal", offers: []Offer{offer("us-east", BareMetalHourly, 0.03)}}
	p := &Pool{Providers: []Provider{c, metal}, Regions: []string{"us-east"}, Profiles: profiles()}

	inst, err := p.Launch(context.Background(), sub("j1", "long", ""))
	if err != nil || inst.Provider != "metal" || inst.Market != BareMetalHourly {
		t.Fatalf("want cheapest non-spot offer (metal): %+v, %v", inst, err)
	}

	metal.soldOut = map[string]bool{"us-east/bare-metal-hourly": true}
	inst, err = p.Launch(context.Background(), sub("j2", "long", ""))
	if err != nil || inst.Provider != "cloud" || inst.Market != OnDemand {
		t.Fatalf("want on-demand after metal sold out: %+v, %v", inst, err)
	}
}

func TestSpillsToNextRegionWhenRegionFull(t *testing.T) {
	c := cloud()
	c.soldOut = map[string]bool{"us-east/on-demand": true, "us-east/spot": true}
	p := &Pool{Providers: []Provider{c}, Regions: []string{"eu-west", "us-east", "ap-south"}, Profiles: profiles()}

	inst, err := p.Launch(context.Background(), sub("j1", "short", "us-east"))
	if err != nil || inst.Region != "eu-west" || inst.Market != Spot {
		t.Fatalf("want spill to eu-west: %+v, %v", inst, err)
	}
	want := []string{"cloud:us-east/spot", "cloud:us-east/on-demand", "cloud:eu-west/spot"}
	if !reflect.DeepEqual(c.attempts, want) {
		t.Fatalf("attempts = %v, want %v", c.attempts, want)
	}
}

func TestRegionOrder(t *testing.T) {
	p := &Pool{Regions: []string{"a", "b", "c"}}
	for home, want := range map[string][]string{
		"":  {"a", "b", "c"},
		"a": {"a", "b", "c"},
		"b": {"b", "c", "a"},
		"c": {"c", "a", "b"},
		"z": {"z", "a", "b", "c"},
	} {
		if got := p.regionOrder(home); !reflect.DeepEqual(got, want) {
			t.Errorf("regionOrder(%q) = %v, want %v", home, got, want)
		}
	}
}

func TestNoCapacityAnywhere(t *testing.T) {
	c := cloud()
	c.soldOut = map[string]bool{"us-east/on-demand": true, "eu-west/on-demand": true}
	c.failing = map[string]bool{"eu-west/on-demand": true}
	p := &Pool{Providers: []Provider{c}, Regions: []string{"us-east", "eu-west"}}

	err := p.Submit(context.Background(), sub("j1", "long", ""))
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("err = %v, want ErrNoCapacity", err)
	}

	// Offers that don't fit the job's shape are never tried.
	small := &fakeProvider{name: "small", offers: []Offer{{Market: OnDemand, Region: "us-east", Arch: jobapi.ArchX86_64, MaxVCPU: 4}}}
	arm := &fakeProvider{name: "arm", offers: []Offer{{Market: OnDemand, Region: "us-east", Arch: jobapi.ArchARM64, MaxVCPU: 16}}}
	p = &Pool{Providers: []Provider{small, arm}, Regions: []string{"us-east"}}
	if err := p.Submit(context.Background(), sub("j2", "", "")); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("err = %v, want ErrNoCapacity", err)
	}
	if len(small.attempts)+len(arm.attempts) != 0 {
		t.Fatalf("tried offers that don't fit: %v %v", small.attempts, arm.attempts)
	}
}

func TestLaunchErrorTriesNextOffer(t *testing.T) {
	c := cloud()
	c.failing = map[string]bool{"us-east/on-demand": true}
	p := &Pool{Providers: []Provider{c}, Regions: []string{"us-east", "eu-west"}}
	inst, err := p.Launch(context.Background(), sub("j1", "long", ""))
	if err != nil || inst.Region != "eu-west" {
		t.Fatalf("want eu-west after us-east API error: %+v, %v", inst, err)
	}
}

func TestCanceledContextStops(t *testing.T) {
	c := cloud()
	p := &Pool{Providers: []Provider{c}, Regions: []string{"us-east"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Launch(ctx, sub("j1", "long", "")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(c.attempts) != 0 {
		t.Fatalf("launched after cancel: %v", c.attempts)
	}
}
