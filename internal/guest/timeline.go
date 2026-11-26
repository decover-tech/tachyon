package guest

import (
	"sync"
	"time"
)

// perf-only: the guest's side of a job's timeline.
//
// Spans are offsets from the payload's arrival on the guest's monotonic
// clock (time.Time.Sub uses it), so the guest's wall clock — restored from a
// snapshot, possibly stale — never enters a duration. hostd anchors the
// offsets on its own clock (hostd/src/hypervisor.rs, anchored_spans).

// TimelineSpan is one guest step. Marks (runner_online, job_started) have
// DurMS 0.
type TimelineSpan struct {
	Name    string            `json:"name"`
	StartMS float64           `json:"start_ms"`
	DurMS   float64           `json:"dur_ms"`
	Depth   int               `json:"depth"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

type timeline struct {
	mu     sync.Mutex
	origin time.Time
	now    func() time.Time
	depth  int
	spans  []TimelineSpan
}

func newTimeline(origin time.Time, now func() time.Time) *timeline {
	return &timeline{origin: origin, now: now}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// span times fn as a step named name. A nil timeline only runs fn.
func (t *timeline) span(name string, attrs map[string]string, fn func() error) error {
	if t == nil {
		return fn()
	}
	t.mu.Lock()
	i := len(t.spans)
	start := t.now()
	t.spans = append(t.spans, TimelineSpan{Name: name, StartMS: ms(start.Sub(t.origin)), Depth: t.depth, Attrs: attrs})
	t.depth++
	t.mu.Unlock()

	err := fn()

	t.mu.Lock()
	t.spans[i].DurMS = ms(t.now().Sub(start))
	t.depth--
	t.mu.Unlock()
	return err
}

// mark records an instant at the current nesting depth.
func (t *timeline) mark(name string, at time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spans = append(t.spans, TimelineSpan{Name: name, StartMS: ms(at.Sub(t.origin)), Depth: t.depth})
}

func (t *timeline) snapshot() []TimelineSpan {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]TimelineSpan(nil), t.spans...)
}
