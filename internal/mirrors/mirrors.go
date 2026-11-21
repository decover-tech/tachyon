// Package mirrors points guests at the region's pull-through package
// mirrors, so tools use them without configuration. Private registries are
// never mirrored: their traffic passes straight through.
package mirrors

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Endpoints are the mirror URLs to use in a region. An empty field means use
// the upstream registry (no mirror, or both mirrors of the pair are down).
type Endpoints struct {
	Registry string // Docker Hub, GHCR, Quay registry proxy
	NPM      string
	PyPI     string // simple index URL
	GoProxy  string
	Maven    string
	Apt      string // http proxy for apt
	Git      string // per-region mirror base, e.g. https://git.r1.tachyon.internal/
}

// Pair is a health-checked mirror pair.
type Pair struct {
	Primary, Secondary string
	PrimaryUp          bool
	SecondaryUp        bool
}

// Pick returns the healthy endpoint, or "" to fall back to upstream.
func (p Pair) Pick() string {
	switch {
	case p.PrimaryUp:
		return p.Primary
	case p.SecondaryUp:
		return p.Secondary
	}
	return ""
}

// Env returns environment variables for the runner's environment.
func (e Endpoints) Env() map[string]string {
	env := map[string]string{}
	if e.NPM != "" {
		env["npm_config_registry"] = e.NPM
	}
	if e.PyPI != "" {
		env["PIP_INDEX_URL"] = e.PyPI
	}
	if e.GoProxy != "" {
		// Fall back to direct for anything the proxy can't serve.
		env["GOPROXY"] = e.GoProxy + ",direct"
	}
	return env
}

// DockerDaemonConfig merges registry-mirrors into a daemon.json document.
func (e Endpoints) DockerDaemonConfig(base map[string]any) ([]byte, error) {
	cfg := map[string]any{}
	for k, v := range base {
		cfg[k] = v
	}
	if e.Registry != "" {
		cfg["registry-mirrors"] = []string{e.Registry}
	} else {
		delete(cfg, "registry-mirrors")
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// PublicAptHosts are the public apt repositories the apt cache mirrors. A
// leading "." matches any subdomain (regional Ubuntu archives).
var PublicAptHosts = []string{
	"archive.ubuntu.com",
	"security.ubuntu.com",
	"ports.ubuntu.com",
	".archive.ubuntu.com",
	"deb.debian.org",
	"security.debian.org",
}

// AptConf is /etc/apt/apt.conf.d/01tachyon-proxy. The proxy is set per
// public host, so private and third-party apt repositories are reached
// directly and never go through the mirror.
func (e Endpoints) AptConf() string {
	if e.Apt == "" {
		return ""
	}
	var b strings.Builder
	for _, h := range PublicAptHosts {
		if strings.HasPrefix(h, ".") {
			continue // apt matches exact hosts only
		}
		fmt.Fprintf(&b, "Acquire::http::Proxy::%s %q;\n", h, e.Apt)
	}
	return b.String()
}

// EndpointsFor returns the endpoints of one tachyon-mirror instance at base
// (cmd/tachyon-mirror serves every ecosystem on one listener), or the zero
// Endpoints, meaning upstream, if base is empty.
func EndpointsFor(base string) Endpoints {
	if base == "" {
		return Endpoints{}
	}
	b := strings.TrimSuffix(base, "/")
	return Endpoints{
		Registry: b,
		NPM:      b + "/npm/",
		PyPI:     b + "/pypi/simple",
		GoProxy:  b + "/go",
		Maven:    b + "/maven",
		Apt:      b,
		Git:      b + "/git/",
	}
}

// MirroredRegistries maps the public registries the registry proxy serves
// to their API servers. Docker's registry-mirrors only covers Docker Hub;
// containerd (and BuildKit, nerdctl) use hosts.toml for the rest.
var MirroredRegistries = map[string]string{
	"docker.io": "https://registry-1.docker.io",
	"ghcr.io":   "https://ghcr.io",
	"quay.io":   "https://quay.io",
}

// ContainerdHosts returns hosts.toml files (path -> content) that send
// pulls from the public registries through the registry proxy. containerd
// adds ?ns=<registry> to mirror requests, which the proxy routes on, and
// falls back to the registry itself if the mirror fails. Registries not
// listed are private and get no entry, so they are reached directly.
func (e Endpoints) ContainerdHosts() map[string]string {
	if e.Registry == "" {
		return nil
	}
	out := map[string]string{}
	for r, server := range MirroredRegistries {
		out["/etc/containerd/certs.d/"+r+"/hosts.toml"] = fmt.Sprintf(
			"server = %q\n\n[host.%q]\n  capabilities = [\"pull\", \"resolve\"]\n",
			server, strings.TrimSuffix(e.Registry, "/"))
	}
	return out
}

// GradleInit is an init script that puts the mirror ahead of other repos.
func (e Endpoints) GradleInit() string {
	if e.Maven == "" {
		return ""
	}
	return fmt.Sprintf(`allprojects {
    buildscript { repositories { maven { url %[1]q } } }
    repositories { maven { url %[1]q } }
}
settingsEvaluated { s ->
    s.pluginManagement.repositories { maven { url %[1]q } }
}
`, e.Maven)
}

// MavenSettings is ~/.m2/settings.xml mirroring Maven Central only.
func (e Endpoints) MavenSettings() string {
	if e.Maven == "" {
		return ""
	}
	return fmt.Sprintf(`<settings>
  <mirrors>
    <mirror><id>tachyon</id><mirrorOf>central</mirrorOf><url>%s</url></mirror>
  </mirrors>
</settings>
`, e.Maven)
}

// GitConfig rewrites fetches of the given repos (owner/name on github.com)
// to the regional git mirror; pushInsteadOf keeps pushes going to GitHub.
// insteadOf is a prefix match, so the mirror must also serve (or redirect)
// any repo whose name extends a listed one.
func (e Endpoints) GitConfig(repos []string) string {
	if e.Git == "" || len(repos) == 0 {
		return ""
	}
	sorted := append([]string(nil), repos...)
	sort.Strings(sorted)
	var b strings.Builder
	base := strings.TrimSuffix(e.Git, "/")
	for _, r := range sorted {
		fmt.Fprintf(&b, "[url %q]\n\tinsteadOf = https://github.com/%s\n", base+"/"+r, r)
		fmt.Fprintf(&b, "[url \"https://github.com/%s\"]\n\tpushInsteadOf = https://github.com/%s\n", r, r)
	}
	return b.String()
}
