package scheduler

import (
	"fmt"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

// Restoring scheduler state after a control plane restart. Hosts come back
// from the database before they reconnect, and the jobs still running on
// them come back as allocations, so capacity accounting is right the moment
// a host says Hello again.

// RestoreHost adds a host known from persisted state with its learned
// oversubscription ratio. It is a no-op if the host is already known (it
// reconnected first). The caller should pass Draining: true until the host
// reconnects, so nothing is placed on a host with no stream.
func (s *Scheduler) RestoreHost(h Host, ratio float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.hosts[h.ID]; ok {
		return
	}
	if ratio < MinRatio || ratio > MaxRatio {
		ratio = StartRatio
	}
	s.hosts[h.ID] = &hostState{Host: h, ratio: ratio}
}

// RestoreAllocation re-reserves capacity for a job already running on
// hostID. The host must be known (RestoreHost or UpsertHost first).
func (s *Scheduler) RestoreAllocation(sub jobapi.Submission, hostID string, cpu, memGB float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.hosts[hostID]
	if h == nil {
		return fmt.Errorf("restore %s: unknown host %s", sub.Spec.JobID, hostID)
	}
	if _, dup := s.allocs[sub.Spec.JobID]; dup {
		return nil
	}
	h.cpuUsed += cpu
	h.memUsed += memGB
	s.allocs[sub.Spec.JobID] = allocation{
		hostID: hostID, repo: sub.Spec.Repo, cpu: cpu, memGB: memGB,
		tenant: sub.Spec.Tenant, profile: sub.Profile,
	}
	return nil
}
