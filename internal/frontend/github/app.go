package github

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
)

// The Tachyon GitHub App's permissions and webhook events. This file is the
// single source of truth: the App manifest (`tachyon github-app manifest`),
// the installation-token scope and the endpoint allowlist below all derive
// from it, and tests fail if the REST client calls anything it does not cover.
//
// The App asks for only what the adapter needs:
//
//   - self-hosted runner management: GitHub gates repository-scoped runner
//     endpoints (generate-jitconfig, list, delete) behind the "administration"
//     repository permission; there is no narrower one. Runners must be
//     repository-scoped (see API.CreateJITConfig), so org-level runner
//     permissions would not do. The client calls only the runner endpoints in
//     Endpoints, and installation tokens are requested with exactly
//     AppPermissions.
//   - actions: read, to look up the workflow run behind a job and to receive
//     workflow_job events.
//   - contents: read, for prebuilds, branch protection lookups and push events.
//   - metadata: read, which GitHub grants every App and cannot be removed.
const (
	PermActions        = "actions"
	PermAdministration = "administration"
	PermContents       = "contents"
	PermMetadata       = "metadata"

	AccessRead  = "read"
	AccessWrite = "write"
)

// AppPermissions is the complete set of repository permissions the App
// requests. Nothing else: no organization or account permissions.
var AppPermissions = map[string]string{
	PermActions:        AccessRead,
	PermAdministration: AccessWrite,
	PermContents:       AccessRead,
	PermMetadata:       AccessRead,
}

// AppEvents are the webhook events the App subscribes to, with the
// permission each one requires. installation and installation_repositories
// events are delivered to every App without subscribing.
//
// pull_request is deliberately absent: it would need pull_requests:read.
// The adapter still handles it if delivered, but PR jobs run without it; they
// only lose the pre-fork hint (pushes to same-repo PR branches still send
// one).
var AppEvents = map[string]string{
	"workflow_job": PermActions,
	"push":         PermContents,
}

// Endpoint is one GitHub REST call the client makes and the installation
// permission it needs. Permission "" means the call is authenticated as the
// App itself (JWT), not as an installation.
type Endpoint struct {
	Method     string
	Path       string // segments in {braces} match any single segment
	Permission string
	Access     string
}

// Endpoints is every call Client makes. A test fails if Client calls anything
// not listed, and NeededPermissions must stay within AppPermissions.
var Endpoints = []Endpoint{
	{"POST", "/app/installations/{installation_id}/access_tokens", "", ""},
	{"GET", "/repos/{owner}/{repo}/actions/runs/{run_id}", PermActions, AccessRead},
	{"GET", "/repos/{owner}/{repo}/branches/{branch}", PermContents, AccessRead},
	{"POST", "/repos/{owner}/{repo}/actions/runners/generate-jitconfig", PermAdministration, AccessWrite},
	{"GET", "/repos/{owner}/{repo}/actions/runners", PermAdministration, AccessRead},
	{"DELETE", "/repos/{owner}/{repo}/actions/runners/{runner_id}", PermAdministration, AccessWrite},
}

// LookupEndpoint finds the Endpoint for a request. path is the escaped URL
// path (a branch name like release/1 arrives as release%2F1); any query is
// ignored.
func LookupEndpoint(method, path string) (Endpoint, bool) {
	path, _, _ = strings.Cut(path, "?")
	got := strings.Split(path, "/")
	for _, e := range Endpoints {
		if e.Method != method {
			continue
		}
		want := strings.Split(e.Path, "/")
		if len(want) != len(got) {
			continue
		}
		ok := true
		for i := range want {
			if strings.HasPrefix(want[i], "{") {
				if got[i] == "" {
					ok = false
					break
				}
				continue
			}
			if want[i] != got[i] {
				ok = false
				break
			}
		}
		if ok {
			return e, true
		}
	}
	return Endpoint{}, false
}

// NeededPermissions is the least set of permissions that covers Endpoints,
// AppEvents and the mandatory metadata permission.
func NeededPermissions() map[string]string {
	need := map[string]string{PermMetadata: AccessRead}
	add := func(perm, access string) {
		if perm == "" {
			return
		}
		if need[perm] != AccessWrite {
			need[perm] = access
		}
	}
	for _, e := range Endpoints {
		add(e.Permission, e.Access)
	}
	for _, perm := range AppEvents {
		add(perm, AccessRead)
	}
	// Prebuilds read repository contents at trusted commits.
	add(PermContents, AccessRead)
	return need
}

// CheckGranted returns an error if granted (the permissions on an
// installation or an installation token) reaches beyond AppPermissions.
// Fewer permissions are not an error here; GitHub rejects the token request
// when an installation has not accepted a permission the App asks for.
func CheckGranted(granted map[string]string) error {
	var extra []string
	for _, perm := range slices.Sorted(maps.Keys(granted)) {
		access := granted[perm]
		want, ok := AppPermissions[perm]
		if !ok || (access == AccessWrite && want != AccessWrite) {
			extra = append(extra, perm+":"+access)
		}
	}
	if len(extra) > 0 {
		return fmt.Errorf("github: token has permissions beyond the App's least-privilege set: %s", strings.Join(extra, ", "))
	}
	return nil
}

// Manifest is a GitHub App manifest, for registering the App with the
// manifest flow:
// https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest
type Manifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	Description        string            `json:"description,omitempty"`
	HookAttributes     HookAttributes    `json:"hook_attributes"`
	RedirectURL        string            `json:"redirect_url,omitempty"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events"`
}

type HookAttributes struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// ManifestOptions are the deployment-specific parts of the manifest.
type ManifestOptions struct {
	Name        string // App name; must be unique on GitHub
	Homepage    string
	WebhookURL  string // the control plane's POST /webhooks/github
	RedirectURL string // where GitHub sends the manifest code; optional
	Public      bool   // false: installable only by the owning account
}

// NewManifest builds the App manifest with exactly AppPermissions and
// AppEvents.
func NewManifest(o ManifestOptions) (Manifest, error) {
	if o.Name == "" {
		return Manifest{}, errors.New("github app manifest: name is required")
	}
	for field, raw := range map[string]string{"homepage": o.Homepage, "webhook URL": o.WebhookURL, "redirect URL": o.RedirectURL} {
		if raw == "" && field == "redirect URL" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return Manifest{}, fmt.Errorf("github app manifest: %s %q is not an http(s) URL", field, raw)
		}
		if field == "webhook URL" && u.Scheme != "https" {
			return Manifest{}, fmt.Errorf("github app manifest: webhook URL %q must be https", raw)
		}
	}
	return Manifest{
		Name:               o.Name,
		URL:                o.Homepage,
		Description:        "Tachyon CI runners: jobs fork from a prebuilt per-repo warm snapshot.",
		HookAttributes:     HookAttributes{URL: o.WebhookURL, Active: true},
		RedirectURL:        o.RedirectURL,
		Public:             o.Public,
		DefaultPermissions: maps.Clone(AppPermissions),
		DefaultEvents:      slices.Sorted(maps.Keys(AppEvents)),
	}, nil
}
