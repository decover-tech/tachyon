package images

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Stage is where a version is in its rollout.
type Stage string

const (
	// Staged: signed and published, being pushed to hosts. No jobs use it.
	Staged Stage = "staged"
	// Canary: every canary host holds it, and jobs placed on canary hosts
	// run on it.
	Canary Stage = "canary"
	// Live: every job pins it.
	Live Stage = "live"
	// Retired: replaced by a newer live version. Kept while jobs use it
	// and as a rollback target.
	Retired Stage = "retired"
	// RolledBack: failed its canary or was rolled back by an operator.
	// Never pinned again.
	RolledBack Stage = "rolled_back"
)

// Policy gates a version's way from staged to live.
type Policy struct {
	// CanaryHosts take a new version first. Required: a version never goes
	// fleet-wide without a canary, except the very first version of an
	// image (there is nothing to canary against).
	CanaryHosts []string
	// MinCanaryJobs successful jobs on the new version before it goes
	// fleet-wide. Default 50.
	MinCanaryJobs int
	// MinSoak is the minimum time in canary. Default 6h.
	MinSoak time.Duration
	// MaxInfraErrorRate rolls the canary back when exceeded (after at least
	// MinCanaryJobs/5 jobs, so one early error doesn't decide it).
	// Default 0.02.
	MaxInfraErrorRate float64
	// KeepRetired is how many retired versions stay available for rollback.
	// Default 2.
	KeepRetired int
}

func (p Policy) withDefaults() Policy {
	if p.MinCanaryJobs == 0 {
		p.MinCanaryJobs = 50
	}
	if p.MinSoak == 0 {
		p.MinSoak = 6 * time.Hour
	}
	if p.MaxInfraErrorRate == 0 {
		p.MaxInfraErrorRate = 0.02
	}
	if p.KeepRetired == 0 {
		p.KeepRetired = 2
	}
	return p
}

// Rollout tracks every image's versions and decides which one a job runs
// on. It implements the control plane's Images interface (Pin) and its
// rollout hooks. Safe for concurrent use.
type Rollout struct {
	Policy  Policy
	Trusted []ed25519.PublicKey
	Now     func() time.Time

	mu      sync.Mutex
	images  map[string]*track
	hosts   map[string]hostView
	running map[string]Ref // job ID -> version it runs on
}

type hostView struct {
	holds    map[string]bool // name@version
	draining bool
}

type track struct {
	live     string
	versions map[string]*version
}

type version struct {
	manifest  *Manifest
	stage     Stage
	published time.Time
	changed   time.Time // when stage last changed
	ok, infra int       // job outcomes while in canary
	reason    string
}

func (r *Rollout) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Rollout) init() {
	if r.images == nil {
		r.images = map[string]*track{}
		r.hosts = map[string]hostView{}
		r.running = map[string]Ref{}
	}
}

func (r *Rollout) track(name string) *track {
	t := r.images[name]
	if t == nil {
		t = &track{versions: map[string]*version{}}
		r.images[name] = t
	}
	return t
}

// Publish verifies a signed manifest and stages its version. A newer
// publish supersedes a version still staged or in canary.
func (r *Rollout) Publish(s *Signed) (*Manifest, error) {
	m, err := s.Verify(r.Trusted)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	t := r.track(m.Name)
	if _, dup := t.versions[m.Version]; dup {
		return nil, fmt.Errorf("%s already published", m.Ref())
	}
	if t.live != "" && !lessVersion(t.live, m.Version) {
		return nil, fmt.Errorf("%s is not newer than live %s", m.Ref(), t.live)
	}
	now := r.now()
	for _, v := range t.versions {
		if v.stage == Staged || v.stage == Canary {
			v.stage, v.changed, v.reason = RolledBack, now, "superseded by "+m.Version
		}
	}
	t.versions[m.Version] = &version{manifest: m, stage: Staged, published: now, changed: now}
	return m, nil
}

// Pin implements controlplane.Images: name -> name@live. A ref that is
// already pinned passes through.
func (r *Rollout) Pin(name string) (string, error) {
	if strings.Contains(name, "@") {
		return name, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	t := r.images[name]
	if t == nil || t.live == "" {
		return "", fmt.Errorf("image %q has no live version", name)
	}
	return name + "@" + t.live, nil
}

// AssignImage is called once a job is placed. On a canary host holding the
// canary version, a job pinned to the live version runs on the canary
// instead. The returned ref is recorded as in use until ObserveResult.
func (r *Rollout) AssignImage(jobID, image, hostID string) string {
	ref, err := ParseRef(image)
	if err != nil {
		return image
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	if t := r.images[ref.Name]; t != nil && ref.Version == t.live && r.isCanaryHost(hostID) {
		if c := t.inStage(Canary); c != "" && r.hosts[hostID].holds[ref.Name+"@"+c] {
			ref.Version = c
		}
	}
	r.running[jobID] = ref
	return ref.String()
}

// ObserveHost records which image versions a host holds, from heartbeats.
func (r *Rollout) ObserveHost(hostID string, images []string, draining bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	h := hostView{holds: map[string]bool{}, draining: draining}
	for _, i := range images {
		h.holds[i] = true
	}
	r.hosts[hostID] = h
}

// ForgetHost drops a host that left the fleet.
func (r *Rollout) ForgetHost(hostID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.hosts, hostID)
}

// ObserveResult records a finished job. Infra errors on a canary version
// count against it.
func (r *Rollout) ObserveResult(jobID string, infraError bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	ref, ok := r.running[jobID]
	if !ok {
		return
	}
	delete(r.running, jobID)
	t := r.images[ref.Name]
	if t == nil {
		return
	}
	if v := t.versions[ref.Version]; v != nil && v.stage == Canary {
		if infraError {
			v.infra++
		} else {
			v.ok++
		}
	}
}

func (r *Rollout) isCanaryHost(id string) bool {
	for _, h := range r.Policy.CanaryHosts {
		if h == id {
			return true
		}
	}
	return false
}

func (t *track) inStage(s Stage) string {
	for ver, v := range t.versions {
		if v.stage == s {
			return ver
		}
	}
	return ""
}

// Transition is one stage change made by Advance.
type Transition struct {
	Ref    Ref
	From   Stage
	To     Stage
	Reason string
}

// Advance moves every image's rollout forward where its gates pass. Call
// it periodically.
func (r *Rollout) Advance() []Transition {
	p := r.Policy.withDefaults()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	now := r.now()
	var out []Transition
	move := func(name, ver string, v *version, to Stage, why string) {
		out = append(out, Transition{Ref: Ref{name, ver}, From: v.stage, To: to, Reason: why})
		v.stage, v.changed, v.reason = to, now, why
	}
	names := make([]string, 0, len(r.images))
	for n := range r.images {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		t := r.images[name]
		if ver := t.inStage(Staged); ver != "" {
			v := t.versions[ver]
			ref := name + "@" + ver
			switch {
			case t.live == "":
				// First version of an image: nothing to canary against.
				if r.allHostsHold(ref) {
					t.live = ver
					move(name, ver, v, Live, "first version; on every host")
				}
			case len(p.CanaryHosts) > 0 && r.canaryHostsHold(ref):
				move(name, ver, v, Canary, "on every canary host")
			}
		}
		if ver := t.inStage(Canary); ver != "" {
			v := t.versions[ver]
			n := v.ok + v.infra
			switch {
			case n >= max(1, p.MinCanaryJobs/5) && float64(v.infra)/float64(n) > p.MaxInfraErrorRate:
				move(name, ver, v, RolledBack, fmt.Sprintf("canary infra errors %d/%d", v.infra, n))
			case v.ok >= p.MinCanaryJobs && now.Sub(v.changed) >= p.MinSoak && r.allHostsHold(name+"@"+ver):
				if old := t.versions[t.live]; old != nil {
					move(name, t.live, old, Retired, "replaced by "+ver)
				}
				t.live = ver
				move(name, ver, v, Live, fmt.Sprintf("canary passed: %d jobs", v.ok))
			}
		}
	}
	return out
}

// canaryHostsHold: every canary host that is reporting holds ref, and at
// least one is reporting.
func (r *Rollout) canaryHostsHold(ref string) bool {
	seen := 0
	for _, id := range r.Policy.CanaryHosts {
		h, ok := r.hosts[id]
		if !ok {
			continue
		}
		seen++
		if !h.holds[ref] {
			return false
		}
	}
	return seen > 0
}

// allHostsHold: the image is pushed to every host before it goes live.
// Draining hosts are exempt.
func (r *Rollout) allHostsHold(ref string) bool {
	active := 0
	for _, h := range r.hosts {
		if h.draining {
			continue
		}
		active++
		if !h.holds[ref] {
			return false
		}
	}
	return active > 0
}

// Rollback makes the newest retired version live again and marks the
// current live version rolled back.
func (r *Rollout) Rollback(name, reason string) (Ref, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	t := r.images[name]
	if t == nil || t.live == "" {
		return Ref{}, fmt.Errorf("image %q has no live version", name)
	}
	prev := ""
	for ver, v := range t.versions {
		if v.stage == Retired && (prev == "" || lessVersion(prev, ver)) {
			prev = ver
		}
	}
	if prev == "" {
		return Ref{}, errors.New("no retired version to roll back to")
	}
	now := r.now()
	cur := t.versions[t.live]
	cur.stage, cur.changed, cur.reason = RolledBack, now, reason
	t.versions[prev].stage, t.versions[prev].changed, t.versions[prev].reason = Live, now, "rollback: "+reason
	t.live = prev
	return Ref{name, prev}, nil
}

// Retained lists the versions of an image hosts must keep: live, staged
// or canary, the newest KeepRetired retired versions (rollback targets),
// and any version a running job uses. Everything else may be deleted.
func (r *Rollout) Retained(name string) []string {
	p := r.Policy.withDefaults()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	t := r.images[name]
	if t == nil {
		return nil
	}
	keep := map[string]bool{}
	var retired []string
	for ver, v := range t.versions {
		switch v.stage {
		case Live, Staged, Canary:
			keep[ver] = true
		case Retired:
			retired = append(retired, ver)
		}
	}
	sort.Slice(retired, func(i, j int) bool { return lessVersion(retired[j], retired[i]) })
	for i, ver := range retired {
		if i < p.KeepRetired {
			keep[ver] = true
		}
	}
	for _, ref := range r.running {
		if ref.Name == name {
			keep[ref.Version] = true
		}
	}
	out := make([]string, 0, len(keep))
	for v := range keep {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return lessVersion(out[i], out[j]) })
	return out
}

// VersionStatus is one version's rollout state.
type VersionStatus struct {
	Version   string
	Stage     Stage
	Reason    string
	CanaryOK  int
	CanaryErr int
}

// Status lists an image's versions, oldest first.
func (r *Rollout) Status(name string) []VersionStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	t := r.images[name]
	if t == nil {
		return nil
	}
	var out []VersionStatus
	for ver, v := range t.versions {
		out = append(out, VersionStatus{Version: ver, Stage: v.stage, Reason: v.reason, CanaryOK: v.ok, CanaryErr: v.infra})
	}
	sort.Slice(out, func(i, j int) bool { return lessVersion(out[i].Version, out[j].Version) })
	return out
}

// Unassign forgets a job that was placed but never dispatched.
func (r *Rollout) Unassign(jobID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, jobID)
}
