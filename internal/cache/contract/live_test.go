package contract

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// startService starts the cache service in-process and returns target
// with URL and job tokens filled in. It is set by service_test.go, which
// builds only with -tags contract, because it needs the service itself.
var startService func(t *testing.T, target Target) Target

// TestLive runs the contract suite with real clients. It is skipped unless
// a client is installed:
//
//	TACHYON_CONTRACT_ACTIONS_CACHE  a checkout of actions/cache at a release
//	TACHYON_CONTRACT_ACTIONS_CACHE_REF  that release's tag, for the report
//	TACHYON_CONTRACT_TOOLKIT        a directory with `npm install @actions/cache`
//
// The service is started in-process (go test -tags contract), or is a
// deployed one given by TACHYON_CACHE_URL, TACHYON_CACHE_REPO and
// TACHYON_CACHE_TOKEN_{MAIN,PR1,PR2,FORK}, minted for the jobs in
// DefaultJobs("refs/heads/main").
//
// Results are appended to $GITHUB_STEP_SUMMARY when it is set.
func TestLive(t *testing.T) {
	actionDir := os.Getenv("TACHYON_CONTRACT_ACTIONS_CACHE")
	toolkitDir := os.Getenv("TACHYON_CONTRACT_TOOLKIT")
	if actionDir == "" && toolkitDir == "" {
		t.Skip("set TACHYON_CONTRACT_ACTIONS_CACHE and/or TACHYON_CONTRACT_TOOLKIT to run the cache contract suite")
	}
	target := liveTarget(t)
	runner := Exec{Node: os.Getenv("TACHYON_CONTRACT_NODE")}
	var clients []Client
	if actionDir != "" {
		clients = append(clients, &ActionClient{Dir: actionDir, Version: os.Getenv("TACHYON_CONTRACT_ACTIONS_CACHE_REF"), Runner: runner, Target: target})
	}
	if toolkitDir != "" {
		clients = append(clients, &ToolkitClient{Dir: toolkitDir, Runner: runner, Target: target})
	}
	run := time.Now().UTC().Format("20060102T150405")
	for i, c := range clients {
		t.Run(c.Name(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			r := Run(ctx, c, target, Config{KeyPrefix: fmt.Sprintf("contract-%s-%d", run, i), TempDir: t.TempDir()})
			summary(t, r.Markdown())
			for _, ch := range r.Checks {
				if ch.Err != nil {
					t.Errorf("%s: %v", ch.Name, ch.Err)
				} else {
					t.Logf("%s: pass", ch.Name)
				}
			}
		})
	}
}

func liveTarget(t *testing.T) Target {
	t.Helper()
	target := Target{Repo: "tachyon-contract/cache", DefaultBranch: "refs/heads/main", Jobs: DefaultJobs("refs/heads/main")}
	url := os.Getenv("TACHYON_CACHE_URL")
	if url == "" {
		if startService == nil {
			t.Fatal("no TACHYON_CACHE_URL, and this build has no in-process cache service: run with -tags contract")
		}
		return startService(t, target)
	}
	target.URL = url
	if r := os.Getenv("TACHYON_CACHE_REPO"); r != "" {
		target.Repo = r
	}
	for _, j := range target.Jobs.All() {
		name := "TACHYON_CACHE_TOKEN_" + strings.ToUpper(j.Name)
		if j.Token = os.Getenv(name); j.Token == "" {
			t.Fatalf("TACHYON_CACHE_URL is set but %s is not", name)
		}
	}
	return target
}

func summary(t *testing.T, md string) {
	p := os.Getenv("GITHUB_STEP_SUMMARY")
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Logf("step summary: %v", err)
		return
	}
	defer f.Close()
	fmt.Fprintln(f, md)
}
