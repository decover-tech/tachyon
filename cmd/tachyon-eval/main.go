// Command tachyon-eval is the perf branch's eval harness (perf-only; never
// merged to main).
//
//	tachyon-eval fakegh -listen 127.0.0.1:8099
//	tachyon-eval run -secret-file wh.secret -n 5 -out results.json -summary summary.md
//
// fakegh answers the GitHub REST calls the control plane makes (run with
// tachyon-controlplane -github-api-url http://127.0.0.1:8099). Tokens are
// granted with whatever permissions are asked for, branches are protected,
// and each generated JIT config is a placeholder: the runner in the VM
// starts and fails to reach GitHub, so a fake job measures everything
// Tachyon owns (webhook to runner start, and teardown) but has no
// runner_online, job_started or job segment. Real GitHub jobs need the App.
//
// run sends signed workflow_job "queued" webhooks to the control plane,
// waits for each job's row in ClickHouse tachyon.job_records, and reports
// each job's latency breakdown (internal/telemetry/timeline.go) as JSON
// and as a Markdown table for a GitHub job summary.
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/decover-tech/tachyon/internal/telemetry"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "fakegh":
		fakegh(os.Args[2:])
	case "run":
		if err := run(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "tachyon-eval:", err)
			os.Exit(1)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: tachyon-eval fakegh [-listen addr] | run [flags]")
	os.Exit(2)
}

// ---- fake GitHub API ----

func fakegh(args []string) {
	fs := flag.NewFlagSet("fakegh", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8099", "listen address")
	fs.Parse(args)
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	var runnerID atomic.Int64
	runnerID.Store(1000)
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Permissions map[string]string `json:"permissions"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		reply(w, 201, map[string]any{"token": "fake-installation-token", "expires_at": time.Now().Add(time.Hour), "permissions": body.Permissions})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		full := r.PathValue("owner") + "/" + r.PathValue("repo")
		reply(w, 200, map[string]any{
			"event": "push", "head_branch": "main",
			"head_repository": map[string]any{"full_name": full},
			"repository":      map[string]any{"full_name": full, "default_branch": "main"},
			"pull_requests":   []any{},
		})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/branches/{branch...}", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"protected": true})
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/actions/runners/generate-jitconfig", func(w http.ResponseWriter, r *http.Request) {
		id := runnerID.Add(1)
		reply(w, 201, map[string]any{"runner": map[string]any{"id": id}, "encoded_jit_config": "ZmFrZS1qaXQtY29uZmln"})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runners", func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, map[string]any{"total_count": 0, "runners": []any{}})
	})
	mux.HandleFunc("DELETE /repos/{owner}/{repo}/actions/runners/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})
	// Anything else (prebuild config lookups, trees) is "not found", and
	// logged so a new call the control plane makes shows up here.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Info("fakegh: unhandled", "method", r.Method, "path", r.URL.Path)
		reply(w, 404, map[string]any{"message": "Not Found"})
	})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		mux.ServeHTTP(w, r)
		log.Info("fakegh", "method", r.Method, "path", r.URL.Path, "ms", time.Since(start).Milliseconds())
	})
	log.Info("fakegh listening", "addr", *listen)
	if err := http.ListenAndServe(*listen, h); err != nil {
		log.Error("fakegh", "err", err)
		os.Exit(1)
	}
}

// ---- driver ----

type jobResult struct {
	JobID     string               `json:"job_id"`
	Outcome   string               `json:"outcome"`
	HostID    string               `json:"host_id"`
	Breakdown *telemetry.Breakdown `json:"breakdown,omitempty"`
	Spans     []json.RawMessage    `json:"spans,omitempty"`
	TimedOut  bool                 `json:"timed_out,omitempty"`
}

type report struct {
	Ref       string      `json:"ref"`
	Label     string      `json:"label"`
	Started   time.Time   `json:"started"`
	Jobs      []jobResult `json:"jobs"`
	Medians   []segStat   `json:"segment_medians"`
	SlowSpans []spanStat  `json:"slowest_spans"`
}

type segStat struct {
	Name               string  `json:"name"`
	MedianSeconds      float64 `json:"median_seconds"`
	MedianUnattributed float64 `json:"median_unattributed_seconds"`
	Jobs               int     `json:"jobs"`
}

type spanStat struct {
	Component     string  `json:"component"`
	Name          string  `json:"name"`
	MedianSeconds float64 `json:"median_seconds"`
	MaxSeconds    float64 `json:"max_seconds"`
	Count         int     `json:"count"`
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	webhook := fs.String("webhook", "http://127.0.0.1:8080/webhooks/github", "control plane webhook URL")
	secretFile := fs.String("secret-file", "", "file holding the webhook secret")
	n := fs.Int("n", 3, "jobs to send")
	interval := fs.Duration("interval", 20*time.Second, "time between jobs (0 = all at once)")
	serial := fs.Bool("serial", false, "wait for each job's record before sending the next (a host with room for one job)")
	label := fs.String("label", "tachyon-ubuntu-24.04-2x", "runs-on label")
	repo := fs.String("repo", "tachyon-eval/fake", "owner/name the fake jobs claim to come from")
	installation := fs.Int64("installation", 1, "installation ID")
	chURL := fs.String("clickhouse-url", "http://127.0.0.1:8123", "ClickHouse HTTP URL")
	chUser := fs.String("clickhouse-user", "tachyon", "ClickHouse user (password in TACHYON_CLICKHOUSE_PASSWORD)")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for every job's record")
	ref := fs.String("ref", "", "git ref under test, for the report")
	out := fs.String("out", "", "write the JSON report here")
	summary := fs.String("summary", "", "append a Markdown summary here (e.g. $GITHUB_STEP_SUMMARY)")
	fs.Parse(args)

	secret, err := os.ReadFile(*secretFile)
	if err != nil {
		return fmt.Errorf("webhook secret: %w", err)
	}
	secret = bytes.TrimSpace(secret)
	rep := report{Ref: *ref, Label: *label, Started: time.Now().UTC()}
	base := time.Now().Unix() * 1000
	pw := os.Getenv("TACHYON_CLICKHOUSE_PASSWORD")
	deadline := time.Now().Add(*timeout)
	got := map[string]jobResult{}
	// wait polls ClickHouse until every job in want has a record or the
	// deadline passes.
	wait := func(want []string) {
		for time.Now().Before(deadline) {
			rows, err := queryRecords(*chURL, *chUser, pw, want)
			if err != nil {
				fmt.Fprintln(os.Stderr, "clickhouse:", err)
			}
			for _, r := range rows {
				if _, seen := got[r.JobID]; !seen {
					var ghID int64
					fmt.Sscanf(r.JobID, "gh_%d", &ghID)
					if err := sendCompleted(*webhook, secret, ghID, *label, *repo, *installation); err != nil {
						fmt.Fprintln(os.Stderr, "completed:", err)
					}
				}
				got[r.JobID] = r
			}
			missing := 0
			for _, id := range want {
				if _, ok := got[id]; !ok {
					missing++
				}
			}
			if missing == 0 {
				return
			}
			time.Sleep(5 * time.Second)
		}
	}
	var ids []string
	for i := 0; i < *n; i++ {
		if i > 0 && *interval > 0 {
			time.Sleep(*interval)
		}
		ghID := base + int64(i)
		if err := sendQueued(*webhook, secret, ghID, *label, *repo, *installation); err != nil {
			return fmt.Errorf("job %d: %w", i, err)
		}
		id := fmt.Sprintf("gh_%d", ghID)
		ids = append(ids, id)
		fmt.Fprintf(os.Stderr, "sent %s\n", id)
		if *serial {
			wait([]string{id})
			fmt.Fprintf(os.Stderr, "recorded %s: %s\n", id, got[id].Outcome)
		}
	}
	wait(ids)
	for _, id := range ids {
		r, ok := got[id]
		if !ok {
			r = jobResult{JobID: id, TimedOut: true}
		}
		rep.Jobs = append(rep.Jobs, r)
	}
	rep.Medians, rep.SlowSpans = summarize(rep.Jobs)

	if *out != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			return err
		}
	}
	md := markdown(rep)
	fmt.Print(md)
	if *summary != "" {
		f, err := os.OpenFile(*summary, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		f.WriteString(md)
	}
	for _, j := range rep.Jobs {
		if j.TimedOut {
			return fmt.Errorf("%s: no job record within %s", j.JobID, *timeout)
		}
	}
	return nil
}

func sendQueued(webhook string, secret []byte, id int64, label, repo string, installation int64) error {
	return sendJobEvent(webhook, secret, "queued", "", id, label, repo, installation)
}

// sendCompleted tells the adapter the job is over, as GitHub does when a
// job ends. Fake jobs never reach GitHub's in_progress, so without it the
// reconcile loop would retry each one as still queued.
func sendCompleted(webhook string, secret []byte, id int64, label, repo string, installation int64) error {
	return sendJobEvent(webhook, secret, "completed", "failure", id, label, repo, installation)
}

func sendJobEvent(webhook string, secret []byte, action, conclusion string, id int64, label, repo string, installation int64) error {
	owner, _, _ := strings.Cut(repo, "/")
	job := map[string]any{
		"id": id, "run_id": id, "run_attempt": 1,
		"workflow_name": "tachyon-eval", "name": "eval",
		"head_sha": fmt.Sprintf("%040x", id), "head_branch": "main",
		"labels": []string{label},
	}
	if conclusion != "" {
		job["conclusion"] = conclusion
	}
	ev := map[string]any{
		"action":       action,
		"workflow_job": job,
		"repository":   map[string]any{"full_name": repo, "private": true, "owner": map[string]any{"login": owner}, "default_branch": "main"},
		"installation": map[string]any{"id": installation},
	}
	body, _ := json.Marshal(ev)
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	req, _ := http.NewRequest("POST", webhook, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "workflow_job")
	req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("eval-%d-%s", id, action))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("webhook: %s %s", resp.Status, b)
	}
	return nil
}

func queryRecords(chURL, user, pw string, ids []string) ([]jobResult, error) {
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = "'" + strings.ReplaceAll(id, "'", "") + "'"
	}
	q := fmt.Sprintf("SELECT job_id, outcome, host_id, timeline FROM tachyon.job_records FINAL WHERE job_id IN (%s) FORMAT JSONEachRow", strings.Join(quoted, ","))
	req, _ := http.NewRequest("POST", chURL+"/", strings.NewReader(q))
	req.Header.Set("X-ClickHouse-User", user)
	if pw != "" {
		req.Header.Set("X-ClickHouse-Key", pw)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: %s", resp.Status, b)
	}
	var out []jobResult
	dec := json.NewDecoder(bytes.NewReader(b))
	for dec.More() {
		var row struct {
			JobID    string `json:"job_id"`
			Outcome  string `json:"outcome"`
			HostID   string `json:"host_id"`
			Timeline string `json:"timeline"`
		}
		if err := dec.Decode(&row); err != nil {
			return out, err
		}
		r := jobResult{JobID: row.JobID, Outcome: row.Outcome, HostID: row.HostID}
		if row.Timeline != "" {
			var tl struct {
				Breakdown telemetry.Breakdown `json:"breakdown"`
				Spans     []json.RawMessage   `json:"spans"`
			}
			if err := json.Unmarshal([]byte(row.Timeline), &tl); err == nil {
				r.Breakdown, r.Spans = &tl.Breakdown, tl.Spans
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func summarize(jobs []jobResult) ([]segStat, []spanStat) {
	var order []string
	secs, unatt := map[string][]float64{}, map[string][]float64{}
	type key struct{ c, n string }
	spans := map[key][]float64{}
	for _, j := range jobs {
		if j.Breakdown == nil {
			continue
		}
		for _, s := range j.Breakdown.Segments {
			if _, ok := secs[s.Name]; !ok {
				order = append(order, s.Name)
			}
			secs[s.Name] = append(secs[s.Name], s.Seconds)
			unatt[s.Name] = append(unatt[s.Name], s.UnattributedSeconds)
		}
		for _, raw := range j.Spans {
			var s struct {
				Component string  `json:"component"`
				Name      string  `json:"name"`
				Seconds   float64 `json:"seconds"`
				Depth     int     `json:"depth"`
			}
			if json.Unmarshal(raw, &s) == nil {
				k := key{s.Component, s.Name}
				spans[k] = append(spans[k], s.Seconds)
			}
		}
	}
	var segs []segStat
	for _, n := range order {
		segs = append(segs, segStat{Name: n, MedianSeconds: median(secs[n]), MedianUnattributed: median(unatt[n]), Jobs: len(secs[n])})
	}
	var sp []spanStat
	for k, v := range spans {
		mx := 0.0
		for _, x := range v {
			if x > mx {
				mx = x
			}
		}
		sp = append(sp, spanStat{Component: k.c, Name: k.n, MedianSeconds: median(v), MaxSeconds: mx, Count: len(v)})
	}
	sort.Slice(sp, func(i, j int) bool { return sp[i].MedianSeconds > sp[j].MedianSeconds })
	if len(sp) > 15 {
		sp = sp[:15]
	}
	return segs, sp
}

func markdown(r report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Tachyon perf eval `%s`\n\n", r.Ref)
	fmt.Fprintf(&b, "%d job(s), label `%s`, started %s\n\n", len(r.Jobs), r.Label, r.Started.Format(time.RFC3339))
	b.WriteString("| job | outcome | wall s | unattributed s | missing phases |\n|---|---|---:|---:|---|\n")
	for _, j := range r.Jobs {
		if j.Breakdown == nil {
			fmt.Fprintf(&b, "| %s | %s | – | – | timed out: %v |\n", j.JobID, j.Outcome, j.TimedOut)
			continue
		}
		var miss []string
		for _, p := range j.Breakdown.MissingPhases {
			miss = append(miss, string(p))
		}
		fmt.Fprintf(&b, "| %s | %s | %.3f | %.3f | %s |\n", j.JobID, j.Outcome, j.Breakdown.WallSeconds, j.Breakdown.UnattributedSeconds, strings.Join(miss, ", "))
	}
	b.WriteString("\n### Segments (median)\n\n| segment | seconds | unattributed s |\n|---|---:|---:|\n")
	for _, s := range r.Medians {
		fmt.Fprintf(&b, "| %s | %.3f | %.3f |\n", s.Name, s.MedianSeconds, s.MedianUnattributed)
	}
	b.WriteString("\n### Slowest steps (median over jobs)\n\n| component | step | median s | max s | n |\n|---|---|---:|---:|---:|\n")
	for _, s := range r.SlowSpans {
		fmt.Fprintf(&b, "| %s | %s | %.3f | %.3f | %d |\n", s.Component, s.Name, s.MedianSeconds, s.MaxSeconds, s.Count)
	}
	b.WriteString("\n")
	return b.String()
}
