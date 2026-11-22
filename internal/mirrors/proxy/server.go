package proxy

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/decover-tech/tachyon/internal/mirrors"
)

// Config configures a mirror server. Empty upstreams use the public ones.
type Config struct {
	Store     *Store
	Client    *http.Client
	Log       *slog.Logger
	PublicURL string // external base URL, e.g. https://mirror-a.r1.tachyon.internal
	Now       func() time.Time

	NPM, PyPI, PyPIFiles, Go, Maven string
	Registries                      map[string]string
	// AptHosts are the apt repository hosts the apt proxy will fetch from;
	// default mirrors.PublicAptHosts. A leading "." matches subdomains.
	AptHosts []string
	// AptUpstream maps an allowlisted apt host to the base URL fetched
	// from; default "http://<host>" (tests point it elsewhere).
	AptUpstream func(host string) string
	// Git serves the git mirror under /git/; nil disables it.
	Git http.Handler
}

func or(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// New returns the mirror's HTTP handler. One listener serves every
// ecosystem, matching mirrors.EndpointsFor:
//
//	/v2/...                      registry proxy (Docker Hub, GHCR, Quay)
//	/npm/...                     npm
//	/pypi/simple/, /pypi/files/  PyPI
//	/go/...                      Go module proxy
//	/maven/...                   Maven Central
//	/git/<owner>/<repo>/...      git mirror
//	absolute-form requests       apt (HTTP forward proxy)
//	/healthz                     health check for the control plane
func New(cfg Config) http.Handler {
	p := &Proxy{Store: cfg.Store, Client: cfg.Client, Log: cfg.Log, PublicURL: cfg.PublicURL, Now: cfg.Now}
	npm := NPM(or(cfg.NPM, NPMUpstream))
	filesBase := or(cfg.PyPIFiles, PyPIFilesUpstream)
	pypi := PyPISimple(or(cfg.PyPI, PyPIUpstream), filesBase)
	files := PyPIFiles(filesBase)
	gop := GoProxy(or(cfg.Go, GoUpstream))
	maven := Maven(or(cfg.Maven, MavenUpstream))
	reg := &Registry{Proxy: p, Upstreams: cfg.Registries}
	apt := &aptProxy{p: p, hosts: cfg.AptHosts, upstream: cfg.AptUpstream}
	if apt.hosts == nil {
		apt.hosts = mirrors.PublicAptHosts
	}
	if apt.upstream == nil {
		apt.upstream = func(host string) string { return "http://" + host }
	}

	prefixed := func(prefix string, up *Upstream) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			rest := strings.TrimPrefix(r.URL.EscapedPath(), prefix)
			p.Serve(w, r, up, "/"+strings.TrimPrefix(rest, "/"), r.URL.RawQuery)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if err := cfg.Store.Writable(); err != nil {
			http.Error(w, "cache not writable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.Handle("/v2/", reg)
	mux.Handle("/npm/", prefixed("/npm", npm))
	mux.Handle("/pypi/simple/", prefixed("/pypi", pypi))
	mux.Handle("/pypi/files/", prefixed("/pypi/files", files))
	mux.Handle("/go/", prefixed("/go", gop))
	mux.Handle("/maven/", prefixed("/maven", maven))
	if cfg.Git != nil {
		mux.Handle("/git/", http.StripPrefix("/git", cfg.Git))
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect || r.URL.IsAbs() {
			apt.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// aptProxy is an HTTP forward proxy for apt (Acquire::http::Proxy::<host>).
// It only fetches from allowlisted public repositories: anything else is
// refused, so the mirror is not an open proxy into the region's network.
// Guests are configured to use it only for those hosts, so private apt
// repositories are reached directly.
type aptProxy struct {
	p        *Proxy
	hosts    []string
	upstream func(host string) string
}

func (a *aptProxy) allowed(host string) bool {
	host = strings.ToLower(host)
	for _, h := range a.hosts {
		if host == h || (strings.HasPrefix(h, ".") && strings.HasSuffix(host, h)) {
			return true
		}
	}
	return false
}

func (a *aptProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "apt mirror proxies GET only", http.StatusMethodNotAllowed)
		return
	}
	port := r.URL.Port()
	host := strings.ToLower(r.URL.Hostname())
	if r.URL.Scheme != "http" || (port != "" && port != "80") || !a.allowed(host) {
		http.Error(w, "host not mirrored", http.StatusForbidden)
		return
	}
	up := &Upstream{
		Name:     "apt/" + host,
		Base:     a.upstream(host),
		Classify: AptPolicy,
		TTL:      time.Minute,
	}
	a.p.Serve(w, r, up, r.URL.EscapedPath(), r.URL.RawQuery)
}
