// tachyon-controlplane serves GitHub webhooks (HTTP) and the hostd link
// (gRPC). State is in memory; this is the development build of the control
// plane, not a production deployment.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/controlplane"
	"github.com/decover-tech/tachyon/internal/frontend/github"
	"github.com/decover-tech/tachyon/internal/mirrors"
	"github.com/decover-tech/tachyon/internal/prebuild"
	"github.com/decover-tech/tachyon/internal/scheduler"
)

func main() {
	httpAddr := flag.String("http", ":8080", "webhook listen address")
	grpcAddr := flag.String("grpc", ":9090", "hostlink listen address")
	image := flag.String("image", "ubuntu-24.04=2026-11-16", "comma-separated name=version image pins")
	appID := flag.Int64("github-app-id", 0, "GitHub App ID")
	keyFile := flag.String("github-app-key", "", "GitHub App private key (PEM)")
	cacheURL := flag.String("cache-url", "", "regional Actions cache URL")
	npmMirror := flag.String("mirror-npm", "", "npm mirror URL")
	goMirror := flag.String("mirror-go", "", "Go module proxy URL")
	registryMirror := flag.String("mirror-registry", "", "container registry mirror URL")
	prebuildInterval := flag.Duration("prebuild-interval", 10*time.Minute, "minimum time between push-triggered prebuilds of one repo")
	prebuildNightly := flag.Int("prebuild-nightly-hour", 3, "UTC hour after which each repo's nightly prebuild refresh runs")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	secret := os.Getenv("TACHYON_GITHUB_WEBHOOK_SECRET")
	if secret == "" || *appID == 0 || *keyFile == "" {
		log.Error("need TACHYON_GITHUB_WEBHOOK_SECRET, -github-app-id and -github-app-key")
		os.Exit(2)
	}
	pem, err := os.ReadFile(*keyFile)
	if err != nil {
		log.Error("read app key", "err", err)
		os.Exit(1)
	}
	key, err := github.ParsePrivateKey(pem)
	if err != nil {
		log.Error("parse app key", "err", err)
		os.Exit(1)
	}

	images := controlplane.StaticImages{}
	for _, kv := range strings.Split(*image, ",") {
		if n, v, ok := strings.Cut(kv, "="); ok {
			images[n] = v
		}
	}
	secrets := &controlplane.MemSecrets{}
	prebuilds := &controlplane.MemPrebuilds{}
	core := &controlplane.Core{
		Scheduler: scheduler.New(nil),
		Images:    images,
		Prebuilds: prebuilds,
		Secrets:   secrets,
		Mirrors:   mirrors.Endpoints{NPM: *npmMirror, GoProxy: *goMirror, Registry: *registryMirror},
		CacheURL:  *cacheURL,
		Log:       log,
	}
	ghClient := &github.Client{AppID: *appID, PrivateKey: key}
	pbManager := &prebuild.Manager{
		Source:      &github.PrebuildSource{Client: ghClient},
		Images:      images,
		Registry:    prebuilds,
		Builder:     core,
		Log:         log,
		MinInterval: *prebuildInterval,
		NightlyHour: *prebuildNightly,
		// Warm steps reach only the git host and Tachyon's mirrors.
		EgressAllowlist: egressHosts("github.com", *npmMirror, *goMirror, *registryMirror),
	}
	core.Prebuild = pbManager
	adapter := &github.Adapter{
		WebhookSecret: []byte(secret),
		API:           ghClient,
		Secrets:       secrets,
		Log:           log,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.Handle("POST /webhooks/github", adapter.Handler(core))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	httpSrv := &http.Server{Addr: *httpAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	// TODO: mTLS between hostd and the control plane.
	grpcSrv := grpc.NewServer()
	hostlinkv1.RegisterHostLinkServiceServer(grpcSrv, &controlplane.HostLink{Core: core})
	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Error("listen grpc", "err", err)
		os.Exit(1)
	}

	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				adapter.Reconcile(ctx)
				core.RetryDeferred(ctx)
				pbManager.Tick(ctx)
			}
		}
	}()
	go func() {
		if err := grpcSrv.Serve(lis); err != nil {
			log.Error("grpc", "err", err)
		}
	}()
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http", "err", err)
		}
	}()
	log.Info("control plane up", "http", *httpAddr, "grpc", *grpcAddr)

	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdown)
	grpcSrv.GracefulStop()
}

// egressHosts returns the host of each non-empty URL (or bare host).
func egressHosts(urls ...string) []string {
	var out []string
	for _, u := range urls {
		if u == "" {
			continue
		}
		if p, err := url.Parse(u); err == nil && p.Host != "" {
			u = p.Host
		}
		out = append(out, u)
	}
	return out
}
