package guest

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"time"
)

// Runner log lines the agent turns into phases. These are the GitHub
// runner's own console messages.
const (
	runnerOnlineMarker = "Listening for Jobs"
	jobStartedMarker   = "Running job: "
	jobResultMarker    = "completed with result: "
)

// phaseWatcher passes runner output through to the console and records the
// time of the first runner_online and job_started lines, plus the job result.
type phaseWatcher struct {
	mu     sync.Mutex
	out    io.Writer
	now    func() time.Time
	buf    []byte
	phases map[string]string
	times  map[string]time.Time // perf-only: same phases, timeline clock
	tnow   func() time.Time     // perf-only: the timeline's clock
	result string
}

func newPhaseWatcher(out io.Writer, now func() time.Time) *phaseWatcher {
	return &phaseWatcher{out: out, now: now, phases: map[string]string{}, times: map[string]time.Time{}}
}

func (w *phaseWatcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.out.Write(p)
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.line(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	// A line longer than this carries no marker we care about.
	if len(w.buf) > 64<<10 {
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

// Flush handles a final line without a newline.
func (w *phaseWatcher) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.line(string(w.buf))
		w.buf = nil
	}
}

func (w *phaseWatcher) line(s string) {
	mark := func(phase string) {
		if _, ok := w.phases[phase]; !ok {
			w.phases[phase] = w.now().UTC().Format(time.RFC3339Nano)
			if w.tnow != nil {
				w.times[phase] = w.tnow()
			}
		}
	}
	switch {
	case strings.Contains(s, runnerOnlineMarker):
		mark(PhaseRunnerOnline)
	case strings.Contains(s, jobStartedMarker):
		mark(PhaseJobStarted)
	case strings.Contains(s, jobResultMarker):
		r := s[strings.Index(s, jobResultMarker)+len(jobResultMarker):]
		w.result = strings.TrimSpace(r)
	}
}

// Phases returns the recorded phase timestamps.
func (w *phaseWatcher) Phases() map[string]string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]string, len(w.phases))
	for k, v := range w.phases {
		out[k] = v
	}
	return out
}

// PhaseTimes returns the recorded phases' times (perf-only).
func (w *phaseWatcher) PhaseTimes() map[string]time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]time.Time, len(w.times))
	for k, v := range w.times {
		out[k] = v
	}
	return out
}

// Result returns the job result the runner printed ("Succeeded", "Failed",
// "Canceled"), or "" if it printed none.
func (w *phaseWatcher) Result() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.result
}
