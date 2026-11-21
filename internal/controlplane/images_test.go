package controlplane

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/images"
)

func signedImage(t *testing.T, priv ed25519.PrivateKey, version string) *images.Signed {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "rootfs.ext4"), []byte("rootfs "+version), 0o644)
	os.WriteFile(filepath.Join(dir, "sbom.spdx.json"), []byte("{}"), 0o644)
	rootfs, _ := images.HashFile(dir, "rootfs.ext4")
	sbom, _ := images.HashFile(dir, "sbom.spdx.json")
	s, err := images.Sign(&images.Manifest{
		Name: "ubuntu-24.04", Version: version, Arch: "x86_64",
		Source: images.Source{
			RunnerImagesRepo: "https://github.com/actions/runner-images", RunnerImagesCommit: strings.Repeat("c", 40),
			APTSnapshot: "20261116T000000Z", PlanSHA256: strings.Repeat("a", 64), BaseRootfsSHA256: strings.Repeat("b", 64),
		},
		SourceDateEpoch: 1, Rootfs: rootfs, SBOM: sbom, Sizes: images.Sizes,
	}, priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func baseImageRef(ref string) *hostlinkv1.SnapshotRef {
	return &hostlinkv1.SnapshotRef{Kind: hostlinkv1.SnapshotRef_KIND_BASE_IMAGE, Key: ref, Id: ref}
}

// A canary host runs jobs on the canary version; its infra errors roll the
// canary back, and the next job on that host is back on live.
func TestImageRolloutThroughHostLink(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ro := &images.Rollout{
		Policy:  images.Policy{CanaryHosts: []string{"canary"}, MinCanaryJobs: 5},
		Trusted: []ed25519.PublicKey{pub},
	}
	e := newEnv()
	e.core.Images = ro

	const v1, v2 = "ubuntu-24.04@2026-11-16", "ubuntu-24.04@2026-11-23"
	if _, err := ro.Publish(signedImage(t, priv, "2026-11-16")); err != nil {
		t.Fatal(err)
	}
	s := e.connect(t, "canary", "us", 16, 64)
	s.push(t, heartbeat(false, baseImageRef(v1)))
	ro.Advance()
	if p, err := ro.Pin("ubuntu-24.04"); err != nil || p != v1 {
		t.Fatalf("pin %s %v", p, err)
	}

	if _, err := ro.Publish(signedImage(t, priv, "2026-11-23")); err != nil {
		t.Fatal(err)
	}
	s.push(t, heartbeat(false, baseImageRef(v1), baseImageRef(v2)))
	if tr := ro.Advance(); len(tr) != 1 || tr[0].To != images.Canary {
		t.Fatalf("%v", tr)
	}

	if err := e.core.Submit(context.Background(), e.submission(t, "j1", 2)); err != nil {
		t.Fatal(err)
	}
	if a := s.next(t).GetAssign(); a.GetBaseImage() != v2 {
		t.Fatalf("canary host job on %s", a.GetBaseImage())
	}
	s.push(t, &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobResult{JobResult: &hostlinkv1.JobResult{
		JobId: "j1", Outcome: hostlinkv1.JobResult_OUTCOME_INFRA_ERROR,
	}}})
	if tr := ro.Advance(); len(tr) != 1 || tr[0].To != images.RolledBack {
		t.Fatalf("%v", tr)
	}

	if err := e.core.Submit(context.Background(), e.submission(t, "j2", 2)); err != nil {
		t.Fatal(err)
	}
	if a := s.next(t).GetAssign(); a.GetBaseImage() != v1 {
		t.Fatalf("after rollback job on %s", a.GetBaseImage())
	}
}

// A host that disconnects and never comes back must not keep a new
// version from going live: the rollout forgets it on disconnect.
func TestImageRolloutForgetsDisconnectedHost(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ro := &images.Rollout{Trusted: []ed25519.PublicKey{pub}}
	e := newEnv()
	e.core.Images = ro

	const v1 = "ubuntu-24.04@2026-11-16"
	if _, err := ro.Publish(signedImage(t, priv, "2026-11-16")); err != nil {
		t.Fatal(err)
	}
	a := e.connect(t, "a", "us", 16, 64)
	gone := e.connect(t, "gone", "us", 16, 64)
	a.push(t, heartbeat(false, baseImageRef(v1)))
	gone.push(t, heartbeat(false)) // has not pulled v1
	if tr := ro.Advance(); len(tr) != 0 {
		t.Fatalf("went live while a host lacks it: %v", tr)
	}

	gone.disconnect()
	if tr := ro.Advance(); len(tr) != 1 || tr[0].To != images.Live {
		t.Fatalf("after disconnect: %v", tr)
	}
	if p, err := ro.Pin("ubuntu-24.04"); err != nil || p != v1 {
		t.Fatalf("pin %s %v", p, err)
	}
}

// When a reconnect replaces a host's stream, the old stream closing must
// not forget the view the new stream reported.
func TestImageRolloutReconnectKeepsHostView(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ro := &images.Rollout{Trusted: []ed25519.PublicKey{pub}}
	e := newEnv()
	e.core.Images = ro

	if _, err := ro.Publish(signedImage(t, priv, "2026-11-16")); err != nil {
		t.Fatal(err)
	}
	a := e.connect(t, "a", "us", 16, 64)
	old := e.connect(t, "h", "us", 16, 64)
	a.push(t, heartbeat(false, baseImageRef("ubuntu-24.04@2026-11-16")))
	cur := e.connect(t, "h", "us", 16, 64) // replaces old
	cur.push(t, heartbeat(false))          // h lacks v1
	old.disconnect()
	if tr := ro.Advance(); len(tr) != 0 {
		t.Fatalf("went live while connected host h lacks it: %v", tr)
	}
}
