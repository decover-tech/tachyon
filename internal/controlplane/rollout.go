package controlplane

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
)

// RolloutStage is how far a base image rollout has got.
type RolloutStage int

const (
	// StageCanary: pushing to the canary hosts.
	StageCanary RolloutStage = iota
	// StageFleet: canaries hold the image; pushing to every other host.
	StageFleet
	// StageLive: every connected host holds the image and it is live.
	StageLive
)

func (s RolloutStage) String() string {
	return [...]string{"canary", "fleet", "live"}[s]
}

// ImageRollout pushes a new base image version to every host before it
// goes live, canary hosts first. Some host (the seed) must already hold the
// image and report it as a KIND_BASE_IMAGE snapshot whose id and key are
// the pinned image (name@version); every other host pulls it from a peer
// that holds it. Drive it with Core.AdvanceImageRollout until StageLive.
type ImageRollout struct {
	Image    string   // name@version
	Arch     string   // only hosts of this arch; empty = all
	Canaries []string // host ids that get the image first
	// Live is called once every connected host holds the image, e.g.
	// LiveImages.Publish. Until then jobs keep pinning the old version.
	Live func(image string) error

	mu    sync.Mutex
	stage RolloutStage
	next  int // round-robin over sources
}

// Stage returns the rollout's current stage.
func (r *ImageRollout) Stage() RolloutStage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stage
}

// AdvanceImageRollout brokers pulls for hosts in the current stage that
// lack the image and moves to the next stage once they all report it.
// Call it periodically (e.g. on a ticker); it is idempotent.
func (c *Core) AdvanceImageRollout(r *ImageRollout) (RolloutStage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stage == StageLive {
		return r.stage, nil
	}
	ref := &hostlinkv1.SnapshotRef{Kind: hostlinkv1.SnapshotRef_KIND_BASE_IMAGE, Key: r.Image, Id: r.Image}

	c.mu.Lock()
	c.init()
	var sources, fleet []*hostConn
	for _, hc := range c.hosts {
		if r.Arch != "" && hc.arch != r.Arch {
			continue
		}
		fleet = append(fleet, hc)
		if holds(hc, ref) && hc.peerAddr != "" {
			sources = append(sources, hc)
		}
	}
	canaries := make([]*hostConn, 0, len(r.Canaries))
	missingCanary := false
	for _, id := range r.Canaries {
		if hc := c.hosts[id]; hc != nil {
			canaries = append(canaries, hc)
		} else {
			missingCanary = true
		}
	}
	lacking := func(hosts []*hostConn) []*hostConn {
		var out []*hostConn
		for _, hc := range hosts {
			if !holds(hc, ref) {
				out = append(out, hc)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
		return out
	}
	byID := func(s []*hostConn) {
		sort.Slice(s, func(i, j int) bool { return s[i].id < s[j].id })
	}
	byID(sources)
	var todo []*hostConn
	if r.stage == StageCanary {
		todo = lacking(canaries)
		if len(todo) == 0 && !missingCanary {
			r.stage = StageFleet
		}
	}
	if r.stage == StageFleet {
		todo = lacking(fleet)
	}
	c.mu.Unlock()

	if r.stage == StageCanary && missingCanary && len(todo) == 0 {
		return r.stage, fmt.Errorf("rollout %s: waiting for canary hosts to connect", r.Image)
	}
	if r.stage == StageFleet && len(todo) == 0 {
		if r.Live != nil {
			if err := r.Live(r.Image); err != nil {
				return r.stage, fmt.Errorf("rollout %s: go live: %w", r.Image, err)
			}
		}
		r.stage = StageLive
		c.log().Info("base image live", "image", r.Image, "hosts", len(fleet))
		return r.stage, nil
	}
	if len(sources) == 0 {
		return r.stage, fmt.Errorf("rollout %s: no connected host holds the image to seed from", r.Image)
	}
	for _, t := range todo {
		src := sources[r.next%len(sources)]
		r.next++
		c.brokerPull(t, src, ref)
	}
	return r.stage, nil
}

// LiveImages is an Images whose versions change when a rollout goes live.
type LiveImages struct {
	mu sync.Mutex
	m  map[string]string // name -> live version
}

// Pin implements Images.
func (l *LiveImages) Pin(name string) (string, error) {
	if strings.Contains(name, "@") {
		return name, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	v, ok := l.m[name]
	if !ok {
		return "", fmt.Errorf("unknown image %q", name)
	}
	return name + "@" + v, nil
}

// Publish makes image (name@version) the live version of its name.
func (l *LiveImages) Publish(image string) error {
	name, version, ok := strings.Cut(image, "@")
	if !ok || name == "" || version == "" {
		return errors.New("image must be name@version")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]string{}
	}
	l.m[name] = version
	return nil
}
