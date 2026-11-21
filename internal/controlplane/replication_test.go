package controlplane

import (
	"sort"
	"testing"
	"time"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
)

func (e *env) host(t *testing.T, id, region, domain string) *fakeStream {
	t.Helper()
	return e.connectHello(t, &hostlinkv1.Hello{
		HostId: id, Region: region, FailureDomain: domain, Arch: "x86_64", Vcpu: 16, MemoryGb: 64,
		PeerAddress: "http://" + id + ":7443",
	})
}

func prebuildResult(job, key, id string) *hostlinkv1.LinkRequest {
	return &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobResult{JobResult: &hostlinkv1.JobResult{
		JobId:    job,
		Outcome:  hostlinkv1.JobResult_OUTCOME_SUCCEEDED,
		Prebuild: &hostlinkv1.SnapshotRef{Kind: hostlinkv1.SnapshotRef_KIND_PREBUILD, Repo: "acme/api", Key: key, Id: id},
	}}}
}

// pulls collects the messages sent to the given hosts (they are forwarded
// asynchronously) until want pulls and their grants have arrived, then
// briefly longer to catch extras. It returns the receivers of
// PullSnapshot, checking each against its source's grant.
func pulls(t *testing.T, hosts map[string]*fakeStream, want int) []string {
	t.Helper()
	grants := map[string]*hostlinkv1.ServeSnapshot{}
	var pls []*hostlinkv1.PullSnapshot
	var to []string
	deadline := time.Now().Add(2 * time.Second)
	var settle time.Time
	for {
		got := false
		for id, s := range hosts {
			select {
			case m := <-s.out:
				got = true
				if g := m.GetServeSnapshot(); g != nil {
					grants[g.GetToken()] = g
				}
				if p := m.GetPullSnapshot(); p != nil {
					pls = append(pls, p)
					to = append(to, id)
				}
			default:
			}
		}
		if got {
			continue
		}
		now := time.Now()
		if len(pls) >= want && len(grants) >= want {
			if settle.IsZero() {
				settle = now.Add(50 * time.Millisecond)
			} else if now.After(settle) {
				break
			}
		} else if now.After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	for i, p := range pls {
		g := grants[p.GetToken()]
		if g == nil || g.GetToHostId() != to[i] || g.GetSnapshotId() != p.GetSnapshot().GetId() {
			t.Errorf("pull to %s has no matching grant: %v", to[i], p)
		}
		if p.GetFromAddress() != "http://"+p.GetFromHostId()+":7443" {
			t.Errorf("from_address = %q", p.GetFromAddress())
		}
	}
	if len(grants) != len(pls) {
		t.Errorf("%d grants for %d pulls", len(grants), len(pls))
	}
	sort.Strings(to)
	return to
}

func TestNewPrebuildReplicatedToTwoHostsAcrossFailureDomains(t *testing.T) {
	e := newEnv()
	hs := map[string]*fakeStream{
		"h1": e.host(t, "h1", "us", "rack-a"),
		"h2": e.host(t, "h2", "us", "rack-a"), // same domain as the builder
		"h3": e.host(t, "h3", "us", "rack-b"),
		"h4": e.host(t, "h4", "us", "rack-c"),
		"eu": e.host(t, "eu", "eu", "rack-d"), // outside the placement set
	}
	for _, s := range hs {
		s.push(t, heartbeat(false))
	}
	hs["h1"].push(t, prebuildResult("j1", "pb1-new", "jobs/j1@pb1-new"))

	if got := pulls(t, hs, 2); len(got) != 2 || got[0] != "h3" || got[1] != "h4" {
		t.Fatalf("replicas = %v, want [h3 h4]", got)
	}
}

func TestReplicationCountsExistingCopiesAndSkipsDraining(t *testing.T) {
	e := newEnv()
	hs := map[string]*fakeStream{
		"h1": e.host(t, "h1", "us", "a"),
		"h2": e.host(t, "h2", "us", "b"),
		"h3": e.host(t, "h3", "us", "c"),
		"h4": e.host(t, "h4", "us", "d"),
	}
	hs["h1"].push(t, heartbeat(false))
	hs["h2"].push(t, heartbeat(false, prebuildRef("acme/api", "jobs/j1@pb1-x")))
	hs["h3"].push(t, heartbeat(true)) // draining
	hs["h4"].push(t, heartbeat(false))
	hs["h1"].push(t, prebuildResult("j1", "pb1-x", "jobs/j1@pb1-x"))

	if got := pulls(t, hs, 1); len(got) != 1 || got[0] != "h4" {
		t.Fatalf("replicas = %v, want [h4]", got)
	}
}

func TestReconcileReplicasTopsUpOnce(t *testing.T) {
	e := newEnv()
	e.registerPrebuild()
	snap, _ := e.prebuilds.Latest("acme/api")
	hs := map[string]*fakeStream{
		"h1": e.host(t, "h1", "us", "a"),
		"h2": e.host(t, "h2", "us", "b"),
		"h3": e.host(t, "h3", "us", "c"),
	}
	ref := &hostlinkv1.SnapshotRef{Kind: hostlinkv1.SnapshotRef_KIND_PREBUILD, Repo: "acme/api", Key: snap.Key, Id: "jobs/j0@" + snap.Key}
	stale := &hostlinkv1.SnapshotRef{Kind: hostlinkv1.SnapshotRef_KIND_PREBUILD, Repo: "acme/api", Key: "old", Id: "jobs/j9@old"}
	hs["h1"].push(t, heartbeat(false, ref))
	hs["h2"].push(t, heartbeat(false, stale)) // an old prebuild is not a replica
	hs["h3"].push(t, heartbeat(false))

	e.core.ReconcileReplicas()
	if got := pulls(t, hs, 2); len(got) != 2 || got[0] != "h2" || got[1] != "h3" {
		t.Fatalf("first reconcile = %v", got)
	}
	// Pulls in flight are not repeated.
	e.core.ReconcileReplicas()
	if got := pulls(t, hs, 0); len(got) != 0 {
		t.Fatalf("second reconcile = %v", got)
	}
}

func TestSourceWithoutPeerAddressIsNotUsed(t *testing.T) {
	e := newEnv()
	h1 := e.connectHello(t, &hostlinkv1.Hello{HostId: "h1", Region: "us", FailureDomain: "a", Arch: "x86_64", Vcpu: 16, MemoryGb: 64})
	h2 := e.host(t, "h2", "us", "b")
	h1.push(t, heartbeat(false))
	h2.push(t, heartbeat(false))
	h1.push(t, prebuildResult("j1", "k", "jobs/j1@k"))
	if got := pulls(t, map[string]*fakeStream{"h1": h1, "h2": h2}, 0); len(got) != 0 {
		t.Fatalf("pulls = %v", got)
	}
}

func imageRef(image string) *hostlinkv1.SnapshotRef {
	return &hostlinkv1.SnapshotRef{Kind: hostlinkv1.SnapshotRef_KIND_BASE_IMAGE, Key: image, Id: image}
}

func TestImageRolloutCanariesFirstThenFleetThenLive(t *testing.T) {
	e := newEnv()
	images := &LiveImages{}
	if err := images.Publish("ubuntu-24.04@v1"); err != nil {
		t.Fatal(err)
	}
	const img = "ubuntu-24.04@v2"
	hs := map[string]*fakeStream{
		"seed": e.host(t, "seed", "us", "a"),
		"c1":   e.host(t, "c1", "us", "b"),
		"h1":   e.host(t, "h1", "us", "c"),
		"h2":   e.host(t, "h2", "eu", "d"),
	}
	hs["seed"].push(t, heartbeat(false, imageRef(img)))
	for _, id := range []string{"c1", "h1", "h2"} {
		hs[id].push(t, heartbeat(false, imageRef("ubuntu-24.04@v1")))
	}
	r := &ImageRollout{Image: img, Canaries: []string{"c1"}, Live: images.Publish}

	st, err := e.core.AdvanceImageRollout(r)
	if err != nil || st != StageCanary {
		t.Fatalf("stage %v err %v", st, err)
	}
	if got := pulls(t, hs, 1); len(got) != 1 || got[0] != "c1" {
		t.Fatalf("canary pulls = %v", got)
	}
	// Still pulling: nothing new, still canary, not live.
	if st, _ := e.core.AdvanceImageRollout(r); st != StageCanary {
		t.Fatalf("stage %v", st)
	}
	if got := pulls(t, hs, 0); len(got) != 0 {
		t.Fatalf("repeat pulls = %v", got)
	}
	if v, _ := images.Pin("ubuntu-24.04"); v != "ubuntu-24.04@v1" {
		t.Fatalf("went live early: %s", v)
	}

	hs["c1"].push(t, heartbeat(false, imageRef(img)))
	if st, err := e.core.AdvanceImageRollout(r); err != nil || st != StageFleet {
		t.Fatalf("stage %v err %v", st, err)
	}
	if got := pulls(t, hs, 2); len(got) != 2 || got[0] != "h1" || got[1] != "h2" {
		t.Fatalf("fleet pulls = %v", got)
	}
	if v, _ := images.Pin("ubuntu-24.04"); v != "ubuntu-24.04@v1" {
		t.Fatalf("went live early: %s", v)
	}

	hs["h1"].push(t, heartbeat(false, imageRef(img)))
	hs["h2"].push(t, heartbeat(false, imageRef(img)))
	if st, err := e.core.AdvanceImageRollout(r); err != nil || st != StageLive {
		t.Fatalf("stage %v err %v", st, err)
	}
	if v, _ := images.Pin("ubuntu-24.04"); v != img {
		t.Fatalf("pin = %s", v)
	}
}

func TestImageRolloutNeedsSeedAndCanaries(t *testing.T) {
	e := newEnv()
	h1 := e.host(t, "h1", "us", "a")
	h1.push(t, heartbeat(false))
	r := &ImageRollout{Image: "img@v2"}
	if _, err := e.core.AdvanceImageRollout(r); err == nil {
		t.Fatal("rollout with no seed should fail")
	}
	h1.push(t, heartbeat(false, imageRef("img@v2")))
	r = &ImageRollout{Image: "img@v2", Canaries: []string{"gone"}}
	if st, err := e.core.AdvanceImageRollout(r); err == nil || st != StageCanary {
		t.Fatalf("missing canary: stage %v err %v", st, err)
	}
	if err := (&LiveImages{}).Publish("noversion"); err == nil {
		t.Fatal("Publish accepted an unversioned image")
	}
}
