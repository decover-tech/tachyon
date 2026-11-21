// Package scheduler places jobs on hosts: on a host that already holds the
// repo's prebuild, packed by measured rather than requested usage, spilling
// to burst capacity only when owned hosts are full.
package scheduler

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

var (
	// ErrNoCapacity means no owned host fits; the caller spills to burst
	// capacity, then to the next region.
	ErrNoCapacity = errors.New("no owned capacity")
	// ErrDeferred means a deferrable job should wait for a quieter fleet.
	ErrDeferred = errors.New("deferred until the fleet is quiet")
	// ErrAlreadyPlaced means the job ID already holds a reservation.
	ErrAlreadyPlaced = errors.New("already placed")
)

// Host is the scheduler's view of one host, refreshed by heartbeats.
type Host struct {
	ID            string
	Region        string
	FailureDomain string // rack / power / switch
	Arch          jobapi.Arch
	VCPU          int
	MemGB         int
	// DedicatedTenant, if set, means only that tenant may run here.
	DedicatedTenant string
	// Snapshots lists prebuild repos present on this host.
	Snapshots map[string]bool
	Draining  bool
}

// Oversubscription ratio policy (decision D6).
const (
	StartRatio = 1.25
	MinRatio   = 1.0
	MaxRatio   = 2.0
	RatioStep  = 0.05
	// MemMarginGB is added to a job's p99 memory when reserving.
	MemMarginGB = 1.0
)

type allocation struct {
	hostID  string
	repo    string
	cpu     float64
	memGB   float64
	tenant  string
	profile string
}

type hostState struct {
	Host
	ratio   float64
	cpuUsed float64
	memUsed float64
}

// Assignment is where a job will run.
type Assignment struct {
	HostID   string
	Locality Locality
	CPU      float64 // reserved vCPUs (measured estimate)
	MemGB    float64 // reserved memory
}

type Locality int

const (
	LocalityNone   Locality = iota // prebuild must be pulled cross-region or absent
	LocalityRegion                 // a peer in this region holds it
	LocalityHost                   // this host holds it
)

func (l Locality) String() string {
	return [...]string{"none", "region", "host"}[l]
}

// Scheduler is safe for concurrent use.
type Scheduler struct {
	Profiles *Profiles
	// DeferBelowUtil admits deferrable jobs only while fleet CPU use is
	// below this fraction of capacity. Default 0.5.
	DeferBelowUtil float64

	mu     sync.Mutex
	hosts  map[string]*hostState
	allocs map[string]allocation // job ID -> allocation
}

func New(profiles *Profiles) *Scheduler {
	if profiles == nil {
		profiles = &Profiles{}
	}
	return &Scheduler{Profiles: profiles, hosts: map[string]*hostState{}, allocs: map[string]allocation{}}
}

// UpsertHost records a heartbeat. New hosts start at StartRatio.
func (s *Scheduler) UpsertHost(h Host) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.hosts[h.ID]; ok {
		st.Host = h
		return
	}
	s.hosts[h.ID] = &hostState{Host: h, ratio: StartRatio}
}

func (s *Scheduler) RemoveHost(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.hosts, id)
	for jid, a := range s.allocs {
		if a.hostID == id {
			delete(s.allocs, jid)
		}
	}
}

// demand estimates what a job will use: its profile if trusted, else its
// requested size. Never more than the VM size.
func (s *Scheduler) demand(sub jobapi.Submission) (cpu, mem float64) {
	hw := sub.Spec.Hardware
	cpu, mem = float64(hw.VCPU), float64(hw.MemoryGB)
	if p, ok := s.Profiles.Get(sub.Profile); ok {
		cpu = math.Min(cpu, math.Max(p.P95CPU, 0.5))
		mem = math.Min(mem, p.P99MemGB+MemMarginGB)
	}
	return cpu, mem
}

// Placed reports whether the job holds a reservation.
func (s *Scheduler) Placed(jobID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.allocs[jobID]
	return ok
}

// Place chooses a host and reserves capacity for the job.
func (s *Scheduler) Place(sub jobapi.Submission) (Assignment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	spec := sub.Spec
	if _, dup := s.allocs[spec.JobID]; dup {
		return Assignment{}, fmt.Errorf("job %s: %w", spec.JobID, ErrAlreadyPlaced)
	}
	if sub.Deferrable {
		threshold := s.DeferBelowUtil
		if threshold == 0 {
			threshold = 0.5
		}
		if s.utilizationLocked() >= threshold {
			return Assignment{}, ErrDeferred
		}
	}
	cpu, mem := s.demand(sub)

	// Regions where some host holds the repo's prebuild.
	prebuildRegions := map[string]bool{}
	for _, h := range s.hosts {
		if h.Snapshots[spec.Prebuild.Repo] {
			prebuildRegions[h.Region] = true
		}
	}
	// Same-repo jobs per failure domain, for spread.
	repoInDomain := map[string]int{}
	for _, a := range s.allocs {
		if a.repo == spec.Repo {
			repoInDomain[s.hosts[a.hostID].FailureDomain]++
		}
	}

	type cand struct {
		h     *hostState
		loc   Locality
		score float64
	}
	var cands []cand
	for _, h := range s.hosts {
		// Hard constraints.
		if h.Draining || h.Arch != spec.Hardware.Arch {
			continue
		}
		if sub.Region != "" && h.Region != sub.Region {
			continue
		}
		if h.DedicatedTenant != "" && h.DedicatedTenant != spec.Tenant {
			continue
		}
		if spec.Hardware.VCPU > h.VCPU || spec.Hardware.MemoryGB > h.MemGB {
			continue
		}
		cpuCap := float64(h.VCPU) * h.ratio
		if h.cpuUsed+cpu > cpuCap || h.memUsed+mem > float64(h.MemGB) {
			continue // memory is never oversubscribed
		}
		loc := LocalityNone
		if spec.Prebuild.Policy != jobapi.PrebuildNone {
			if h.Snapshots[spec.Prebuild.Repo] {
				loc = LocalityHost
			} else if prebuildRegions[h.Region] {
				loc = LocalityRegion
			}
		}
		headroom := (cpuCap - h.cpuUsed - cpu) / cpuCap
		score := 100*float64(loc) + 20*headroom - 5*float64(repoInDomain[h.FailureDomain])
		cands = append(cands, cand{h, loc, score})
	}
	if len(cands) == 0 {
		return Assignment{}, ErrNoCapacity
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].h.ID < cands[j].h.ID
	})
	best := cands[0]
	best.h.cpuUsed += cpu
	best.h.memUsed += mem
	s.allocs[spec.JobID] = allocation{hostID: best.h.ID, repo: spec.Repo, cpu: cpu, memGB: mem, tenant: spec.Tenant, profile: sub.Profile}
	return Assignment{HostID: best.h.ID, Locality: best.loc, CPU: cpu, MemGB: mem}, nil
}

// Release frees a job's reservation and, if usage is given, feeds its
// profile.
func (s *Scheduler) Release(jobID string, usage *Usage) {
	s.mu.Lock()
	a, ok := s.allocs[jobID]
	if ok {
		delete(s.allocs, jobID)
		if h := s.hosts[a.hostID]; h != nil {
			h.cpuUsed = math.Max(0, h.cpuUsed-a.cpu)
			h.memUsed = math.Max(0, h.memUsed-a.memGB)
		}
	}
	s.mu.Unlock()
	if ok && usage != nil {
		s.Profiles.Observe(a.profile, *usage)
	}
}

// ReportSlowdown adjusts a host's oversubscription ratio. slowdown is the
// median ratio of recent job wall time to each job's own history (1.0 = no
// change). Above 1.10 the ratio steps back; it rises only with sustained
// evidence of no slowdown.
func (s *Scheduler) ReportSlowdown(hostID string, slowdown float64, sustained bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.hosts[hostID]
	if h == nil {
		return
	}
	switch {
	case slowdown > 1.10:
		h.ratio = math.Max(MinRatio, h.ratio-2*RatioStep)
	case sustained && slowdown < 1.03:
		h.ratio = math.Min(MaxRatio, h.ratio+RatioStep)
	}
}

// Ratio returns a host's current oversubscription ratio.
func (s *Scheduler) Ratio(hostID string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.hosts[hostID]; h != nil {
		return h.ratio
	}
	return 0
}

// Utilization is reserved CPU over physical vCPUs across the fleet.
func (s *Scheduler) Utilization() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.utilizationLocked()
}

func (s *Scheduler) utilizationLocked() float64 {
	var used, total float64
	for _, h := range s.hosts {
		if h.Draining {
			continue
		}
		used += h.cpuUsed
		total += float64(h.VCPU)
	}
	if total == 0 {
		return 1
	}
	return used / total
}
