package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/telemetry"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// perf-only: one job end to end. The control plane's own steps, the host's
// and the guest's spans land on one record; runner_online and job_started
// come from the guest's marks; the breakdown is logged once with every
// phase present.
func TestJobTimelineEndToEnd(t *testing.T) {
	e := newEnv()
	var logs lockedBuf
	e.core.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	s := e.connect(t, "h1", "us", 4, 16)
	s.push(t, heartbeat(false))

	ctx, _ := telemetry.WithTimeline(context.Background())
	sub := e.submission(t, "j1", 4)
	if err := e.core.Submit(ctx, sub); err != nil {
		t.Fatal(err)
	}
	s.next(t)

	rec := e.core.Record("j1")
	var cp []string
	for _, sp := range rec.Spans() {
		cp = append(cp, sp.Name)
	}
	// No Store in this env, so no store spans.
	for _, want := range []string{"images.pin", "scheduler.place", "build_assign", "prebuild.plan_fork", "secrets.get", "hostlink.enqueue"} {
		if !strings.Contains(strings.Join(cp, ","), want) {
			t.Errorf("control-plane spans %v lack %s", cp, want)
		}
	}

	base := time.Now()
	ts := func(ms int) *timestamppb.Timestamp {
		return timestamppb.New(base.Add(time.Duration(ms) * time.Millisecond))
	}
	ev := func(p hostlinkv1.Phase, ms int) {
		s.push(t, &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobEvent{JobEvent: &hostlinkv1.JobEvent{JobId: "j1", Phase: p, At: ts(ms)}}})
	}
	ev(hostlinkv1.Phase_PHASE_HOST_RECEIVED, 10)
	ev(hostlinkv1.Phase_PHASE_VM_RESUMED, 300)
	ev(hostlinkv1.Phase_PHASE_JOB_FINISHED, 5000)
	ev(hostlinkv1.Phase_PHASE_VM_DESTROYED, 5200)
	span := func(comp, name string, ms, dur int) *hostlinkv1.TimelineSpan {
		return &hostlinkv1.TimelineSpan{Component: comp, Name: name, Start: ts(ms), Duration: durationpb.New(time.Duration(dur) * time.Millisecond)}
	}
	s.push(t, &hostlinkv1.LinkRequest{Msg: &hostlinkv1.LinkRequest_JobResult{JobResult: &hostlinkv1.JobResult{
		JobId:   "j1",
		Outcome: hostlinkv1.JobResult_OUTCOME_SUCCEEDED,
		Spans: []*hostlinkv1.TimelineSpan{
			span("host", "take_vm", 20, 200),
			span("guest", "guest.runner", 400, 4500),
			span("guest", "guest.runner_online", 1200, 0),
			span("guest", "guest.job_started", 1500, 0),
			span("host", "destroy", 5000, 190),
		},
	}}})

	if got, ok := rec.At(telemetry.RunnerOnline); !ok || !got.Equal(base.Add(1200*time.Millisecond)) {
		t.Errorf("runner_online = %v %v; want the guest's anchored mark", got, ok)
	}
	if _, ok := rec.At(telemetry.JobStarted); !ok {
		t.Error("job_started not marked from the guest")
	}
	b := rec.Breakdown()
	if len(b.MissingPhases) != 0 {
		t.Errorf("missing phases %v", b.MissingPhases)
	}
	var sum float64
	for _, sg := range b.Segments {
		sum += sg.Seconds
	}
	if d := sum - b.WallSeconds; d > 1e-6 || d < -1e-6 {
		t.Errorf("segments sum %.6f != wall %.6f", sum, b.WallSeconds)
	}

	var line map[string]any
	for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["msg"] == "job breakdown" {
			if line != nil {
				t.Fatal("breakdown logged twice")
			}
			line = m
		}
	}
	if line == nil || line["job"] != "j1" || line["host"] != "h1" || line["missing_phases"] != "" {
		t.Fatalf("breakdown line = %v", line)
	}
	if !strings.Contains(line["slowest"].(string), "guest:guest.runner=") {
		t.Errorf("slowest = %v", line["slowest"])
	}
}
