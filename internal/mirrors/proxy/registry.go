package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/decover-tech/tachyon/internal/mirrors"
)

// Registry is a pull-through proxy for public container images. It fetches
// upstream anonymously; an image upstream won't serve anonymously is
// private, and the mirror answers 404 so the client falls back to the
// upstream registry with its own credentials. Private images therefore
// never enter the cache.
type Registry struct {
	Proxy *Proxy
	// Upstreams maps a registry host (the ?ns= that containerd mirrors
	// send; Docker's registry-mirrors sends none, meaning docker.io) to its
	// API base. Default mirrors.MirroredRegistries.
	Upstreams map[string]string
	// TagTTL is how long a tag→manifest lookup is cached.
	TagTTL time.Duration

	once    sync.Once
	tokens  *tokenCache
	streams map[string]*Upstream
}

var registryPath = regexp.MustCompile(`^/v2/(.+)/(manifests|blobs)/([^/]+)$`)

func (g *Registry) init() {
	g.once.Do(func() {
		ups := g.Upstreams
		if ups == nil {
			ups = mirrors.MirroredRegistries
		}
		ttl := g.TagTTL
		if ttl == 0 {
			ttl = time.Minute
		}
		g.tokens = &tokenCache{client: g.Proxy.client(), now: g.Proxy.now, m: map[string]token{}}
		g.streams = map[string]*Upstream{}
		for host, base := range ups {
			g.streams[host] = &Upstream{
				Name:       "registry/" + host,
				Base:       base,
				TTL:        ttl,
				VaryAccept: true, // manifest media type negotiation
				Classify:   registryPolicy,
				Fetch:      g.tokens.fetch,
			}
		}
	})
}

func registryPolicy(p string) Policy {
	m := registryPath.FindStringSubmatch(p)
	if m == nil {
		return NoStore
	}
	if m[2] == "blobs" || strings.HasPrefix(m[3], "sha256:") {
		return Immutable
	}
	return Mutable
}

func (g *Registry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.init()
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		// Pull-only. Pushes go to the real registry.
		http.Error(w, "mirror is pull-only", http.StatusMethodNotAllowed)
		return
	}
	m := registryPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		registryError(w, http.StatusNotFound, "NAME_UNKNOWN")
		return
	}
	ns := r.URL.Query().Get("ns")
	if ns == "" {
		ns = "docker.io"
	}
	up, ok := g.streams[ns]
	if !ok {
		// Not a registry we mirror (e.g. a private one): let the client
		// fall back to it directly.
		registryError(w, http.StatusNotFound, "NAME_UNKNOWN")
		return
	}
	if hasCredentials(r) {
		// Credentials for the mirror mean nothing upstream; never relay
		// or cache a credentialed pull. The client falls back.
		registryError(w, http.StatusNotFound, "NAME_UNKNOWN")
		return
	}
	g.Proxy.Serve(w, r, up, r.URL.EscapedPath(), "")
}

func registryError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code}}})
}

type token struct {
	value   string
	expires time.Time
}

// tokenCache gets anonymous pull tokens for the registry token protocol.
type tokenCache struct {
	client *http.Client
	now    func() time.Time
	mu     sync.Mutex
	m      map[string]token // host + scope
}

func repoName(p string) string {
	if m := registryPath.FindStringSubmatch(p); m != nil {
		return m[1]
	}
	return ""
}

// fetch performs req, answering a Bearer challenge with an anonymous token.
// A response upstream still refuses is reported as 404: the image is
// private, and must not be served (or cached) by the mirror.
func (t *tokenCache) fetch(req *http.Request) (*http.Response, error) {
	scope := "repository:" + repoName(req.URL.Path) + ":pull"
	key := req.URL.Host + " " + scope
	if tok, ok := t.get(key); ok {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		ch := parseChallenge(resp.Header.Get("WWW-Authenticate"))
		resp.Body.Close()
		if ch["realm"] == "" {
			return notFound(req), nil
		}
		if ch["scope"] == "" {
			ch["scope"] = scope
		}
		tok, ttl, err := t.anonymous(req, ch)
		if err != nil {
			return notFound(req), nil
		}
		t.put(key, tok, ttl)
		retry := req.Clone(req.Context())
		retry.Header.Set("Authorization", "Bearer "+tok)
		resp, err = t.client.Do(retry)
		if err != nil {
			return nil, err
		}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		resp.Body.Close()
		return notFound(req), nil
	}
	return resp, nil
}

func notFound(req *http.Request) *http.Response {
	body := `{"errors":[{"code":"NAME_UNKNOWN"}]}`
	return &http.Response{
		StatusCode:    http.StatusNotFound,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func (t *tokenCache) get(key string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tok, ok := t.m[key]
	if !ok || !t.now().Before(tok.expires) {
		return "", false
	}
	return tok.value, true
}

func (t *tokenCache) put(key, value string, ttl time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m[key] = token{value, t.now().Add(ttl)}
}

func (t *tokenCache) anonymous(req *http.Request, ch map[string]string) (string, time.Duration, error) {
	u, err := url.Parse(ch["realm"])
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", 0, errBadRealm
	}
	q := u.Query()
	if ch["service"] != "" {
		q.Set("service", ch["service"])
	}
	q.Set("scope", ch["scope"])
	u.RawQuery = q.Encode()
	treq, err := http.NewRequestWithContext(req.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := t.client.Do(treq)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, errBadRealm
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", 0, err
	}
	tok := body.Token
	if tok == "" {
		tok = body.AccessToken
	}
	if tok == "" {
		return "", 0, errBadRealm
	}
	ttl := time.Duration(body.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	// Renew a little early.
	return tok, ttl - ttl/10, nil
}

type tokenErr string

func (e tokenErr) Error() string { return string(e) }

const errBadRealm = tokenErr("registry: no anonymous token")

// parseChallenge parses `Bearer realm="...",service="...",scope="..."`.
func parseChallenge(h string) map[string]string {
	out := map[string]string{}
	scheme, rest, ok := strings.Cut(strings.TrimSpace(h), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return out
	}
	for rest != "" {
		rest = strings.TrimLeft(rest, " ,")
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if strings.HasPrefix(v, `"`) {
			end := strings.Index(v[1:], `"`)
			if end < 0 {
				break
			}
			out[k] = v[1 : end+1]
			rest = v[end+2:]
		} else {
			val, r, _ := strings.Cut(v, ",")
			out[k] = strings.TrimSpace(val)
			rest = r
		}
	}
	return out
}
