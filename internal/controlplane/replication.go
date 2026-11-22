package controlplane

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
)

// Snapshot distribution (architecture: "Image and storage layer").
//
// Hosts move snapshots between themselves; the control plane only brokers
// each transfer. For every pull it mints a single-use token, sends it to the
// source host in a ServeSnapshot (the grant) and to the receiving host in a
// PullSnapshot. The receiver dials the source's PeerSnapshotService with the
// token, and the source refuses any pull it was not granted. Hosts report
// what they hold on every heartbeat, which is how the control plane learns
// a pull finished.

// PrebuildReplicas is how many hosts besides the builder receive each new
// prebuild, chosen from the repo's placement set.
const PrebuildReplicas = 2

// pullTTL bounds a pull grant and how long an unfinished pull blocks a
// retry of the same snapshot to the same host.
const pullTTL = 10 * time.Minute

// replication tracks brokered pulls that have not yet shown up in the
// receiver's heartbeat inventory. The zero value is ready to use.
type replication struct {
	mu       sync.Mutex
	inflight map[string]time.Time // receiver|snapshot id -> expiry
}

// begin reserves a pull; false if one is already in flight.
func (r *replication) begin(key string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight == nil {
		r.inflight = map[string]time.Time{}
	}
	for k, exp := range r.inflight {
		if now.After(exp) {
			delete(r.inflight, k)
		}
	}
	if _, ok := r.inflight[key]; ok {
		return false
	}
	r.inflight[key] = now.Add(pullTTL)
	return true
}

func newPullToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b[:])
}

// brokerPull has target pull ref from source. It returns false when source
// cannot serve pulls or the same pull is already in flight.
func (c *Core) brokerPull(target, source *hostConn, ref *hostlinkv1.SnapshotRef) bool {
	if source.peerAddr == "" {
		c.log().Warn("snapshot source has no peer address", "host", source.id, "snapshot", ref.GetId())
		return false
	}
	now := c.now()
	if !c.repl.begin(target.id+"|"+ref.GetId(), now) {
		return false
	}
	token := newPullToken()
	source.send(&hostlinkv1.LinkResponse{Msg: &hostlinkv1.LinkResponse_ServeSnapshot{ServeSnapshot: &hostlinkv1.ServeSnapshot{
		Token:      token,
		SnapshotId: ref.GetId(),
		ToHostId:   target.id,
		ExpiresAt:  timestamppb.New(now.Add(pullTTL)),
	}}})
	target.send(&hostlinkv1.LinkResponse{Msg: &hostlinkv1.LinkResponse_PullSnapshot{PullSnapshot: &hostlinkv1.PullSnapshot{
		Snapshot:    ref,
		FromHostId:  source.id,
		FromAddress: source.peerAddr,
		Token:       token,
	}}})
	c.log().Info("snapshot pull brokered", "snapshot", ref.GetId(), "from", source.id, "to", target.id)
	return true
}

// holds reports whether hc's last heartbeat listed ref.
func holds(hc *hostConn, ref *hostlinkv1.SnapshotRef) bool {
	for _, r := range hc.snaps {
		if r.GetKind() == ref.GetKind() && r.GetId() == ref.GetId() {
			return true
		}
	}
	return false
}

// placementSet returns the connected hosts that could run src's repo's
// jobs: same region and architecture, not draining. Sorted by id. Caller
// holds c.mu.
func (c *Core) placementSet(src *hostConn) []*hostConn {
	var out []*hostConn
	for _, hc := range c.hosts {
		if hc.region == src.region && hc.arch == src.arch && !hc.draining {
			out = append(out, hc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// replicatePrebuild copies a new prebuild from the host that built it to
// PrebuildReplicas other hosts in the placement set. The builder's
// heartbeat may not list the prebuild yet, so the builder is passed in.
func (c *Core) replicatePrebuild(sourceID string, ref *hostlinkv1.SnapshotRef) {
	c.mu.Lock()
	src := c.hosts[sourceID]
	if src == nil {
		c.mu.Unlock()
		return
	}
	targets := c.replicaTargets(src, ref)
	c.mu.Unlock()
	for _, t := range targets {
		c.brokerPull(t, src, ref)
	}
}

// replicaTargets picks hosts to receive ref so that src plus
// PrebuildReplicas hosts in src's placement set hold it, spreading copies
// across failure domains. Caller holds c.mu.
func (c *Core) replicaTargets(src *hostConn, ref *hostlinkv1.SnapshotRef) []*hostConn {
	usedDomains := map[string]bool{src.failureDomain: true}
	have := 0
	var candidates []*hostConn
	for _, hc := range c.placementSet(src) {
		switch {
		case hc == src:
		case holds(hc, ref):
			have++
			usedDomains[hc.failureDomain] = true
		default:
			candidates = append(candidates, hc)
		}
	}
	need := PrebuildReplicas - have
	var picked []*hostConn
	taken := map[*hostConn]bool{}
	// First pass: hosts in failure domains that hold no copy yet; second
	// pass: anyone left.
	for pass := 0; pass < 2 && len(picked) < need; pass++ {
		for _, hc := range candidates {
			if len(picked) >= need {
				break
			}
			if taken[hc] || (pass == 0 && usedDomains[hc.failureDomain]) {
				continue
			}
			picked = append(picked, hc)
			taken[hc] = true
			usedDomains[hc.failureDomain] = true
		}
	}
	return picked
}

// ReconcileReplicas tops up the replicas of every repo's latest prebuild,
// e.g. after a host holding one is drained or lost. Call it periodically;
// pulls already in flight are not repeated.
func (c *Core) ReconcileReplicas() {
	c.mu.Lock()
	c.init()
	hosts := make([]*hostConn, 0, len(c.hosts))
	for _, hc := range c.hosts {
		hosts = append(hosts, hc)
	}
	c.mu.Unlock()
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].id < hosts[j].id })

	type holder struct {
		hc  *hostConn
		ref *hostlinkv1.SnapshotRef
	}
	latest := map[string]string{} // repo -> latest prebuild key
	first := map[string]holder{}  // repo -> a host that can serve it
	var repos []string
	for _, hc := range hosts {
		c.mu.Lock()
		snaps, canServe := hc.snaps, !hc.draining && hc.peerAddr != ""
		c.mu.Unlock()
		for _, r := range snaps {
			if r.GetKind() != hostlinkv1.SnapshotRef_KIND_PREBUILD {
				continue
			}
			repo := r.GetRepo()
			key, seen := latest[repo]
			if !seen {
				// The registry is consulted outside c.mu.
				if snap, _ := c.Prebuilds.Latest(repo); snap != nil {
					key = snap.Key
				}
				latest[repo] = key
			}
			if !canServe || key == "" || r.GetKey() != key {
				continue
			}
			if _, dup := first[repo]; !dup {
				first[repo] = holder{hc, r}
				repos = append(repos, repo)
			}
		}
	}

	for _, repo := range repos {
		h := first[repo]
		c.mu.Lock()
		var targets []*hostConn
		if c.hosts[h.hc.id] == h.hc {
			targets = c.replicaTargets(h.hc, h.ref)
		}
		c.mu.Unlock()
		for _, t := range targets {
			c.brokerPull(t, h.hc, h.ref)
		}
	}
}
