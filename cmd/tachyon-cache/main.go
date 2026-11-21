// tachyon-cache is the regional GitHub-compatible Actions cache service. It
// serves the Twirp cache API and signed blob URLs. This build stores blobs on
// local disk (-dir) or in memory; the production object store (decision D4)
// plugs in behind cache.BlobStore.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/decover-tech/tachyon/internal/cache"
)

func main() {
	addr := flag.String("http", ":8090", "listen address")
	publicURL := flag.String("public-url", "", "base URL VMs reach this service at, e.g. http://cache.r1.tachyon.internal:8090")
	dir := flag.String("dir", "", "store blobs under this directory (default: in memory)")
	quotaGB := flag.Int64("quota-gb", cache.DefaultQuotaBytes>>30, "per-repo cache quota in GB")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	key := os.Getenv("TACHYON_CACHE_KEY")
	if key == "" || *publicURL == "" {
		log.Error("need TACHYON_CACHE_KEY (shared with the control plane) and -public-url")
		os.Exit(2)
	}
	var store cache.BlobStore = &cache.MemStore{}
	if *dir != "" {
		store = cache.DirStore{Root: *dir}
	}
	ix := &cache.Index{QuotaBytes: *quotaGB << 30}
	srv := cache.NewServer(ix, store, &cache.Signer{Key: []byte(key)}, *publicURL)
	srv.Log = log

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http", "err", err)
			stop()
		}
	}()
	log.Info("cache service up", "http", *addr, "public_url", *publicURL)
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdown)
}
