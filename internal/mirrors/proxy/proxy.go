// Package proxy is the pull-through package mirror that runs in each region
// (cmd/tachyon-mirror): a registry proxy for Docker Hub, GHCR and Quay, npm,
// PyPI, Go module, Maven and apt caches, and (in package gitmirror) the git
// mirror. Each region runs two instances; the control plane health-checks
// them (mirrors.Monitor) and points guests at upstream if both are down.
//
// Only public content is mirrored. The mirror fetches upstream anonymously,
// and any request carrying credentials passes straight through to upstream
// with those credentials and is never cached, so one tenant's private
// packages are never served to another.
package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Policy says whether and how long a response may be cached.
type Policy int

const (
	// NoStore responses are relayed, never cached.
	NoStore Policy = iota
	// Immutable responses (content-addressed or versioned artifacts) are
	// cached forever.
	Immutable
	// Mutable responses (indexes, tags, metadata) are cached for the
	// upstream's TTL and served stale only when upstream is unreachable.
	Mutable
)

// FetchFunc performs one upstream request. The registry uses it to add
// anonymous bearer tokens.
type FetchFunc func(*http.Request) (*http.Response, error)

// Upstream is one mirrored upstream.
type Upstream struct {
	Name string
	// Base is the upstream URL that request paths are appended to.
	Base string
	// Classify returns the cache policy for an upstream path.
	Classify func(path string) Policy
	// TTL bounds how long Mutable responses are served from cache.
	TTL time.Duration
	// VaryAccept forwards the client's Accept header and keys the cache on
	// it, for upstreams that negotiate content (npm, PyPI, registries).
	VaryAccept bool
	// Rewrite, if set, rewrites a cacheable body before it is served (not
	// before it is stored), e.g. to point absolute download URLs at the
	// mirror. mirror is this mirror's external base URL.
	Rewrite func(body []byte, mirror string) []byte
	// Fetch overrides the default HTTP client.
	Fetch FetchFunc
}

// Proxy serves mirrored upstreams from a Store.
type Proxy struct {
	Store  *Store
	Client *http.Client
	Log    *slog.Logger
	Now    func() time.Time
	// PublicURL is the mirror's external base URL; if empty it is derived
	// from each request.
	PublicURL string
}

func (p *Proxy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Proxy) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (p *Proxy) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

// mirrorBase is the URL guests reach this mirror at.
func (p *Proxy) mirrorBase(r *http.Request) string {
	if p.PublicURL != "" {
		return strings.TrimSuffix(p.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// hasCredentials reports whether the client sent its own credentials, which
// marks the request as private: pass it through, never cache it.
func hasCredentials(r *http.Request) bool {
	return r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != ""
}

// forwardHeaders are client request headers passed to upstream.
var forwardHeaders = []string{"Accept", "User-Agent", "Npm-Command", "Content-Type"}

// relayHeaders are upstream response headers passed to the client.
var relayHeaders = []string{
	"Content-Type", "ETag", "Last-Modified", "Location",
	"Docker-Content-Digest", "Docker-Distribution-Api-Version",
	"WWW-Authenticate", "Cache-Control",
}

func copyHeaders(dst, src http.Header, names []string) {
	for _, h := range names {
		if v := src.Values(h); len(v) > 0 {
			dst[http.CanonicalHeaderKey(h)] = append([]string(nil), v...)
		}
	}
}

// Serve proxies r to upstream path upath (escaped, starting with "/") plus
// rawQuery.
func (p *Proxy) Serve(w http.ResponseWriter, r *http.Request, up *Upstream, upath, rawQuery string) {
	target := strings.TrimSuffix(up.Base, "/") + upath
	if rawQuery != "" {
		target += "?" + rawQuery
	}

	if hasCredentials(r) || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		p.passThrough(w, r, up, target)
		return
	}

	policy := NoStore
	if up.Classify != nil {
		policy = up.Classify(upath)
	}
	key := up.Name + " " + target
	if up.VaryAccept {
		key += " accept=" + r.Header.Get("Accept")
	}

	var stale *Meta
	if policy != NoStore {
		if m, body, ok := p.Store.Get(key); ok {
			fresh := policy == Immutable || p.now().Sub(m.Stored) < up.TTL
			if fresh {
				defer body.Close()
				p.serveCached(w, r, up, m, body, "hit")
				return
			}
			body.Close()
			stale = &m
		}
	}

	req, err := p.newUpstreamRequest(r.Context(), http.MethodGet, target, r, up.VaryAccept)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodHead && policy == NoStore {
		req.Method = http.MethodHead
	}
	resp, err := p.fetch(up, req)
	if err != nil {
		// Upstream unreachable: a stale index beats failing the job.
		if stale != nil {
			if m, body, ok := p.Store.Get(key); ok {
				defer body.Close()
				p.serveCached(w, r, up, m, body, "stale")
				return
			}
		}
		p.log().Warn("upstream fetch failed", "upstream", up.Name, "url", target, "err", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	cacheable := policy != NoStore && resp.StatusCode == http.StatusOK && storable(resp.Header)
	if !cacheable {
		copyHeaders(w.Header(), resp.Header, relayHeaders)
		if resp.ContentLength >= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
		}
		w.Header().Set("X-Tachyon-Cache", "bypass")
		w.WriteHeader(resp.StatusCode)
		if r.Method != http.MethodHead {
			io.Copy(w, resp.Body)
		}
		return
	}

	sw, err := p.Store.Put(key, Meta{Header: resp.Header, Stored: p.now()})
	if err != nil {
		p.log().Warn("cache write failed", "err", err)
	}
	if up.Rewrite != nil {
		// Rewritten bodies are indexes: small enough to buffer.
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			if sw != nil {
				sw.Abort()
			}
			http.Error(w, "upstream read failed", http.StatusBadGateway)
			return
		}
		if sw != nil {
			if _, err := sw.Write(body); err == nil {
				sw.Commit()
			} else {
				sw.Abort()
			}
		}
		p.writeBody(w, r, resp.Header, up.Rewrite(body, p.mirrorBase(r)), "miss")
		return
	}

	copyHeaders(w.Header(), resp.Header, keptHeaders)
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.Header().Set("X-Tachyon-Cache", "miss")
	w.WriteHeader(http.StatusOK)
	var dst io.Writer = w
	if r.Method == http.MethodHead {
		dst = io.Discard
	}
	if sw == nil {
		io.Copy(dst, resp.Body)
		return
	}
	n, err := io.Copy(io.MultiWriter(sw, clientWriter{dst}), resp.Body)
	if err != nil || (resp.ContentLength >= 0 && n != resp.ContentLength) {
		sw.Abort()
		return
	}
	if err := sw.Commit(); err != nil {
		p.log().Warn("cache commit failed", "err", err)
	}
}

// clientWriter swallows client write errors so a disconnecting client
// doesn't abort filling the cache for the next one.
type clientWriter struct{ w io.Writer }

func (c clientWriter) Write(b []byte) (int, error) {
	c.w.Write(b)
	return len(b), nil
}

// storable is false when upstream marks a response private.
func storable(h http.Header) bool {
	cc := strings.ToLower(h.Get("Cache-Control"))
	return !strings.Contains(cc, "private") && !strings.Contains(cc, "no-store")
}

func (p *Proxy) serveCached(w http.ResponseWriter, r *http.Request, up *Upstream, m Meta, body io.Reader, state string) {
	if up.Rewrite != nil {
		b, err := io.ReadAll(body)
		if err != nil {
			http.Error(w, "cache read failed", http.StatusInternalServerError)
			return
		}
		p.writeBody(w, r, m.Header, up.Rewrite(b, p.mirrorBase(r)), state)
		return
	}
	copyHeaders(w.Header(), m.Header, keptHeaders)
	w.Header().Set("Content-Length", strconv.FormatInt(m.Size, 10))
	w.Header().Set("X-Tachyon-Cache", state)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		io.Copy(w, body)
	}
}

func (p *Proxy) writeBody(w http.ResponseWriter, r *http.Request, h http.Header, body []byte, state string) {
	copyHeaders(w.Header(), h, keptHeaders)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("X-Tachyon-Cache", state)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

func (p *Proxy) newUpstreamRequest(ctx context.Context, method, target string, r *http.Request, accept bool) (*http.Request, error) {
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("bad upstream url %q", target)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}
	for _, h := range forwardHeaders {
		if h == "Accept" && !accept {
			continue
		}
		if v := r.Header.Values(h); len(v) > 0 {
			req.Header[h] = v
		}
	}
	return req, nil
}

func (p *Proxy) fetch(up *Upstream, req *http.Request) (*http.Response, error) {
	if up.Fetch != nil {
		return up.Fetch(req)
	}
	return p.client().Do(req)
}

// passThrough relays a private or non-GET request untouched, with the
// client's own credentials, and caches nothing.
func (p *Proxy) passThrough(w http.ResponseWriter, r *http.Request, up *Upstream, target string) {
	var body io.Reader
	if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
		body = r.Body
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.ContentLength = r.ContentLength
	for k, v := range r.Header {
		switch http.CanonicalHeaderKey(k) {
		case "Host", "Connection", "Proxy-Authorization", "Accept-Encoding":
			continue
		}
		req.Header[k] = v
	}
	// The client's own credentials go to upstream as-is, so don't let the
	// registry token helper (or anything else) substitute its own.
	resp, err := p.client().Do(req)
	if err != nil {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header, relayHeaders)
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	w.Header().Set("X-Tachyon-Cache", "pass")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// replaceAll is a Rewrite helper: replaces each upstream prefix with the
// mirror base plus a path.
func replaceAll(body []byte, pairs ...string) []byte {
	for i := 0; i+1 < len(pairs); i += 2 {
		body = bytes.ReplaceAll(body, []byte(pairs[i]), []byte(pairs[i+1]))
	}
	return body
}
