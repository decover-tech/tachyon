// Package contract checks a running Tachyon cache service against the
// real GitHub clients: the newest actions/cache release and the newest
// @actions/cache toolkit package. The cache protocol is GitHub-internal and
// was rewritten in 2025, so these run daily (.github/workflows/
// cache-contract.yml) to catch a client change before customers do.
//
// The suite drives each client as a workflow would (save, exact restore,
// restore-key prefix, miss) and checks GitHub's scoping rules end to end:
// the default branch is readable by every ref, pull-request entries stay
// with their pull request, and forks can read but never write.
package contract

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/decover-tech/tachyon/internal/jobapi"
)

// JobSpec is one simulated job: the ref it runs on and the cache token
// the control plane would give its VM.
type JobSpec struct {
	Name    string
	Ref     string // GITHUB_REF
	BaseRef string // pull requests: the target branch
	Event   string // GITHUB_EVENT_NAME
	Trust   jobapi.Trust
	Token   string
}

// Target is the cache service under test.
type Target struct {
	URL           string // the service's base URL, as ACTIONS_RESULTS_URL
	Repo          string // owner/name the job tokens are scoped to
	DefaultBranch string
	Jobs          Jobs
}

// Jobs are the four jobs the suite needs.
type Jobs struct {
	Main JobSpec // a push to the default branch
	PR1  JobSpec // a same-repo pull request
	PR2  JobSpec // another same-repo pull request
	Fork JobSpec // a pull request from a fork
}

// All returns the jobs in a fixed order, for filling in tokens.
func (j *Jobs) All() []*JobSpec { return []*JobSpec{&j.Main, &j.PR1, &j.PR2, &j.Fork} }

// DefaultJobs returns the suite's jobs, without tokens.
func DefaultJobs(defaultBranch string) Jobs {
	pr := func(name string, n int, trust jobapi.Trust) JobSpec {
		return JobSpec{Name: name, Ref: fmt.Sprintf("refs/pull/%d/merge", n), BaseRef: defaultBranch, Event: "pull_request", Trust: trust}
	}
	return Jobs{
		Main: JobSpec{Name: "main", Ref: defaultBranch, Event: "push", Trust: jobapi.Trusted},
		PR1:  pr("pr1", 1, jobapi.Untrusted),
		PR2:  pr("pr2", 2, jobapi.Untrusted),
		Fork: pr("fork", 3, jobapi.UntrustedFork),
	}
}

// Config tunes a suite run.
type Config struct {
	// KeyPrefix makes this run's keys unique, so the suite can run against
	// a long-lived service. Required.
	KeyPrefix string
	// BlobBytes is the size of the incompressible file in each entry, so
	// uploads and downloads move real data. Default 4 MiB.
	BlobBytes int
	// TempDir holds per-step workspaces. Default os.TempDir().
	TempDir string
	// Settle is the wait between two saves whose order a check relies on,
	// so their creation times differ even at one-second resolution.
	// Default 1.1s.
	Settle time.Duration
}

// Check is the outcome of one contract check.
type Check struct {
	Name string
	Err  error
}

// Report is one client's run of the suite.
type Report struct {
	Client string
	Checks []Check
}

// Failed reports whether any check failed.
func (r Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Err != nil {
			return true
		}
	}
	return false
}

// Markdown renders the report as a table for $GITHUB_STEP_SUMMARY.
func (r Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "### %s\n\n| Check | Result |\n| --- | --- |\n", r.Client)
	for _, c := range r.Checks {
		res := "pass"
		if c.Err != nil {
			res = "**FAIL**: " + strings.NewReplacer("|", `\|`, "\n", " ").Replace(c.Err.Error())
		}
		fmt.Fprintf(&b, "| %s | %s |\n", c.Name, res)
	}
	return b.String()
}

// paths is what every entry caches, relative to the workspace.
var paths = []string{"data"}

// content is one entry's files.
type content map[string][]byte

func newContent(blobBytes int) (content, error) {
	blob := make([]byte, blobBytes)
	if _, err := rand.Read(blob); err != nil {
		return nil, err
	}
	small := make([]byte, 16)
	if _, err := rand.Read(small); err != nil {
		return nil, err
	}
	return content{"small.txt": fmt.Appendf(nil, "%x\n", small), "nested/blob.bin": blob}, nil
}

func (c content) write(ws string) error {
	for name, b := range c {
		p := filepath.Join(ws, "data", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// matches reports whether ws holds exactly c's files.
func (c content) matches(ws string) error {
	for name, want := range c {
		got, err := os.ReadFile(filepath.Join(ws, "data", filepath.FromSlash(name)))
		if err != nil {
			return fmt.Errorf("restored files: %w", err)
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("restored %s differs from what was saved (%d bytes, want %d)", name, len(got), len(want))
		}
	}
	return nil
}

// suite is one run's state.
type suite struct {
	ctx    context.Context
	c      Client
	t      Target
	cfg    Config
	report *Report
}

// Run runs the contract suite with client against the service in target.
// It never stops early on a failed check, except when the first save fails,
// since every later check depends on it.
func Run(ctx context.Context, c Client, target Target, cfg Config) Report {
	if cfg.BlobBytes <= 0 {
		cfg.BlobBytes = 4 << 20
	}
	if cfg.Settle <= 0 {
		cfg.Settle = 1100 * time.Millisecond
	}
	if cfg.TempDir == "" {
		cfg.TempDir = os.TempDir()
	}
	r := &Report{Client: c.Name()}
	if cfg.KeyPrefix == "" {
		r.Checks = append(r.Checks, Check{"config", errors.New("contract: KeyPrefix is required")})
		return *r
	}
	s := &suite{ctx: ctx, c: c, t: target, cfg: cfg, report: r}
	s.run()
	return *r
}

func (s *suite) key(parts ...string) string {
	return s.cfg.KeyPrefix + "-" + strings.Join(parts, "-")
}

func (s *suite) check(name string, f func() error) bool {
	err := f()
	s.report.Checks = append(s.report.Checks, Check{Name: name, Err: err})
	return err == nil
}

func (s *suite) workspace() (string, error) {
	return os.MkdirTemp(s.cfg.TempDir, "tachyon-contract-ws-")
}

// save writes c into a fresh workspace and saves it as key. strict requires
// the save to have worked: no warnings or errors, and the entry restorable
// by the same job. (The clients report some save failures, such as a
// refused reservation, only as plain log lines, so the restore is what
// proves the save.)
func (s *suite) save(job JobSpec, key string, c content, strict bool) (StepResult, error) {
	ws, err := s.workspace()
	if err != nil {
		return StepResult{}, err
	}
	defer os.RemoveAll(ws)
	if err := c.write(ws); err != nil {
		return StepResult{}, err
	}
	res, err := s.c.Save(s.ctx, job, ws, key, paths)
	if err != nil {
		return res, stepErr("save", res, err)
	}
	if !strict {
		return res, nil
	}
	if p := res.Problems(); len(p) > 0 {
		return res, stepErr("save", res, fmt.Errorf("%s as %s: %v", key, job.Name, p))
	}
	if res.Outputs["cache-id"] == "-1" {
		return res, stepErr("save", res, fmt.Errorf("%s as %s: client reported cache id -1 (not saved)", key, job.Name))
	}
	if err := s.restore(job, key, nil, key, c); err != nil {
		return res, fmt.Errorf("save %s as %s, then restoring it: %w", key, job.Name, err)
	}
	return res, nil
}

// restore restores into a fresh workspace and checks the outcome: want is
// the expected matched key ("" for a miss), and on a match the files must
// equal c. Every restore must be clean: no warnings, no errors.
func (s *suite) restore(job JobSpec, key string, restoreKeys []string, want string, c content) error {
	ws, err := s.workspace()
	if err != nil {
		return err
	}
	defer os.RemoveAll(ws)
	res, err := s.c.Restore(s.ctx, job, ws, key, restoreKeys, paths)
	if err != nil {
		return stepErr("restore", res, err)
	}
	if p := res.Problems(); len(p) > 0 {
		return stepErr("restore", res, fmt.Errorf("%s as %s: %v", key, job.Name, p))
	}
	got := res.MatchedKey()
	switch {
	case want == "" && got != "":
		return fmt.Errorf("restore %s as %s matched %q, want a miss", key, job.Name, got)
	case want != "" && got != want:
		return fmt.Errorf("restore %s as %s matched %q, want %q", key, job.Name, got, want)
	case res.Hit() != (want != "" && want == key):
		return fmt.Errorf("restore %s as %s: cache-hit=%v, want %v", key, job.Name, res.Hit(), want == key)
	}
	if want != "" {
		return c.matches(ws)
	}
	return nil
}

func stepErr(op string, res StepResult, err error) error {
	log := strings.TrimSpace(string(res.Log))
	if len(log) > 2000 {
		log = "..." + log[len(log)-2000:]
	}
	if log == "" {
		return fmt.Errorf("%s: %w", op, err)
	}
	return fmt.Errorf("%s: %w\n%s", op, err, log)
}

func (s *suite) run() {
	j := s.t.Jobs
	var a, b, pr, dup, fork content
	for _, c := range []*content{&a, &b, &pr, &dup, &fork} {
		var err error
		if *c, err = newContent(s.cfg.BlobBytes); err != nil {
			s.check("generate content", func() error { return err })
			return
		}
	}
	mainKey1, mainKey2 := s.key("main", "1"), s.key("main", "2")
	prKey, forkKey := s.key("pr", "1"), s.key("fork", "1")

	if !s.check("save from the default branch", func() error {
		_, err := s.save(j.Main, mainKey1, a, true)
		return err
	}) {
		return
	}
	s.check("exact restore", func() error {
		return s.restore(j.Main, mainKey1, nil, mainKey1, a)
	})
	s.check("restore-key prefix match", func() error {
		return s.restore(j.Main, s.key("main", "nope"), []string{s.key("main") + "-"}, mainKey1, a)
	})
	s.check("miss is clean", func() error {
		return s.restore(j.Main, s.key("absent"), []string{s.key("absent") + "-"}, "", nil)
	})
	s.check("newest entry wins a prefix match", func() error {
		time.Sleep(s.cfg.Settle)
		if _, err := s.save(j.Main, mainKey2, b, true); err != nil {
			return err
		}
		return s.restore(j.Main, s.key("main", "nope"), []string{s.key("main") + "-"}, mainKey2, b)
	})
	s.check("entries are immutable", func() error {
		// The client may warn that the key is taken; the entry must not change.
		if _, err := s.save(j.Main, mainKey1, dup, false); err != nil {
			return err
		}
		return s.restore(j.Main, mainKey1, nil, mainKey1, a)
	})
	s.check("pull request reads the default branch", func() error {
		return s.restore(j.PR1, mainKey1, nil, mainKey1, a)
	})
	if s.check("pull request saves its own entry", func() error {
		_, err := s.save(j.PR1, prKey, pr, true)
		return err
	}) {
		s.check("other pull requests can't read it", func() error {
			return s.restore(j.PR2, prKey, []string{s.key("pr") + "-"}, "", nil)
		})
		s.check("the default branch can't read it", func() error {
			return s.restore(j.Main, prKey, []string{s.key("pr") + "-"}, "", nil)
		})
	}
	s.check("fork reads the default branch", func() error {
		return s.restore(j.Fork, mainKey1, nil, mainKey1, a)
	})
	s.check("fork can't write", func() error {
		// The client reports the refusal as a warning; nothing may be stored.
		if _, err := s.save(j.Fork, forkKey, fork, false); err != nil {
			return err
		}
		if err := s.restore(j.Fork, forkKey, nil, "", nil); err != nil {
			return err
		}
		return s.restore(j.Main, forkKey, nil, "", nil)
	})
}
