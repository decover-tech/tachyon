package prebuild

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

// Source reads a repo from its platform (GitHub first). tenant and repo are
// as in jobapi.Spec, with the repo in short form (owner/name).
type Source interface {
	// DefaultHead returns the head commit of the repo's default branch.
	DefaultHead(ctx context.Context, tenant, repo string) (string, error)
	// Protected reports whether branch is a protected branch.
	Protected(ctx context.Context, tenant, repo, branch string) (bool, error)
	// Files lists every file path in the tree at commit, slash-separated.
	Files(ctx context.Context, tenant, repo, commit string) ([]string, error)
	// ReadFile returns one file at commit, or an error wrapping
	// fs.ErrNotExist if it is absent.
	ReadFile(ctx context.Context, tenant, repo, commit, path string) ([]byte, error)
	// CloneToken mints a short-lived token that can only read this repo.
	CloneToken(ctx context.Context, tenant, repo string) (CloneToken, error)
}

// CloneToken is a short-lived, read-only credential for one repo.
type CloneToken struct {
	URL       string // https clone URL without credentials
	Token     []byte // secret
	ExpiresAt time.Time
}

// ImagePinner pins an image name to its live version (name@version).
type ImagePinner interface {
	Pin(name string) (string, error)
}

// Registry is where prebuilds are registered.
type Registry interface {
	Latest(repo string) (*Snapshot, *Config)
	// SetConfig records the config the repo's latest prebuild was built from.
	SetConfig(repo string, c *Config)
}

// Builder runs a prebuild build on a host.
type Builder interface {
	// BuildPrebuild places and dispatches the build as job req.JobID. The
	// result comes back through Manager.Finished.
	BuildPrebuild(ctx context.Context, req BuildRequest) error
}

// Trigger says why a prebuild was built.
type Trigger string

const (
	TriggerPush    Trigger = "push"
	TriggerNightly Trigger = "nightly"
)

// BuildRequest is one prebuild build. It never carries repo secrets: the
// only credential is the clone token, which is for the clone commands alone.
type BuildRequest struct {
	JobID     string
	Tenant    string
	Repo      string // owner/name
	Commit    string
	BaseImage string // name@version
	Key       string
	Trigger   Trigger
	Hardware  jobapi.Hardware
	// Refresh: the key matches From, so the build forks it and only fetches
	// new git objects. A builder that can't fork From (it isn't on the
	// assigned host) runs WarmCommands on an empty disk instead.
	Refresh bool
	From    *Snapshot

	CloneCommands   []string
	CloneToken      []byte // secret
	WarmCommands    []string
	KeepPaths       []string
	ScrubPaths      []string
	EgressAllowlist []string
}

// String omits the clone token.
func (r BuildRequest) String() string {
	return fmt.Sprintf("prebuild %s@%s key=%s refresh=%v trigger=%s", r.Repo, r.Commit, r.Key, r.Refresh, r.Trigger)
}

// Manager decides when each repo's prebuild is built: on pushes to the
// default branch and other protected branches, at most once per MinInterval
// per repo, plus a nightly refresh. Pushes that arrive inside the interval
// or while a build runs are coalesced, and the newest commit is built once
// the interval has passed. Call Tick periodically.
type Manager struct {
	Source   Source
	Images   ImagePinner
	Registry Registry
	Builder  Builder
	Log      *slog.Logger
	Now      func() time.Time

	// Image is the base image name prebuilds are built on. Default ubuntu-24.04.
	Image string
	// Hardware for prebuild VMs. Default 4 vCPU x86_64.
	Hardware jobapi.Hardware
	// MinInterval between push-triggered builds of one repo. Default 10m.
	MinInterval time.Duration
	// NightlyHour (UTC, 0-23) after which each repo gets its daily refresh.
	NightlyHour int
	// BuildTimeout after which a build with no result is presumed lost.
	// Default 7h (the 6h job deadline plus slack).
	BuildTimeout time.Duration
	// EgressAllowlist applies while warm steps run (mirrors, the git host).
	EgressAllowlist []string

	mu    sync.Mutex
	repos map[string]*repoState
	seq   uint64
}

type repoState struct {
	tenant      string
	lastBuild   time.Time // last push-triggered build start (rate limit)
	lastNightly time.Time
	inflight    string // job ID of the running build; reserved before dispatch
	inflightAt  time.Time
	inflightCfg *Config
	pending     *pending // newest push waiting for the interval
}

type pending struct {
	commit  string
	trigger Trigger
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) log() *slog.Logger {
	if m.Log != nil {
		return m.Log
	}
	return slog.Default()
}

func (m *Manager) minInterval() time.Duration {
	if m.MinInterval > 0 {
		return m.MinInterval
	}
	return 10 * time.Minute
}

func (m *Manager) buildTimeout() time.Duration {
	if m.BuildTimeout > 0 {
		return m.BuildTimeout
	}
	return 7 * time.Hour
}

func (m *Manager) state(tenant, repo string) *repoState {
	if m.repos == nil {
		m.repos = map[string]*repoState{}
	}
	st := m.repos[repo]
	if st == nil {
		// The first nightly refresh comes on the next day's NightlyHour.
		st = &repoState{lastNightly: m.now()}
		m.repos[repo] = st
	}
	if tenant != "" {
		st.tenant = tenant
	}
	return st
}

// ShortRepo strips a host prefix: github.com/acme/api -> acme/api.
func ShortRepo(repo string) string {
	if i := strings.Index(repo, "/"); i >= 0 && strings.Contains(repo[:i], ".") {
		return repo[i+1:]
	}
	return repo
}

// OnPush handles a push hint. Only pushes to the default branch (trusted)
// or another protected branch of the same repo build prebuilds; pull
// requests and forks never do. Errors talking to the platform are logged and
// the build is retried on a later Tick, so a webhook is never failed here.
func (m *Manager) OnPush(ctx context.Context, h jobapi.Hint) error {
	if h.Event != "push" || h.Trust == jobapi.UntrustedFork || !ValidCommit(h.Commit) {
		return nil
	}
	branch, ok := strings.CutPrefix(h.Ref, "refs/heads/")
	if !ok {
		return nil
	}
	repo := ShortRepo(h.Repo)
	if h.Trust != jobapi.Trusted {
		prot, err := m.Source.Protected(ctx, h.Tenant, repo, branch)
		if err != nil {
			return fmt.Errorf("prebuild: branch protection for %s:%s: %w", repo, branch, err)
		}
		if !prot {
			return nil
		}
	}

	m.mu.Lock()
	st := m.state(h.Tenant, repo)
	now := m.now()
	if st.inflight != "" || !ShouldBuild(jobapi.Trusted, st.lastBuild, now, m.minInterval()) {
		st.pending = &pending{commit: h.Commit, trigger: TriggerPush}
		m.mu.Unlock()
		return nil
	}
	st.lastBuild = now
	jobID := m.reserve(repo, st, now)
	m.mu.Unlock()
	m.start(ctx, repo, jobID, h.Commit, TriggerPush)
	return nil
}

// Tick starts coalesced push builds whose interval has passed and the
// nightly refresh of every known repo. Call it periodically.
func (m *Manager) Tick(ctx context.Context) {
	type job struct {
		repo, jobID, commit string
		trigger             Trigger
	}
	var jobs []job
	m.mu.Lock()
	now := m.now()
	names := make([]string, 0, len(m.repos))
	for r := range m.repos {
		names = append(names, r)
	}
	sort.Strings(names)
	for _, repo := range names {
		st := m.repos[repo]
		if st.inflight != "" {
			if now.Sub(st.inflightAt) < m.buildTimeout() {
				continue
			}
			m.log().Warn("prebuild build lost; giving up on it", "repo", repo, "job", st.inflight)
			st.inflight, st.inflightCfg = "", nil
		}
		switch {
		case m.nightlyDue(st, now):
			// Nightly bypasses the interval and supersedes a pending push:
			// it builds the default branch's head.
			st.lastNightly = now
			st.pending = nil
			jobs = append(jobs, job{repo: repo, jobID: m.reserve(repo, st, now), trigger: TriggerNightly})
		case st.pending != nil && ShouldBuild(jobapi.Trusted, st.lastBuild, now, m.minInterval()):
			jobs = append(jobs, job{repo: repo, jobID: m.reserve(repo, st, now), commit: st.pending.commit, trigger: st.pending.trigger})
			st.pending = nil
			st.lastBuild = now
		}
	}
	m.mu.Unlock()
	for _, j := range jobs {
		m.start(ctx, j.repo, j.jobID, j.commit, j.trigger)
	}
}

// reserve marks a build of repo in flight under a new job ID, so no second
// build starts while this one is planned and dispatched. m.mu is held.
func (m *Manager) reserve(repo string, st *repoState, now time.Time) string {
	m.seq++
	st.inflight = fmt.Sprintf("pb_%s_%d_%d", sanitizeID(repo), now.Unix(), m.seq)
	st.inflightAt = now
	st.inflightCfg = nil
	return st.inflight
}

func sanitizeID(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '_'
	}, s)
}

func (m *Manager) nightlyDue(st *repoState, now time.Time) bool {
	n, last := now.UTC(), st.lastNightly.UTC()
	if n.Hour() < m.NightlyHour {
		return false
	}
	ny, nm, nd := n.Date()
	ly, lm, ld := last.Date()
	return ny != ly || nm != lm || nd != ld
}

// start plans and dispatches one build. On failure the build is queued
// again as pending so a later Tick retries it.
func (m *Manager) start(ctx context.Context, repo, jobID, commit string, trigger Trigger) {
	m.mu.Lock()
	st := m.state("", repo)
	tenant := st.tenant
	m.mu.Unlock()

	req, cfg, err := m.plan(ctx, tenant, repo, commit, trigger)
	switch {
	case errors.Is(err, errNothingToDo):
		m.log().Info("prebuild up to date", "repo", repo, "commit", req.Commit)
		m.release(repo, jobID, "", "")
		return
	case errors.Is(err, errNoConfig):
		m.log().Info("repo has no usable prebuild config", "repo", repo, "commit", req.Commit)
		m.release(repo, jobID, "", "")
		return
	case err != nil:
		m.log().Warn("prebuild plan failed; will retry", "repo", repo, "commit", commit, "err", err)
		m.release(repo, jobID, commit, trigger)
		return
	}
	req.JobID = jobID
	m.mu.Lock()
	if st.inflight == jobID {
		st.inflightCfg = cfg
	}
	m.mu.Unlock()
	if err := m.Builder.BuildPrebuild(ctx, req); err != nil {
		m.log().Warn("prebuild dispatch failed; will retry", "repo", repo, "commit", req.Commit, "err", err)
		m.release(repo, jobID, req.Commit, trigger)
		return
	}
	m.log().Info("prebuild started", "repo", repo, "commit", req.Commit, "key", req.Key, "refresh", req.Refresh, "trigger", trigger, "job", jobID)
}

// release clears a reservation that never became a build. A non-empty
// trigger queues the commit for retry on a later Tick, unless a newer push
// is already pending.
func (m *Manager) release(repo, jobID, commit string, trigger Trigger) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.state("", repo)
	if st.inflight == jobID {
		st.inflight, st.inflightCfg = "", nil
	}
	if trigger != "" && st.pending == nil {
		st.pending = &pending{commit: commit, trigger: trigger}
	}
}

var (
	errNothingToDo = errors.New("prebuild already at this commit and key")
	errNoConfig    = errors.New("no prebuild config")
)

// plan reads the config and key files at commit and decides between a
// refresh (key unchanged: fetch git objects only) and a full warm-up.
func (m *Manager) plan(ctx context.Context, tenant, repo, commit string, trigger Trigger) (BuildRequest, *Config, error) {
	if commit == "" {
		head, err := m.Source.DefaultHead(ctx, tenant, repo)
		if err != nil {
			return BuildRequest{}, nil, fmt.Errorf("default head: %w", err)
		}
		commit = head
	}
	if !ValidCommit(commit) {
		return BuildRequest{}, nil, fmt.Errorf("invalid commit %q", commit)
	}
	raw, err := m.Source.ReadFile(ctx, tenant, repo, commit, ConfigPath)
	if errors.Is(err, fs.ErrNotExist) {
		return BuildRequest{Commit: commit}, nil, errNoConfig
	}
	if err != nil {
		return BuildRequest{}, nil, err
	}
	cfg, err := Parse(raw)
	if err != nil {
		// The customer's config error; retrying the same commit won't help.
		m.log().Warn("invalid prebuild config", "repo", repo, "commit", commit, "err", err)
		return BuildRequest{Commit: commit}, nil, errNoConfig
	}
	files, err := m.Source.Files(ctx, tenant, repo, commit)
	if err != nil {
		return BuildRequest{}, nil, fmt.Errorf("list files: %w", err)
	}
	keyFiles, err := KeyFilesIn(cfg, files, func(rel string) ([]byte, error) {
		return m.Source.ReadFile(ctx, tenant, repo, commit, rel)
	})
	if err != nil {
		return BuildRequest{}, nil, err
	}
	image := m.Image
	if image == "" {
		image = "ubuntu-24.04"
	}
	base, err := m.Images.Pin(image)
	if err != nil {
		return BuildRequest{}, nil, fmt.Errorf("pin image: %w", err)
	}
	key := Key(base, raw, keyFiles)

	latest, _ := m.Registry.Latest(repo)
	refresh := latest != nil && latest.Healthy && latest.Key == key && latest.BaseImage == base
	if refresh && latest.Commit == commit {
		return BuildRequest{Commit: commit}, nil, errNothingToDo
	}

	tok, err := m.Source.CloneToken(ctx, tenant, repo)
	if err != nil {
		return BuildRequest{}, nil, fmt.Errorf("clone token: %w", err)
	}
	clone, err := CloneCommands(tok.URL, commit)
	if err != nil {
		return BuildRequest{}, nil, err
	}
	hw := m.Hardware
	if hw.VCPU == 0 {
		hw = jobapi.NewHardware(jobapi.ArchX86_64, 4)
	}
	req := BuildRequest{
		Tenant:          tenant,
		Repo:            repo,
		Commit:          commit,
		BaseImage:       base,
		Key:             key,
		Trigger:         trigger,
		Hardware:        hw,
		Refresh:         refresh,
		CloneCommands:   clone,
		CloneToken:      tok.Token,
		KeepPaths:       append([]string(nil), cfg.Paths...),
		ScrubPaths:      append([]string(nil), CredentialFiles...),
		EgressAllowlist: append([]string(nil), m.EgressAllowlist...),
	}
	if refresh {
		req.From = latest
	}
	// Warm commands are always sent so a builder that can't fork From can
	// fall back to a full warm-up; Refresh builds skip them.
	for _, s := range cfg.Warm {
		req.WarmCommands = append(req.WarmCommands, s.Run)
	}
	return req, cfg, nil
}

// Finished records a build's result. On success the config it was built
// from becomes the repo's prebuild config (the snapshot itself is
// registered by the control plane from the host's report). The next build
// waits for Tick, so a coalesced push sees the newly registered snapshot.
func (m *Manager) Finished(repo, jobID string, ok bool) {
	m.mu.Lock()
	st := m.repos[repo]
	if st == nil || st.inflight != jobID {
		m.mu.Unlock()
		return
	}
	cfg := st.inflightCfg
	st.inflight, st.inflightCfg = "", nil
	m.mu.Unlock()
	if ok {
		if cfg != nil {
			m.Registry.SetConfig(repo, cfg)
		}
		m.log().Info("prebuild finished", "repo", repo, "job", jobID)
		return
	}
	m.log().Warn("prebuild failed", "repo", repo, "job", jobID)
}

// Inflight returns the job ID of repo's running build, if any.
func (m *Manager) Inflight(repo string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.repos[repo]; st != nil {
		return st.inflight
	}
	return ""
}
