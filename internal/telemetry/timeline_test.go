package telemetry

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"
)

var tl0 = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

func at(ms int) time.Time { return tl0.Add(time.Duration(ms) * time.Millisecond) }

func span(comp, name string, startMS, durMS, depth int) Span {
	return Span{Component: comp, Name: name, Start: at(startMS), Duration: time.Duration(durMS) * time.Millisecond, Depth: depth}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestBreakdownSegmentsSumToWallClock(t *testing.T) {
	r := NewRecord("j1")
	r.Mark(WebhookReceived, at(0))
	r.Mark(HostAssigned, at(400))
	r.Mark(HostReceived, at(450))
	r.Mark(VMResumed, at(1450))
	r.Mark(RunnerOnline, at(3450))
	r.Mark(JobStarted, at(4450))
	r.Mark(JobFinished, at(64450))
	r.Mark(VMDestroyed, at(65450))
	r.AddSpans(
		span("control_plane", "webhook.read_verify", -50, 40, 0), // before webhook_received
		span("control_plane", "github.http", 0, 300, 0),
		span("host", "take_vm", 500, 600, 0),
		span("host", "exec ip link", 520, 100, 1), // nested: counted once
		span("host", "resume", 1100, 300, 0),
		span("guest", "guest.runner", 1500, 63000, 0),
		span("host", "destroy", 64500, 900, 0),
	)
	b := r.Breakdown()
	if len(b.MissingPhases) != 0 {
		t.Fatalf("missing %v", b.MissingPhases)
	}
	want := []struct {
		name       string
		sec, unatt float64
	}{
		{"request→webhook_received", 0.05, 0.01},
		{"control_plane", 0.4, 0.1},
		{"dispatch", 0.05, 0.05},
		{"host_prep", 1.0, 0.1}, // 450..500 and 1400..1450 uncovered
		{"guest_setup_and_runner_start", 2.0, 0.05},
		{"runner_waiting_for_job", 1.0, 0},
		{"job", 60.0, 0},
		{"teardown", 1.0, 0.05}, // the runner span overlaps its first 50 ms
	}
	if len(b.Segments) != len(want) {
		t.Fatalf("segments = %+v", b.Segments)
	}
	var sum, unatt float64
	for i, w := range want {
		s := b.Segments[i]
		if s.Name != w.name || !approx(s.Seconds, w.sec) || !approx(s.UnattributedSeconds, w.unatt) {
			t.Errorf("segment %d = %s %.3fs (unattributed %.3f), want %s %.3fs (%.3f)",
				i, s.Name, s.Seconds, s.UnattributedSeconds, w.name, w.sec, w.unatt)
		}
		sum += s.Seconds
		unatt += s.UnattributedSeconds
	}
	if !approx(sum, b.WallSeconds) || !approx(b.WallSeconds, 65.5) {
		t.Errorf("segments sum to %.3f, wall %.3f, want 65.5", sum, b.WallSeconds)
	}
	if !approx(unatt, b.UnattributedSeconds) {
		t.Errorf("unattributed %.3f != sum of segments %.3f", b.UnattributedSeconds, unatt)
	}
}

func TestBreakdownMissingPhasesAndSkew(t *testing.T) {
	r := NewRecord("j2")
	r.Mark(WebhookReceived, at(0))
	r.Mark(HostAssigned, at(400))
	// The host's clock is behind: it received the job "before" assignment.
	r.Mark(HostReceived, at(380))
	r.Mark(VMResumed, at(1380))
	b := r.Breakdown()
	if len(b.MissingPhases) != 4 {
		t.Errorf("missing = %v", b.MissingPhases)
	}
	d := b.Segments[1]
	if d.Name != "dispatch" || !approx(d.Seconds, -0.02) || d.UnattributedSeconds != 0 {
		t.Errorf("skewed dispatch = %+v; want -0.02 s, reported not hidden", d)
	}
	// With RunnerOnline missing, the next segment spans two phases.
	r.Mark(JobStarted, at(2380))
	b = r.Breakdown()
	if last := b.Segments[len(b.Segments)-1]; last.Name != "vm_resumed→job_started" {
		t.Errorf("last segment %q", last.Name)
	}
}

func TestStartSpanNestsOnContextTimeline(t *testing.T) {
	ctx, tl := WithTimeline(context.Background())
	end := StartSpan(ctx, "outer", "k", "v")
	StartSpan(ctx, "inner")()
	end()
	StartSpan(context.Background(), "no timeline")() // no-op
	s := tl.Spans()
	if len(s) != 2 || s[0].Name != "outer" || s[0].Depth != 0 || s[1].Depth != 1 || s[0].Attrs["k"] != "v" {
		t.Fatalf("spans = %+v", s)
	}
	if s[0].Component != "control_plane" || s[0].Duration < s[1].Duration {
		t.Errorf("outer %+v inner %+v", s[0], s[1])
	}
}

func TestRowCarriesTimeline(t *testing.T) {
	r := NewRecord("j3")
	r.Mark(WebhookReceived, at(0))
	r.Mark(HostAssigned, at(100))
	r.Mark(HostReceived, at(150))
	r.AddSpans(span("control_plane", "scheduler.place", 10, 20, 0))
	row := r.Row(Pricing{})
	if row.HostReceivedAt == nil || row.UnattributedSeconds == nil || !approx(*row.UnattributedSeconds, 0.13) {
		t.Fatalf("row = %+v", row)
	}
	var tj TimelineJSON
	if err := json.Unmarshal([]byte(row.Timeline), &tj); err != nil {
		t.Fatal(err)
	}
	if len(tj.Spans) != 1 || tj.Spans[0].Name != "scheduler.place" || !approx(tj.Spans[0].OffsetSeconds, 0.01) {
		t.Errorf("timeline = %s", row.Timeline)
	}
	// Spans survive persistence.
	if got := FromData(r.Data()).Spans(); len(got) != 1 || got[0].Name != "scheduler.place" {
		t.Errorf("spans after round trip = %+v", got)
	}
}

func TestAnnotateAndTimedTransport(t *testing.T) {
	if got := Route("/repos/acme/api/actions/runs/123/jobs"); got != "/repos/{owner}/{repo}/actions/runs/{n}/jobs" {
		t.Errorf("route = %s", got)
	}
	if got := Route("/app/installations/42/access_tokens"); got != "/app/installations/{n}/access_tokens" {
		t.Errorf("route = %s", got)
	}
	ctx, tl := WithTimeline(context.Background())
	rt := TimedTransport("github", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 201, Body: http.NoBody}, nil
	}))
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.github.com/repos/a/b/actions/runners/generate-jitconfig", nil)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	s := tl.Spans()
	if len(s) != 1 || s[0].Name != "github POST /repos/{owner}/{repo}/actions/runners/generate-jitconfig" || s[0].Attrs["status"] != "201" {
		t.Fatalf("spans = %+v", s)
	}
	Annotate(ctx, "ignored", "no open span") // must not panic or attach
	if len(tl.Spans()[0].Attrs) != 1 {
		t.Errorf("annotate attached to a closed span: %v", tl.Spans()[0].Attrs)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
