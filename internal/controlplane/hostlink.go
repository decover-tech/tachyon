package controlplane

import (
	"errors"
	"fmt"
	"io"
	"time"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/jobapi"
	"github.com/decover-tech/tachyon/internal/prebuild"
	"github.com/decover-tech/tachyon/internal/scheduler"
	"github.com/decover-tech/tachyon/internal/telemetry"
)

// hostConn is one connected hostd stream.
type hostConn struct {
	id     string
	region string
	out    chan *hostlinkv1.LinkResponse
	snaps  []*hostlinkv1.SnapshotRef
	closed chan struct{}
}

func (h *hostConn) send(m *hostlinkv1.LinkResponse) {
	select {
	case h.out <- m:
	case <-h.closed:
	}
}

// HostLink serves hostlink.v1.HostLinkService on top of a Core.
type HostLink struct {
	hostlinkv1.UnimplementedHostLinkServiceServer
	Core *Core
}

// Link handles one hostd stream: Hello first, then heartbeats, job
// events and results; assignments flow back on the same stream.
func (s *HostLink) Link(stream hostlinkv1.HostLinkService_LinkServer) error {
	c := s.Core
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil || hello.GetHostId() == "" {
		return errors.New("first message must be Hello with host_id")
	}
	// TODO: authenticate the host from its mTLS client certificate and
	// check it matches hello.host_id.
	hc := &hostConn{
		id:     hello.GetHostId(),
		region: hello.GetRegion(),
		out:    make(chan *hostlinkv1.LinkResponse, 64),
		closed: make(chan struct{}),
	}
	c.mu.Lock()
	c.init()
	if old := c.hosts[hc.id]; old != nil {
		close(old.closed)
	}
	c.hosts[hc.id] = hc
	c.mu.Unlock()
	c.log().Info("host connected", "host", hc.id, "region", hc.region)

	c.Scheduler.UpsertHost(scheduler.Host{
		ID:            hc.id,
		Region:        hello.GetRegion(),
		FailureDomain: hello.GetFailureDomain(),
		Arch:          jobapi.Arch(hello.GetArch()),
		VCPU:          int(hello.GetVcpu()),
		MemGB:         int(hello.GetMemoryGb()),
	})

	sendErr := make(chan error, 1)
	go func() {
		for {
			select {
			case m := <-hc.out:
				if err := stream.Send(m); err != nil {
					sendErr <- err
					return
				}
			case <-hc.closed:
				sendErr <- nil
				return
			case <-stream.Context().Done():
				sendErr <- stream.Context().Err()
				return
			}
		}
	}()

	defer func() {
		c.mu.Lock()
		if c.hosts[hc.id] == hc {
			delete(c.hosts, hc.id)
			close(hc.closed)
		}
		c.mu.Unlock()
		// Running jobs keep running on the host; capacity stays reserved
		// until the host reconnects or is declared dead by missed heartbeats.
	}()

	for {
		m, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case err := <-sendErr:
			return err
		default:
		}
		switch msg := m.GetMsg().(type) {
		case *hostlinkv1.LinkRequest_Heartbeat:
			s.onHeartbeat(hc, hello, msg.Heartbeat)
		case *hostlinkv1.LinkRequest_JobEvent:
			s.onJobEvent(msg.JobEvent)
		case *hostlinkv1.LinkRequest_JobResult:
			s.onJobResult(hc, msg.JobResult)
		case *hostlinkv1.LinkRequest_Hello:
			return fmt.Errorf("host %s: duplicate Hello", hc.id)
		}
		if seq := m.GetSeq(); seq > 0 {
			// The host keeps the message until acked and may resend it
			// after a reconnect; onJobEvent and onJobResult are idempotent.
			hc.send(&hostlinkv1.LinkResponse{Msg: &hostlinkv1.LinkResponse_Ack{Ack: &hostlinkv1.Ack{Seq: seq}}})
		}
	}
}

func (s *HostLink) onHeartbeat(hc *hostConn, hello *hostlinkv1.Hello, hb *hostlinkv1.Heartbeat) {
	c := s.Core
	repos := map[string]bool{}
	for _, ref := range hb.GetSnapshots() {
		if ref.GetKind() == hostlinkv1.SnapshotRef_KIND_PREBUILD {
			repos[ref.GetRepo()] = true
		}
	}
	c.mu.Lock()
	hc.snaps = hb.GetSnapshots()
	c.mu.Unlock()
	c.Scheduler.UpsertHost(scheduler.Host{
		ID:            hc.id,
		Region:        hello.GetRegion(),
		FailureDomain: hello.GetFailureDomain(),
		Arch:          jobapi.Arch(hello.GetArch()),
		VCPU:          int(hello.GetVcpu()),
		MemGB:         int(hello.GetMemoryGb()),
		Snapshots:     repos,
		Draining:      hb.GetDraining(),
	})
}

var phaseMap = map[hostlinkv1.Phase]telemetry.Phase{
	hostlinkv1.Phase_PHASE_HOST_ASSIGNED: telemetry.HostAssigned,
	hostlinkv1.Phase_PHASE_VM_RESUMED:    telemetry.VMResumed,
	hostlinkv1.Phase_PHASE_RUNNER_ONLINE: telemetry.RunnerOnline,
	hostlinkv1.Phase_PHASE_JOB_STARTED:   telemetry.JobStarted,
	hostlinkv1.Phase_PHASE_JOB_FINISHED:  telemetry.JobFinished,
	hostlinkv1.Phase_PHASE_VM_DESTROYED:  telemetry.VMDestroyed,
}

func (s *HostLink) onJobEvent(ev *hostlinkv1.JobEvent) {
	rec := s.Core.Record(ev.GetJobId())
	p, ok := phaseMap[ev.GetPhase()]
	if rec == nil || !ok {
		return
	}
	at := time.Now()
	if ev.GetAt() != nil {
		at = ev.GetAt().AsTime()
	}
	rec.Mark(p, at)
}

func (s *HostLink) onJobResult(hc *hostConn, r *hostlinkv1.JobResult) {
	c := s.Core
	var usage *scheduler.Usage
	if r.GetOutcome() == hostlinkv1.JobResult_OUTCOME_SUCCEEDED || r.GetOutcome() == hostlinkv1.JobResult_OUTCOME_FAILED {
		usage = &scheduler.Usage{CPU: r.GetAvgBusyVcpu(), MemGB: r.GetPeakMemoryGb()}
	}
	c.Scheduler.Release(r.GetJobId(), usage)
	if pb := r.GetPrebuild(); pb != nil && pb.GetKey() != "" {
		c.Prebuilds.Register(prebuild.Snapshot{
			Repo:      pb.GetRepo(),
			Key:       pb.GetKey(),
			Commit:    r.GetCommit(),
			BaseImage: r.GetBaseImage(),
			CreatedAt: time.Now(),
			Healthy:   true,
		})
	}
	c.log().Info("job finished", "job", r.GetJobId(), "host", hc.id, "outcome", r.GetOutcome().String())
}

// hostSnapshot finds the prebuild snapshot for repo on a host.
func (c *Core) hostSnapshot(hostID, repo string) (*hostlinkv1.SnapshotRef, bool) {
	return c.findSnap(hostID, hostlinkv1.SnapshotRef_KIND_PREBUILD, repo, "")
}

// hostSticky finds a sticky volume head for repo/key on a host.
func (c *Core) hostSticky(hostID, repo, key string) (*hostlinkv1.SnapshotRef, bool) {
	return c.findSnap(hostID, hostlinkv1.SnapshotRef_KIND_STICKY, repo, key)
}

func (c *Core) findSnap(hostID string, kind hostlinkv1.SnapshotRef_Kind, repo, key string) (*hostlinkv1.SnapshotRef, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	hc := c.hosts[hostID]
	if hc == nil {
		return nil, false
	}
	for _, ref := range hc.snaps {
		if ref.GetKind() == kind && ref.GetRepo() == repo && (key == "" || ref.GetKey() == key) {
			return ref, true
		}
	}
	return nil, false
}

// hostWithPrebuild returns a connected host holding repo's prebuild.
func (c *Core) hostWithPrebuild(repo string) (*hostConn, *hostlinkv1.SnapshotRef) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, hc := range c.hosts {
		for _, ref := range hc.snaps {
			if ref.GetKind() == hostlinkv1.SnapshotRef_KIND_PREBUILD && ref.GetRepo() == repo {
				return hc, ref
			}
		}
	}
	return nil, nil
}

// requestPull asks hostID to pull repo's prebuild from a peer in its region.
func (c *Core) requestPull(hostID, repo string) {
	c.mu.Lock()
	target := c.hosts[hostID]
	var from *hostConn
	var ref *hostlinkv1.SnapshotRef
	for _, hc := range c.hosts {
		if hc == target || target == nil || hc.region != target.region {
			continue
		}
		for _, r := range hc.snaps {
			if r.GetKind() == hostlinkv1.SnapshotRef_KIND_PREBUILD && r.GetRepo() == repo {
				from, ref = hc, r
			}
		}
	}
	c.mu.Unlock()
	if target == nil || from == nil {
		return
	}
	target.send(&hostlinkv1.LinkResponse{Msg: &hostlinkv1.LinkResponse_PullSnapshot{PullSnapshot: &hostlinkv1.PullSnapshot{
		Snapshot:   ref,
		FromHostId: from.id,
	}}})
}
