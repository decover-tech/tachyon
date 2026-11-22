package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// upstream is a fake package registry that counts requests per path.
type upstream struct {
	*httptest.Server
	mu    sync.Mutex
	hits  map[string]int
	auths []string
}

func newUpstream(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *upstream {
	u := &upstream{hits: map[string]int{}}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits[r.URL.EscapedPath()]++
		u.auths = append(u.auths, r.Header.Get("Authorization"))
		u.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) count(p string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[p]
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newMirror(t *testing.T, cfg Config) (*httptest.Server, *clock) {
	t.Helper()
	c := &clock{time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)}
	if cfg.Store == nil {
		cfg.Store = &Store{Dir: t.TempDir()}
	}
	cfg.Now = c.now
	srv := httptest.NewServer(New(cfg))
	t.Cleanup(srv.Close)
	return srv, c
}

func get(t *testing.T, rawURL string, hdr ...string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, rawURL, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestNPM(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			fmt.Fprintf(w, "audit:%s", b)
		case strings.HasSuffix(r.URL.Path, ".tgz"):
			w.Write([]byte("tarball"))
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"name":%q,"accept":%q}`, r.URL.EscapedPath(), r.Header.Get("Accept"))
		}
	})
	srv, clk := newMirror(t, Config{NPM: up.URL})

	// Packument: cached, keyed on Accept, refetched after the TTL.
	resp, body := get(t, srv.URL+"/npm/@acme%2fwidgets", "Accept", "application/vnd.npm.install-v1+json")
	if resp.StatusCode != 200 || !strings.Contains(body, `"/@acme%2fwidgets"`) || resp.Header.Get("X-Tachyon-Cache") != "miss" {
		t.Fatalf("packument: %d %s %v", resp.StatusCode, body, resp.Header)
	}
	resp, _ = get(t, srv.URL+"/npm/@acme%2fwidgets", "Accept", "application/vnd.npm.install-v1+json")
	if resp.Header.Get("X-Tachyon-Cache") != "hit" || up.count("/@acme%2fwidgets") != 1 {
		t.Fatalf("second packument fetch not a hit: %v, upstream hits %d", resp.Header, up.count("/@acme%2fwidgets"))
	}
	get(t, srv.URL+"/npm/@acme%2fwidgets", "Accept", "application/json")
	if up.count("/@acme%2fwidgets") != 2 {
		t.Error("different Accept should be a separate cache entry")
	}
	clk.t = clk.t.Add(6 * time.Minute)
	get(t, srv.URL+"/npm/@acme%2fwidgets", "Accept", "application/vnd.npm.install-v1+json")
	if up.count("/@acme%2fwidgets") != 3 {
		t.Error("packument not refetched after TTL")
	}

	// Tarballs are immutable.
	clk.t = clk.t.Add(1000 * time.Hour)
	for range 2 {
		if _, b := get(t, srv.URL+"/npm/left-pad/-/left-pad-1.3.0.tgz"); b != "tarball" {
			t.Fatalf("tarball body %q", b)
		}
	}
	if n := up.count("/left-pad/-/left-pad-1.3.0.tgz"); n != 1 {
		t.Errorf("tarball fetched upstream %d times", n)
	}

	// Credentialed requests are private: passed through with the
	// customer's credentials and never cached.
	for range 2 {
		resp, _ = get(t, srv.URL+"/npm/@private%2fpkg", "Authorization", "Bearer npm_secret")
		if resp.Header.Get("X-Tachyon-Cache") != "pass" {
			t.Errorf("credentialed request cache state %q", resp.Header.Get("X-Tachyon-Cache"))
		}
	}
	if up.count("/@private%2fpkg") != 2 {
		t.Error("credentialed request was cached")
	}
	if !contains(up.auths, "Bearer npm_secret") {
		t.Error("client credentials not passed upstream")
	}
	if _, b := get(t, srv.URL+"/npm/left-pad"); b == "" {
		t.Fatal("empty")
	}
	if contains(up.auths[len(up.auths)-1:], "Bearer npm_secret") {
		t.Error("credentials leaked into anonymous fetch")
	}

	// POST (npm audit) passes through.
	resp2, err := http.Post(srv.URL+"/npm/-/npm/v1/security/audits/quick", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if string(b) != "audit:{}" {
		t.Errorf("audit = %q", b)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestPyPIRewritesFileLinks(t *testing.T) {
	var files *upstream
	index := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<a href="%s/packages/ab/cd/requests-2.0.whl#sha256=x">requests</a>`, files.URL)
	})
	files = newUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("wheel")) })
	srv, _ := newMirror(t, Config{PyPI: index.URL, PyPIFiles: files.URL})

	for _, want := range []string{"miss", "hit"} {
		resp, body := get(t, srv.URL+"/pypi/simple/requests/")
		if resp.Header.Get("X-Tachyon-Cache") != want {
			t.Errorf("index cache = %q, want %q", resp.Header.Get("X-Tachyon-Cache"), want)
		}
		wantLink := srv.URL + "/pypi/files/packages/ab/cd/requests-2.0.whl"
		if !strings.Contains(body, wantLink) || strings.Contains(body, files.URL) {
			t.Fatalf("index not rewritten: %s", body)
		}
	}
	for range 2 {
		if _, b := get(t, srv.URL+"/pypi/files/packages/ab/cd/requests-2.0.whl"); b != "wheel" {
			t.Fatalf("file = %q", b)
		}
	}
	if files.count("/packages/ab/cd/requests-2.0.whl") != 1 {
		t.Error("wheel not cached")
	}
}

func TestGoProxy(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "missing") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Write([]byte(r.URL.Path))
	})
	srv, clk := newMirror(t, Config{Go: up.URL})
	paths := []string{"/golang.org/x/text/@v/list", "/golang.org/x/text/@v/v0.22.0.zip", "/example.com/missing/@v/v1.0.0.info"}
	for range 2 {
		for _, p := range paths {
			get(t, srv.URL+"/go"+p)
		}
	}
	clk.t = clk.t.Add(2 * time.Minute)
	for _, p := range paths {
		get(t, srv.URL+"/go"+p)
	}
	if n := up.count(paths[0]); n != 2 {
		t.Errorf("@v/list fetched %d times, want 2 (cached, then expired)", n)
	}
	if n := up.count(paths[1]); n != 1 {
		t.Errorf("zip fetched %d times, want 1", n)
	}
	if n := up.count(paths[2]); n != 3 {
		t.Errorf("404 fetched %d times, want 3 (never cached)", n)
	}
	if resp, _ := get(t, srv.URL+"/go"+paths[2]); resp.StatusCode != 404 {
		t.Errorf("404 relayed as %d", resp.StatusCode)
	}
}

func TestPolicies(t *testing.T) {
	m := Maven("x")
	for p, want := range map[string]Policy{
		"/org/foo/bar/1.0/bar-1.0.jar":                   Immutable,
		"/org/foo/bar/maven-metadata.xml":                Mutable,
		"/org/foo/bar/maven-metadata.xml.sha1":           Mutable,
		"/org/foo/bar/1.0-SNAPSHOT/bar-1.0-20260101.jar": Mutable,
		"/org/foo/bar/":                                  NoStore,
	} {
		if got := m.Classify(p); got != want {
			t.Errorf("maven %s = %v, want %v", p, got, want)
		}
	}
	for p, want := range map[string]Policy{
		"/ubuntu/pool/main/c/curl/curl_8.5.0_amd64.deb":                Immutable,
		"/ubuntu/dists/noble/InRelease":                                Mutable,
		"/ubuntu/dists/noble/main/binary-amd64/by-hash/SHA256/abc1234": Immutable,
		"/": NoStore,
	} {
		if got := AptPolicy(p); got != want {
			t.Errorf("apt %s = %v, want %v", p, got, want)
		}
	}
	for p, want := range map[string]Policy{
		"/v2/library/alpine/manifests/3.20":        Mutable,
		"/v2/library/alpine/manifests/sha256:abcd": Immutable,
		"/v2/library/alpine/blobs/sha256:abcd":     Immutable,
		"/v2/library/alpine/tags/list":             NoStore,
	} {
		if got := registryPolicy(p); got != want {
			t.Errorf("registry %s = %v, want %v", p, got, want)
		}
	}
}

func TestStaleOnUpstreamFailureAndPrivateResponses(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "private") {
			w.Header().Set("Cache-Control", "private")
		}
		w.Write([]byte("v1"))
	})
	srv, clk := newMirror(t, Config{NPM: up.URL})
	get(t, srv.URL+"/npm/pkg")
	for range 2 {
		get(t, srv.URL+"/npm/private-pkg")
	}
	if up.count("/private-pkg") != 2 {
		t.Error("Cache-Control: private response was cached")
	}

	up.Close()
	clk.t = clk.t.Add(time.Hour)
	resp, body := get(t, srv.URL+"/npm/pkg")
	if resp.StatusCode != 200 || body != "v1" || resp.Header.Get("X-Tachyon-Cache") != "stale" {
		t.Errorf("stale fallback: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if resp, _ := get(t, srv.URL+"/npm/never-seen"); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("uncached + upstream down = %d", resp.StatusCode)
	}
}

// fakeRegistry implements the registry token flow: anonymous tokens for
// public repos, and manifests that refuse any other token.
func fakeRegistry(t *testing.T) *upstream {
	var reg *upstream
	reg = newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			scope := r.URL.Query().Get("scope")
			if r.URL.Query().Get("service") != "fake" {
				http.Error(w, "bad service", 400)
				return
			}
			fmt.Fprintf(w, `{"token":"anon:%s","expires_in":300}`, scope)
			return
		}
		repo := repoName(r.URL.Path)
		want := "Bearer anon:repository:" + repo + ":pull"
		if strings.HasPrefix(repo, "acme/private") || r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake"`, reg.URL))
			http.Error(w, `{"errors":[{"code":"UNAUTHORIZED"}]}`, http.StatusUnauthorized)
			return
		}
		if strings.Contains(r.URL.Path, "/manifests/") {
			w.Header().Set("Content-Type", r.Header.Get("Accept"))
			w.Header().Set("Docker-Content-Digest", "sha256:m")
		}
		w.Write([]byte("content:" + r.URL.Path))
	})
	return reg
}

func TestRegistry(t *testing.T) {
	hub, ghcr := fakeRegistry(t), fakeRegistry(t)
	srv, _ := newMirror(t, Config{Registries: map[string]string{"docker.io": hub.URL, "ghcr.io": ghcr.URL}})

	resp, _ := get(t, srv.URL+"/v2/")
	if resp.StatusCode != 200 || resp.Header.Get("Docker-Distribution-Api-Version") != "registry/2.0" {
		t.Fatalf("/v2/ = %d %v", resp.StatusCode, resp.Header)
	}

	const mt = "application/vnd.oci.image.index.v1+json"
	for range 2 {
		resp, body := get(t, srv.URL+"/v2/library/alpine/manifests/3.20", "Accept", mt)
		if resp.StatusCode != 200 || body != "content:/v2/library/alpine/manifests/3.20" {
			t.Fatalf("manifest = %d %q", resp.StatusCode, body)
		}
		if resp.Header.Get("Content-Type") != mt || resp.Header.Get("Docker-Content-Digest") != "sha256:m" {
			t.Errorf("manifest headers %v", resp.Header)
		}
	}
	if n := hub.count("/v2/library/alpine/manifests/3.20"); n != 2 {
		// One 401 challenge plus one authorized fetch; the second client
		// request is a cache hit.
		t.Errorf("upstream manifest requests = %d, want 2", n)
	}
	if n := hub.count("/token"); n != 1 {
		t.Errorf("token requests = %d, want 1", n)
	}

	// HEAD on a cached blob carries its length.
	get(t, srv.URL+"/v2/library/alpine/blobs/sha256:b")
	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/v2/library/alpine/blobs/sha256:b", nil)
	hresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	hresp.Body.Close()
	if hresp.ContentLength != int64(len("content:/v2/library/alpine/blobs/sha256:b")) {
		t.Errorf("HEAD length = %d", hresp.ContentLength)
	}

	// containerd sends ?ns= for non-Docker Hub registries.
	if _, body := get(t, srv.URL+"/v2/owner/app/blobs/sha256:c?ns=ghcr.io"); body != "content:/v2/owner/app/blobs/sha256:c" {
		t.Errorf("ghcr blob = %q", body)
	}
	if ghcr.count("/v2/owner/app/blobs/sha256:c") == 0 || hub.count("/v2/owner/app/blobs/sha256:c") != 0 {
		t.Error("ns=ghcr.io not routed to ghcr")
	}

	// Private images, unknown registries and credentialed pulls all 404 so
	// the client goes to the real registry with its own credentials.
	for _, c := range []struct {
		url  string
		hdr  []string
		desc string
	}{
		{"/v2/acme/private-app/manifests/latest", nil, "private image"},
		{"/v2/acme/app/manifests/latest?ns=registry.acme.internal", nil, "private registry"},
		{"/v2/library/alpine/manifests/3.20", []string{"Authorization", "Basic dXNlcjpwYXNz"}, "credentialed pull"},
	} {
		if resp, _ := get(t, srv.URL+c.url, c.hdr...); resp.StatusCode != 404 {
			t.Errorf("%s = %d, want 404", c.desc, resp.StatusCode)
		}
	}
	get(t, srv.URL+"/v2/acme/private-app/manifests/latest")
	if n := hub.count("/v2/acme/private-app/manifests/latest"); n != 4 {
		t.Errorf("private manifest upstream requests = %d, want 4 (never cached)", n)
	}

	put, _ := http.NewRequest(http.MethodPut, srv.URL+"/v2/library/alpine/manifests/x", nil)
	presp, err := http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	presp.Body.Close()
	if presp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT = %d", presp.StatusCode)
	}
}

func TestParseChallenge(t *testing.T) {
	got := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull"`)
	if got["realm"] != "https://auth.docker.io/token" || got["service"] != "registry.docker.io" || got["scope"] != "repository:library/alpine:pull" {
		t.Errorf("parseChallenge = %v", got)
	}
	if len(parseChallenge(`Basic realm="x"`)) != 0 {
		t.Error("Basic challenge parsed as Bearer")
	}
}

func TestAptProxy(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("deb")) })
	srv, _ := newMirror(t, Config{AptUpstream: func(string) string { return up.URL }})
	proxyURL, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	fetch := func(u string) (int, string) {
		resp, err := client.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for range 2 {
		if code, b := fetch("http://archive.ubuntu.com/ubuntu/pool/main/c/curl/curl_8.5.0_amd64.deb"); code != 200 || b != "deb" {
			t.Fatalf("deb = %d %q", code, b)
		}
	}
	if code, _ := fetch("http://us.archive.ubuntu.com/ubuntu/pool/main/c/curl/curl_8.5.0_amd64.deb"); code != 200 {
		t.Errorf("regional archive = %d", code)
	}
	if n := up.count("/ubuntu/pool/main/c/curl/curl_8.5.0_amd64.deb"); n != 2 {
		t.Errorf("upstream fetches = %d, want 2 (one per host)", n)
	}
	for _, u := range []string{
		"http://apt.acme.internal/pool/secret.deb", // private repo
		"http://169.254.169.254/latest/meta-data/", // not an open proxy
		"http://archive.ubuntu.com:8080/ubuntu/pool/x.deb",
	} {
		if code, _ := fetch(u); code != http.StatusForbidden {
			t.Errorf("%s = %d, want 403", u, code)
		}
	}
}

func TestHealthz(t *testing.T) {
	dir := t.TempDir()
	srv, _ := newMirror(t, Config{Store: &Store{Dir: dir}})
	if resp, _ := get(t, srv.URL+"/healthz"); resp.StatusCode != 200 {
		t.Errorf("healthz = %d", resp.StatusCode)
	}
	// A cache dir that can't be created makes the instance unhealthy.
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o644)
	bad, _ := newMirror(t, Config{Store: &Store{Dir: filepath.Join(blocker, "cache")}})
	if resp, _ := get(t, bad.URL+"/healthz"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("unwritable healthz = %d", resp.StatusCode)
	}
}

func TestStorePrune(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	put := func(key string, size int) {
		w, err := s.Put(key, Meta{Stored: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(strings.Repeat("x", size)))
		if err := w.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	put("old", 1000)
	put("new", 1000)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(s.path("old"), old, old)
	if _, body, ok := s.Get("new"); ok {
		r := bufio.NewReader(body)
		b, _ := io.ReadAll(r)
		body.Close()
		if len(b) != 1000 {
			t.Fatalf("body len %d", len(b))
		}
	} else {
		t.Fatal("missing entry")
	}
	n, err := s.Prune(1500)
	if err != nil || n != 1 {
		t.Fatalf("Prune = %d, %v", n, err)
	}
	if _, _, ok := s.Get("old"); ok {
		t.Error("LRU entry survived prune")
	}
	if _, b, ok := s.Get("new"); !ok {
		t.Error("recent entry pruned")
	} else {
		b.Close()
	}

	// Aborted writes leave nothing behind.
	w, _ := s.Put("aborted", Meta{})
	w.Write([]byte("partial"))
	w.Abort()
	if _, _, ok := s.Get("aborted"); ok {
		t.Error("aborted entry visible")
	}
}
