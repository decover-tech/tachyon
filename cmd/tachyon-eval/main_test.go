package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/decover-tech/tachyon/internal/frontend/github"
	"github.com/decover-tech/tachyon/internal/telemetry"
)

// The control plane must accept what the driver sends: a valid signature,
// a workflow_job event, and labels the adapter parses as a Tachyon job.
func TestSendQueuedIsAcceptedByTheAdapter(t *testing.T) {
	secret := []byte("s3cret")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !github.VerifySignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
			t.Error("signature rejected")
		}
		if r.Header.Get("X-GitHub-Event") != "workflow_job" {
			t.Errorf("event = %q", r.Header.Get("X-GitHub-Event"))
		}
		var ev struct {
			Action      string `json:"action"`
			WorkflowJob struct {
				Labels []string `json:"labels"`
			} `json:"workflow_job"`
		}
		json.Unmarshal(body, &ev)
		if _, err := github.ParseLabels(ev.WorkflowJob.Labels); err != nil || ev.Action != "queued" {
			t.Errorf("action %q labels %v: %v", ev.Action, ev.WorkflowJob.Labels, err)
		}
		w.WriteHeader(202)
	}))
	defer srv.Close()
	if err := sendQueued(srv.URL, secret, 42, "tachyon-ubuntu-24.04-2x", "o/r", 1); err != nil {
		t.Fatal(err)
	}
}

func TestSummaryMediansAndMarkdown(t *testing.T) {
	seg := func(name string, s, u float64) telemetry.Segment {
		return telemetry.Segment{Name: name, Seconds: s, UnattributedSeconds: u}
	}
	job := func(id string, hp float64) jobResult {
		return jobResult{JobID: id, Outcome: "failed", Breakdown: &telemetry.Breakdown{
			WallSeconds: hp + 1, Segments: []telemetry.Segment{seg("control_plane", 1, 0.1), seg("host_prep", hp, 0)},
		}, Spans: []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"component":"host","name":"take_vm","seconds":%g}`, hp))}}
	}
	r := report{Ref: "perf/instrumentation", Jobs: []jobResult{job("a", 1), job("b", 3), job("c", 2), {JobID: "d", TimedOut: true}}}
	r.Medians, r.SlowSpans = summarize(r.Jobs)
	if len(r.Medians) != 2 || r.Medians[1].Name != "host_prep" || r.Medians[1].MedianSeconds != 2 || r.Medians[1].Jobs != 3 {
		t.Fatalf("medians = %+v", r.Medians)
	}
	if len(r.SlowSpans) != 1 || r.SlowSpans[0].MedianSeconds != 2 || r.SlowSpans[0].MaxSeconds != 3 {
		t.Fatalf("spans = %+v", r.SlowSpans)
	}
	md := markdown(r)
	for _, want := range []string{"| host_prep | 2.000 |", "| d |  | – | – | timed out: true |", "| host | take_vm | 2.000 | 3.000 | 3 |"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
}
