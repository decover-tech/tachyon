// tachyon-mirror is a region's pull-through package mirror: registry proxy
// (Docker Hub, GHCR, Quay), npm, PyPI, Go module proxy, Maven, apt cache
// and git mirror, on one listener. Each region runs two instances on
// separate machines; the control plane health-checks both
// (-mirror-primary/-mirror-secondary) and points guests at upstream if
// both are down.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/decover-tech/tachyon/internal/mirrors/gitmirror"
	"github.com/decover-tech/tachyon/internal/mirrors/proxy"
)

func main() {
	listen := flag.String("listen", ":8443", "listen address")
	dataDir := flag.String("data", "/var/cache/tachyon-mirror", "cache directory")
	publicURL := flag.String("public-url", "", "URL guests reach this instance at (default: from each request)")
	tlsCert := flag.String("tls-cert", "", "TLS certificate (Docker requires TLS for registry mirrors)")
	tlsKey := flag.String("tls-key", "", "TLS key")
	maxGB := flag.Int64("max-cache-gb", 500, "evict least recently used package cache entries above this size")
	gitRepos := flag.String("git-repos", "", "comma-separated owner/name repos to mirror (repos Tachyon builds); must be public; empty redirects all git requests upstream")
	gitUpstream := flag.String("git-upstream", "https://github.com", "git upstream")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	store := &proxy.Store{Dir: filepath.Join(*dataDir, "packages")}
	if err := store.Writable(); err != nil {
		log.Error("cache dir", "err", err)
		os.Exit(1)
	}
	// The git handler is always mounted: with no -git-repos it redirects
	// every request to upstream, so a guest pointed at /git/ by mistake
	// still fetches instead of getting a 404.
	git := &gitmirror.Mirror{
		Dir:      filepath.Join(*dataDir, "git"),
		Upstream: *gitUpstream,
		Log:      log,
	}
	if *gitRepos != "" {
		git.Allowed = gitmirror.AllowList(strings.Split(*gitRepos, ","))
	}
	// No overall timeout: image layers can take a while. Bound the wait
	// for upstream to start answering instead.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	client := &http.Client{Transport: tr}
	h := proxy.New(proxy.Config{Store: store, Client: client, Log: log, PublicURL: *publicURL, Git: git})
	srv := &http.Server{Addr: *listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := store.Prune(*maxGB << 30); err != nil {
					log.Warn("prune", "err", err)
				} else if n > 0 {
					log.Info("pruned cache", "entries", n)
				}
			}
		}
	}()
	go func() {
		var err error
		if *tlsCert != "" {
			err = srv.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			os.Exit(1)
		}
	}()
	log.Info("mirror up", "listen", *listen, "data", *dataDir)
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
}
