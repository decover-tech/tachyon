package scheduler

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Usage is one finished job's measured resource use.
type Usage struct {
	CPU   float64 // average busy vCPUs
	MemGB float64 // peak memory
	// Duration is the job's run time; zero when unknown.
	Duration time.Duration
}

// Profile is the packing estimate for a repo/workflow/job name.
type Profile struct {
	Samples   int
	P95CPU    float64
	P99MemGB  float64
	PeakMemGB float64
	// P95Duration is over samples with a known duration; zero if none.
	P95Duration time.Duration
}

// Profiles keeps a bounded window of usage samples per profile key.
type Profiles struct {
	Window int // samples kept per key; default 50
	// MinSamples before a profile is trusted over the requested size.
	MinSamples int

	mu      sync.Mutex
	samples map[string][]Usage
}

func (p *Profiles) Observe(key string, u Usage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.samples == nil {
		p.samples = map[string][]Usage{}
	}
	w := p.Window
	if w == 0 {
		w = 50
	}
	s := append(p.samples[key], u)
	if len(s) > w {
		s = s[len(s)-w:]
	}
	p.samples[key] = s
}

// Get returns the profile for key, ok=false if there aren't enough samples.
func (p *Profiles) Get(key string) (Profile, bool) {
	p.mu.Lock()
	s := append([]Usage(nil), p.samples[key]...)
	p.mu.Unlock()
	min := p.MinSamples
	if min == 0 {
		min = 5
	}
	if len(s) < min {
		return Profile{Samples: len(s)}, false
	}
	cpu := make([]float64, len(s))
	mem := make([]float64, len(s))
	var dur []float64
	for i, u := range s {
		cpu[i], mem[i] = u.CPU, u.MemGB
		if u.Duration > 0 {
			dur = append(dur, float64(u.Duration))
		}
	}
	prof := Profile{
		Samples:   len(s),
		P95CPU:    percentile(cpu, 0.95),
		P99MemGB:  percentile(mem, 0.99),
		PeakMemGB: percentile(mem, 1),
	}
	if len(dur) > 0 {
		prof.P95Duration = time.Duration(percentile(dur, 0.95))
	}
	return prof, true
}

// percentile uses nearest-rank.
func percentile(v []float64, q float64) float64 {
	sort.Float64s(v)
	i := int(math.Ceil(q*float64(len(v)))) - 1
	if i < 0 {
		i = 0
	}
	return v[i]
}
