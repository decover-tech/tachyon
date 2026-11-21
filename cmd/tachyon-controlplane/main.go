// tachyon-controlplane serves GitHub webhooks (HTTP) and the hostd link
// (gRPC). With TACHYON_DATABASE_URL set, jobs, pending runners, hosts, the
// prebuild registry and job records are kept in Postgres and reloaded on
// start; without it state is in memory (development only). Secrets are
// always in memory until the managed key store lands.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
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
	"github.com/decover-tech/tachyon/internal/scheduler"
	"github.com/decover-tech/tachyon/internal/store/postgres"
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	secrets := &controlplane.MemSecrets{}
	var prebuilds controlplane.Prebuilds = &controlplane.MemPrebuilds{}
	var db *postgres.Store
	if dsn := os.Getenv("TACHYON_DATABASE_URL"); dsn != "" {
		db, err = postgres.Open(ctx, dsn)
		if err != nil {
			log.Error("open database", "err", err)
			os.Exit(1)
		}
		defer db.Close()
		prebuilds = &controlplane.StorePrebuilds{Store: db, OnError: func(op string, err error) {
			log.Warn("prebuild registry", "op", op, "err", err)
		}}
	} else {
		log.Warn("TACHYON_DATABASE_URL not set: state is in memory and lost on restart")
	}
	core := &controlplane.Core{
		Scheduler: scheduler.New(nil),
		Images:    images,
		Prebuilds: prebuilds,
		Secrets:   secrets,
		Mirrors:   mirrors.Endpoints{NPM: *npmMirror, GoProxy: *goMirror, Registry: *registryMirror},
		CacheURL:  *cacheURL,
		Log:       log,
	}
	adapter := &github.Adapter{
		WebhookSecret: []byte(secret),
		API:           &github.Client{AppID: *appID, PrivateKey: key},
		Secrets:       secrets,
		Log:           log,
	}
	if db != nil {
		core.Store, adapter.Store = db, db
		st, err := core.Restore(ctx)
		if err != nil {
			log.Error("restore control plane state", "err", err)
			os.Exit(1)
		}
		pending, err := adapter.Restore(ctx)
		if err != nil {
			log.Error("restore pending runners", "err", err)
			os.Exit(1)
		}
		log.Info("state restored", "hosts", st.Hosts, "running", st.Running, "requeued", st.Requeued, "pending_runners", pending)
	}

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
