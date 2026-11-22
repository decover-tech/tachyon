package github

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// enforceLeastPrivilege fails the test for any request the client makes that
// is not in Endpoints, or whose permission the App does not hold.
func enforceLeastPrivilege(t *testing.T, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e, ok := LookupEndpoint(r.Method, r.URL.EscapedPath())
		switch {
		case !ok:
			t.Errorf("client called %s %s, which is not in Endpoints", r.Method, r.URL.EscapedPath())
		case e.Permission != "" && !covers(AppPermissions[e.Permission], e.Access):
			t.Errorf("%s %s needs %s:%s, App has %q", r.Method, r.URL.Path, e.Permission, e.Access, AppPermissions[e.Permission])
		}
		next.ServeHTTP(w, r)
	})
}

func covers(have, need string) bool {
	return have == AccessWrite || (have == AccessRead && need == AccessRead)
}

func TestAppPermissionsAreExactlyTheIssueSet(t *testing.T) {
	// Self-hosted runner management (administration, repo-scoped runners),
	// Actions read, contents read, and GitHub's mandatory metadata read.
	want := map[string]string{
		"actions":        "read",
		"administration": "write",
		"contents":       "read",
		"metadata":       "read",
	}
	if !maps.Equal(AppPermissions, want) {
		t.Errorf("AppPermissions = %v, want %v", AppPermissions, want)
	}
	if got := slices.Sorted(maps.Keys(AppEvents)); !slices.Equal(got, []string{"push", "workflow_job"}) {
		t.Errorf("AppEvents = %v", got)
	}
}

func TestAppPermissionsAreLeast(t *testing.T) {
	need := NeededPermissions()
	if !maps.Equal(need, AppPermissions) {
		t.Errorf("needed permissions %v differ from requested %v: drop what is unused or list the endpoint that needs it", need, AppPermissions)
	}
	for ev, perm := range AppEvents {
		if _, ok := AppPermissions[perm]; !ok {
			t.Errorf("event %s needs %s, which the App does not request", ev, perm)
		}
	}
}

func TestLookupEndpoint(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		perm         string
		ok           bool
	}{
		{"GET", "/repos/acme/api/actions/runs/7", PermActions, true},
		{"GET", "/repos/acme/api/branches/release%2F1", PermContents, true},
		{"GET", "/repos/acme/api/branches/release/1", "", false}, // unescaped slash is another path
		{"POST", "/repos/acme/api/actions/runners/generate-jitconfig", PermAdministration, true},
		{"GET", "/repos/acme/api/actions/runners?per_page=100&page=2", PermAdministration, true},
		{"DELETE", "/repos/acme/api/actions/runners/9", PermAdministration, true},
		{"POST", "/app/installations/42/access_tokens", "", true},
		{"PUT", "/repos/acme/api/actions/runners/9", "", false},
		{"PATCH", "/repos/acme/api", "", false},
		{"GET", "/repos/acme/api/contents/README.md", "", false},
		{"DELETE", "/repos/acme/api/actions/runners/", "", false},
	} {
		e, ok := LookupEndpoint(tc.method, tc.path)
		if ok != tc.ok || e.Permission != tc.perm {
			t.Errorf("%s %s = %+v, %v; want perm %q ok %v", tc.method, tc.path, e, ok, tc.perm, tc.ok)
		}
	}
}

func TestCheckGranted(t *testing.T) {
	if err := CheckGranted(AppPermissions); err != nil {
		t.Errorf("exact set: %v", err)
	}
	if err := CheckGranted(map[string]string{"actions": "read"}); err != nil {
		t.Errorf("subset: %v", err)
	}
	if err := CheckGranted(nil); err != nil {
		t.Errorf("empty: %v", err)
	}
	err := CheckGranted(map[string]string{"actions": "write", "contents": "read", "secrets": "read"})
	if err == nil || !strings.Contains(err.Error(), "actions:write") || !strings.Contains(err.Error(), "secrets:read") {
		t.Errorf("broader set: %v", err)
	}
}

func TestClientRejectsOverbroadToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/1/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"token": "t", "expires_at": time.Now().Add(time.Hour),
			"permissions": map[string]string{"contents": "write"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &Client{AppID: 1, PrivateKey: testKey(t), BaseURL: srv.URL}
	_, err := c.ListRunners(context.Background(), 1, "acme/api")
	if err == nil || !strings.Contains(err.Error(), "contents:write") {
		t.Errorf("ListRunners with overbroad token: %v", err)
	}
}

func TestNewManifest(t *testing.T) {
	m, err := NewManifest(ManifestOptions{
		Name:       "Tachyon",
		Homepage:   "https://tachyon.example",
		WebhookURL: "https://cp.tachyon.example/webhooks/github",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(m)
	var got map[string]any
	json.Unmarshal(raw, &got)
	perms := got["default_permissions"].(map[string]any)
	if len(perms) != len(AppPermissions) {
		t.Errorf("manifest permissions = %v", perms)
	}
	for k, v := range AppPermissions {
		if perms[k] != v {
			t.Errorf("manifest %s = %v, want %s", k, perms[k], v)
		}
	}
	if ev := got["default_events"].([]any); len(ev) != 2 || ev[0] != "push" || ev[1] != "workflow_job" {
		t.Errorf("manifest events = %v", ev)
	}
	hook := got["hook_attributes"].(map[string]any)
	if hook["url"] != "https://cp.tachyon.example/webhooks/github" || hook["active"] != true {
		t.Errorf("hook = %v", hook)
	}
	if got["public"] != false {
		t.Errorf("public = %v, want false by default", got["public"])
	}
	if _, ok := got["redirect_url"]; ok {
		t.Error("redirect_url should be omitted when unset")
	}

	// The manifest holds a copy: mutating it must not widen AppPermissions.
	m.DefaultPermissions["secrets"] = "write"
	if _, ok := AppPermissions["secrets"]; ok {
		t.Error("manifest shares AppPermissions map")
	}
}

func TestAdapterAllowedOwners(t *testing.T) {
	h := newHarness(t)
	h.a.AllowedOwners = []string{"pilot-org", "ACME"}

	// acme/api is allowed (case-insensitive).
	if code := h.post("workflow_job", jobEvent("queued", 1, tachyonLabels, ""), testSecret); code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	if len(h.core.subs) != 1 {
		t.Fatalf("allowed owner: %d submissions", len(h.core.subs))
	}

	// Another account's events are acknowledged but start no work.
	ev := jobEvent("queued", 2, tachyonLabels, "")
	ev["repository"] = map[string]any{"full_name": "stranger/api", "default_branch": "main"}
	if code := h.post("workflow_job", ev, testSecret); code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	push := map[string]any{
		"ref": "refs/heads/main", "after": "abc",
		"repository":   map[string]any{"full_name": "stranger/api", "default_branch": "main"},
		"installation": map[string]any{"id": 9},
	}
	h.post("push", push, testSecret)
	if len(h.core.subs) != 1 || len(h.api.created) != 1 || len(h.core.hints) != 0 {
		t.Errorf("disallowed owner did work: subs=%d created=%d hints=%d", len(h.core.subs), len(h.api.created), len(h.core.hints))
	}

	// Events without a repository (ping) are still accepted.
	if code := h.post("ping", map[string]any{"zen": "hi"}, testSecret); code != http.StatusAccepted {
		t.Errorf("ping status %d", code)
	}
}

func TestNewManifestValidates(t *testing.T) {
	ok := ManifestOptions{Name: "Tachyon", Homepage: "https://t.example", WebhookURL: "https://cp.example/webhooks/github"}
	for name, mut := range map[string]func(*ManifestOptions){
		"no name":         func(o *ManifestOptions) { o.Name = "" },
		"no homepage":     func(o *ManifestOptions) { o.Homepage = "" },
		"no webhook":      func(o *ManifestOptions) { o.WebhookURL = "" },
		"http webhook":    func(o *ManifestOptions) { o.WebhookURL = "http://cp.example/webhooks/github" },
		"relative hook":   func(o *ManifestOptions) { o.WebhookURL = "/webhooks/github" },
		"bad redirect":    func(o *ManifestOptions) { o.RedirectURL = "ftp://x" },
		"scheme-less url": func(o *ManifestOptions) { o.Homepage = "t.example" },
	} {
		o := ok
		mut(&o)
		if _, err := NewManifest(o); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := NewManifest(ok); err != nil {
		t.Errorf("valid options: %v", err)
	}
}
