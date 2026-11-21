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
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/controlplane"
	"github.com/decover-tech/tachyon/internal/frontend/github"
	"github.com/decover-tech/tachyon/internal/hostauth"
	"github.com/decover-tech/tachyon/internal/mirrors"
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
	tlsCert := flag.String("hostlink-cert", "", "hostlink server certificate (PEM)")
	tlsKey := flag.String("hostlink-key", "", "hostlink server private key (PEM)")
	clientCA := flag.String("hostlink-client-ca", "", "CA bundle that host client certificates must chain to (PEM)")
	trustDomain := flag.String("hostlink-trust-domain", hostauth.DefaultTrustDomain, "SPIFFE trust domain of host identities")
	insecureLink := flag.Bool("hostlink-insecure", false, "serve hostlink without mTLS or host authentication (local development only)")
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
	core := &controlplane.Core{
		Scheduler: scheduler.New(nil),
		Images:    images,
		Prebuilds: &controlplane.MemPrebuilds{},
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.Handle("POST /webhooks/github", adapter.Handler(core))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	httpSrv := &http.Server{Addr: *httpAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	link := &controlplane.HostLink{Core: core}
	var grpcOpts []grpc.ServerOption
	if *insecureLink {
		log.Warn("hostlink running WITHOUT mTLS: any client can claim any host id")
	} else {
		tlsCfg, err := hostauth.ServerTLS(hostauth.ServerFiles{Cert: *tlsCert, Key: *tlsKey, ClientCA: *clientCA})
		if err != nil {
			log.Error("hostlink TLS (set -hostlink-cert, -hostlink-key, -hostlink-client-ca, or -hostlink-insecure for local development)", "err", err)
			os.Exit(2)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(tlsCfg)))
		link.Auth = hostauth.PeerAuthenticator{TrustDomain: *trustDomain}
	}
	grpcSrv := grpc.NewServer(grpcOpts...)
	hostlinkv1.RegisterHostLinkServiceServer(grpcSrv, link)
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
