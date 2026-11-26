package store

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/decover-tech/tachyon/internal/prebuild"
	"github.com/decover-tech/tachyon/internal/telemetry"
)

// Mem is an in-memory Store. Values are deep-copied in and out, as a
// database would, so callers can't alias stored state. The zero value is
// ready to use.
type Mem struct {
	mu        sync.Mutex
	jobs      map[string]Job
	hosts     map[string]Host
	prebuilds map[string][]prebuild.Snapshot
	configs   map[string]*prebuild.Config
	records   map[string]telemetry.RecordData
	pending   map[pendingKey]PendingRunner
	active    map[activeKey]ActiveRunner
}

type pendingKey struct {
	frontend string
	id       int64
}

type activeKey struct{ frontend, name string }

var _ Store = (*Mem)(nil)

func clone[T any](v T) T {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return out
}

func (m *Mem) init() {
	if m.jobs == nil {
		m.jobs = map[string]Job{}
		m.hosts = map[string]Host{}
		m.prebuilds = map[string][]prebuild.Snapshot{}
		m.configs = map[string]*prebuild.Config{}
		m.records = map[string]telemetry.RecordData{}
		m.pending = map[pendingKey]PendingRunner{}
		m.active = map[activeKey]ActiveRunner{}
	}
}

func (m *Mem) PutJob(_ context.Context, j Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.jobs[j.ID] = clone(j)
	return nil
}

func (m *Mem) SetJobState(_ context.Context, id string, state JobState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		j.State = state
		m.jobs[id] = j
	}
	return nil
}

func (m *Mem) OpenJobs(context.Context) ([]Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Job
	for _, j := range m.jobs {
		if j.State.Open() {
			out = append(out, clone(j))
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

func (m *Mem) PutHost(_ context.Context, h Host) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.hosts[h.Host.ID] = clone(h)
	return nil
}

func (m *Mem) Hosts(context.Context) ([]Host, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Host
	for _, h := range m.hosts {
		out = append(out, clone(h))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Host.ID < out[b].Host.ID })
	return out, nil
}

func (m *Mem) PutPrebuild(_ context.Context, s prebuild.Snapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	list := m.prebuilds[s.Repo]
	for i, old := range list {
		if old.Key == s.Key && old.Commit == s.Commit {
			list[i] = s
			return nil
		}
	}
	m.prebuilds[s.Repo] = append(list, s)
	return nil
}

func (m *Mem) LatestPrebuild(_ context.Context, repo string) (prebuild.Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *prebuild.Snapshot
	for i, s := range m.prebuilds[repo] {
		if !s.Healthy {
			continue
		}
		// Ties go to the later registration.
		if best == nil || !s.CreatedAt.Before(best.CreatedAt) {
			best = &m.prebuilds[repo][i]
		}
	}
	if best == nil {
		return prebuild.Snapshot{}, ErrNotFound
	}
	return *best, nil
}

func (m *Mem) PutPrebuildConfig(_ context.Context, repo string, c *prebuild.Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.configs[repo] = clone(c)
	return nil
}

func (m *Mem) PrebuildConfig(_ context.Context, repo string) (*prebuild.Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.configs[repo]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(c), nil
}

func (m *Mem) PutRecord(_ context.Context, r telemetry.RecordData) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.records[r.JobID] = clone(r)
	return nil
}

func (m *Mem) Record(_ context.Context, jobID string) (telemetry.RecordData, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[jobID]
	if !ok {
		return telemetry.RecordData{}, ErrNotFound
	}
	return clone(r), nil
}

func (m *Mem) RecordsFinishedSince(_ context.Context, since time.Time) ([]telemetry.RecordData, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []telemetry.RecordData
	for _, r := range m.records {
		if finishedSince(r, since) {
			out = append(out, clone(r))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JobID < out[j].JobID })
	return out, nil
}

func (m *Mem) RecordsFor(_ context.Context, jobIDs []string) ([]telemetry.RecordData, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []telemetry.RecordData
	for _, id := range jobIDs {
		if r, ok := m.records[id]; ok {
			out = append(out, clone(r))
		}
	}
	return out, nil
}

func (m *Mem) PutPendingRunner(_ context.Context, p PendingRunner) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.pending[pendingKey{p.Frontend, p.ExternalID}] = clone(p)
	return nil
}

func (m *Mem) DeletePendingRunner(_ context.Context, frontend string, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pending, pendingKey{frontend, id})
	return nil
}

func (m *Mem) PendingRunners(_ context.Context, frontend string) ([]PendingRunner, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []PendingRunner
	for k, p := range m.pending {
		if k.frontend == frontend {
			out = append(out, clone(p))
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ExternalID < out[b].ExternalID })
	return out, nil
}

func (m *Mem) PutActiveRunner(_ context.Context, r ActiveRunner) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.active[activeKey{r.Frontend, r.Name}] = r
	return nil
}

func (m *Mem) DeleteActiveRunner(_ context.Context, frontend, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.active, activeKey{frontend, name})
	return nil
}

func (m *Mem) ActiveRunners(_ context.Context, frontend string) ([]ActiveRunner, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ActiveRunner
	for k, r := range m.active {
		if k.frontend == frontend {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}
