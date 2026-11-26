package telemetry

// perf-only: where a job's wall-clock time went.
//
// A job's timeline is its phases (record.go) plus spans: timed steps from
// the control plane (recorded here through the request context), hostd and
// the guest (reported in JobResult). Breakdown splits the job's wall clock
// into contiguous segments between consecutive phases and, for each, how
// much of it the spans account for. Time no span covers is unattributed: a
// gap in the instrumentation, to be driven to zero.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

// HostReceived is when hostd got the AssignJob, on the host's clock.
const HostReceived Phase = "host_received"

// PhaseOrder is the order phases happen in; Breakdown's segments follow it.
var PhaseOrder = []Phase{WebhookReceived, HostAssigned, HostReceived, VMResumed, RunnerOnline, JobStarted, JobFinished, VMDestroyed}

// segmentNames names the segment between two adjacent phases.
var segmentNames = map[[2]Phase]string{
	{WebhookReceived, HostAssigned}: "control_plane",
	{HostAssigned, HostReceived}:    "dispatch",
	{HostReceived, VMResumed}:       "host_prep",
	{VMResumed, RunnerOnline}:       "guest_setup_and_runner_start",
	{RunnerOnline, JobStarted}:      "runner_waiting_for_job",
	{JobStarted, JobFinished}:       "job",
	{JobFinished, VMDestroyed}:      "teardown",
}

// Span is one timed step of a job.
type Span struct {
	Component string            `json:"component"` // control_plane | host | guest
	Name      string            `json:"name"`
	Start     time.Time         `json:"start"`
	Duration  time.Duration     `json:"duration_ns"`
	Depth     int               `json:"depth"`
	Attrs     map[string]string `json:"attrs,omitempty"`
}

func (s Span) End() time.Time { return s.Start.Add(s.Duration) }

// Timeline collects control-plane spans for one job as its request moves
// through the adapter and core. Safe for concurrent use.
type Timeline struct {
	mu    sync.Mutex
	spans []Span
	open  []int // indexes of open spans, innermost last
}

type timelineKey struct{}

// WithTimeline returns ctx carrying a new timeline.
func WithTimeline(ctx context.Context) (context.Context, *Timeline) {
	tl := &Timeline{}
	return context.WithValue(ctx, timelineKey{}, tl), tl
}

// TimelineFrom returns ctx's timeline, or nil.
func TimelineFrom(ctx context.Context) *Timeline {
	tl, _ := ctx.Value(timelineKey{}).(*Timeline)
	return tl
}

// StartSpan opens a control-plane span on ctx's timeline and returns the
// function that closes it; with no timeline both are no-ops. attrs are
// key, value pairs.
//
//	defer telemetry.StartSpan(ctx, "store.save_job")()
func StartSpan(ctx context.Context, name string, attrs ...string) func() {
	tl := TimelineFrom(ctx)
	if tl == nil {
		return func() {}
	}
	var a map[string]string
	if len(attrs) > 1 {
		a = make(map[string]string, len(attrs)/2)
		for i := 0; i+1 < len(attrs); i += 2 {
			a[attrs[i]] = attrs[i+1]
		}
	}
	tl.mu.Lock()
	i := len(tl.spans)
	tl.spans = append(tl.spans, Span{Component: "control_plane", Name: name, Start: time.Now(), Depth: len(tl.open), Attrs: a})
	tl.open = append(tl.open, i)
	tl.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			tl.mu.Lock()
			defer tl.mu.Unlock()
			tl.spans[i].Duration = time.Since(tl.spans[i].Start)
			for k := len(tl.open) - 1; k >= 0; k-- {
				if tl.open[k] == i {
					tl.open = append(tl.open[:k], tl.open[k+1:]...)
					break
				}
			}
		})
	}
}

// Annotate sets an attribute on ctx's innermost open span, for what a step
// learns while it runs (a prebuild match, an HTTP status). No-op without a
// timeline or an open span.
func Annotate(ctx context.Context, key, value string) {
	tl := TimelineFrom(ctx)
	if tl == nil {
		return
	}
	tl.mu.Lock()
	defer tl.mu.Unlock()
	if len(tl.open) == 0 {
		return
	}
	s := &tl.spans[tl.open[len(tl.open)-1]]
	if s.Attrs == nil {
		s.Attrs = map[string]string{}
	}
	s.Attrs[key] = value
}

// Spans returns the timeline's spans so far.
func (tl *Timeline) Spans() []Span {
	if tl == nil {
		return nil
	}
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return append([]Span(nil), tl.spans...)
}

// AddSpans appends spans to the record.
func (r *Record) AddSpans(s ...Span) {
	r.mu.Lock()
	r.spans = append(r.spans, s...)
	r.mu.Unlock()
}

// Spans returns the record's spans in start order.
func (r *Record) Spans() []Span {
	r.mu.Lock()
	out := append([]Span(nil), r.spans...)
	r.mu.Unlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// Segment is the time between two consecutive recorded phases.
type Segment struct {
	Name  string `json:"name"`
	From  Phase  `json:"from"`
	To    Phase  `json:"to"`
	Start time.Time
	// Seconds can be negative when the two phases come from different
	// clocks (control plane vs host) that disagree; such a segment is
	// reported, not hidden.
	Seconds             float64 `json:"seconds"`
	AttributedSeconds   float64 `json:"attributed_seconds"`
	UnattributedSeconds float64 `json:"unattributed_seconds"`
}

// Breakdown is a job's wall clock split into segments.
type Breakdown struct {
	Start               time.Time `json:"start"`
	WallSeconds         float64   `json:"wall_seconds"`
	UnattributedSeconds float64   `json:"unattributed_seconds"`
	Segments            []Segment `json:"segments"`
	// MissingPhases are phases in PhaseOrder the job never reported.
	MissingPhases []Phase `json:"missing_phases,omitempty"`
}

// requestPhase names the time before webhook_received that control-plane
// spans cover (reading and verifying the webhook).
const requestPhase Phase = "request"

// Breakdown computes the job's segments from its phases and spans.
func (r *Record) Breakdown() Breakdown {
	spans := r.Spans()
	type point struct {
		p Phase
		t time.Time
	}
	var pts []point
	var b Breakdown
	for _, p := range PhaseOrder {
		if t, ok := r.At(p); ok {
			pts = append(pts, point{p, t})
		} else {
			b.MissingPhases = append(b.MissingPhases, p)
		}
	}
	if len(pts) == 0 {
		return b
	}
	// Spans that start before the first phase (the webhook's own read and
	// verify) get a leading segment of their own.
	if len(spans) > 0 && spans[0].Start.Before(pts[0].t) {
		pts = append([]point{{requestPhase, spans[0].Start}}, pts...)
	}
	b.Start = pts[0].t
	b.WallSeconds = pts[len(pts)-1].t.Sub(pts[0].t).Seconds()
	for i := 0; i+1 < len(pts); i++ {
		from, to := pts[i], pts[i+1]
		name, ok := segmentNames[[2]Phase{from.p, to.p}]
		if !ok {
			name = string(from.p) + "→" + string(to.p)
		}
		seg := Segment{Name: name, From: from.p, To: to.p, Start: from.t, Seconds: to.t.Sub(from.t).Seconds()}
		if seg.Seconds > 0 {
			seg.AttributedSeconds = covered(spans, from.t, to.t).Seconds()
			seg.UnattributedSeconds = seg.Seconds - seg.AttributedSeconds
		}
		b.UnattributedSeconds += seg.UnattributedSeconds
		b.Segments = append(b.Segments, seg)
	}
	return b
}

// covered is how much of [from, to) the union of spans covers. Spans
// nest, so the union counts a parent and its children once.
func covered(spans []Span, from, to time.Time) time.Duration {
	type iv struct{ a, b time.Time }
	var ivs []iv
	for _, s := range spans {
		a, e := s.Start, s.End()
		if a.Before(from) {
			a = from
		}
		if e.After(to) {
			e = to
		}
		if e.After(a) {
			ivs = append(ivs, iv{a, e})
		}
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].a.Before(ivs[j].a) })
	var total time.Duration
	var cur *iv
	for i := range ivs {
		v := ivs[i]
		switch {
		case cur == nil:
			cur = &v
		case !v.a.After(cur.b):
			if v.b.After(cur.b) {
				cur.b = v.b
			}
		default:
			total += cur.b.Sub(cur.a)
			cur = &v
		}
	}
	if cur != nil {
		total += cur.b.Sub(cur.a)
	}
	return total
}

// TimelineJSON is the breakdown and spans as stored in ClickHouse
// (job_records.timeline). Span starts are offsets from the breakdown's
// start, in seconds, so they read without timestamp parsing.
type TimelineJSON struct {
	Breakdown Breakdown  `json:"breakdown"`
	Spans     []spanJSON `json:"spans"`
}

type spanJSON struct {
	Component     string            `json:"component"`
	Name          string            `json:"name"`
	OffsetSeconds float64           `json:"offset_seconds"`
	Seconds       float64           `json:"seconds"`
	Depth         int               `json:"depth"`
	Attrs         map[string]string `json:"attrs,omitempty"`
}

// TimelineJSON returns the record's breakdown and spans for storage.
func (r *Record) TimelineJSON() TimelineJSON {
	b := r.Breakdown()
	out := TimelineJSON{Breakdown: b, Spans: []spanJSON{}}
	for _, s := range r.Spans() {
		out.Spans = append(out.Spans, spanJSON{
			Component: s.Component, Name: s.Name,
			OffsetSeconds: s.Start.Sub(b.Start).Seconds(), Seconds: s.Duration.Seconds(),
			Depth: s.Depth, Attrs: s.Attrs,
		})
	}
	return out
}

// timelineString is TimelineJSON encoded, or "" when there is nothing.
func (r *Record) timelineString() string {
	t := r.TimelineJSON()
	if len(t.Breakdown.Segments) == 0 && len(t.Spans) == 0 {
		return ""
	}
	b, err := json.Marshal(t)
	if err != nil {
		return ""
	}
	return string(b)
}

// BreakdownLogAttrs is the per-job latency breakdown as log attributes:
// wall and unattributed time, each segment, and the slowest top-level
// steps. One line per job, for the perf environment.
func (r *Record) BreakdownLogAttrs(slowest int) []any {
	b := r.Breakdown()
	segs := make([]any, 0, len(b.Segments))
	for _, s := range b.Segments {
		segs = append(segs, map[string]float64{
			s.Name: round3(s.Seconds), "unattributed": round3(s.UnattributedSeconds),
		})
	}
	var top []Span
	for _, s := range r.Spans() {
		if s.Depth == 0 {
			top = append(top, s)
		}
	}
	sort.SliceStable(top, func(i, j int) bool { return top[i].Duration > top[j].Duration })
	if len(top) > slowest {
		top = top[:slowest]
	}
	slow := make([]string, 0, len(top))
	for _, s := range top {
		slow = append(slow, s.Component+":"+s.Name+"="+formatSeconds(s.Duration))
	}
	pct := 0.0
	if b.WallSeconds > 0 {
		pct = 100 * b.UnattributedSeconds / b.WallSeconds
	}
	missing := make([]string, 0, len(b.MissingPhases))
	for _, p := range b.MissingPhases {
		missing = append(missing, string(p))
	}
	return []any{
		"wall_s", round3(b.WallSeconds),
		"unattributed_s", round3(b.UnattributedSeconds),
		"unattributed_pct", round3(pct),
		"segments", segs,
		"slowest", strings.Join(slow, " "),
		"missing_phases", strings.Join(missing, ","),
	}
}

func round3(f float64) float64 { return float64(int64(f*1000+0.5*sign(f))) / 1000 }

func sign(f float64) float64 {
	if f < 0 {
		return -1
	}
	return 1
}

func formatSeconds(d time.Duration) string { return d.Round(time.Millisecond).String() }
