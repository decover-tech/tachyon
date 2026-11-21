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

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/controlplane"
	"github.com/decover-tech/tachyon/internal/frontend/github"
	"github.com/decover-tech/tachyon/internal/mirrors"
	"github.com/decover-tech/tachyon/internal/scheduler"
	"github.com/decover-tech/tachyon/internal/secrets"
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
	keyStore := secrets.Config{}
	flag.StringVar(&keyStore.Backend, "secrets", "vault", "key store for JIT configs and tokens: vault or mem (development only)")
	flag.StringVar(&keyStore.VaultAddr, "vault-addr", os.Getenv("VAULT_ADDR"), "Vault/OpenBao address")
	flag.StringVar(&keyStore.VaultMount, "vault-mount", "secret", "KV v2 mount")
	flag.StringVar(&keyStore.VaultPrefix, "vault-prefix", "tachyon", "key prefix under the mount")
	flag.StringVar(&keyStore.VaultNamespace, "vault-namespace", os.Getenv("VAULT_NAMESPACE"), "Vault namespace (optional)")
	flag.StringVar(&keyStore.VaultTokenFile, "vault-token-file", "", "file holding the Vault token, re-read per request (default: $TACHYON_VAULT_TOKEN)")
	flag.DurationVar(&keyStore.JITTTL, "jit-ttl", secrets.DefaultJITTTL, "TTL of stored JIT runner configs")
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

	keyStore.VaultToken = secrets.Redacted(os.Getenv("TACHYON_VAULT_TOKEN"))
	store, err := secrets.Open(keyStore)
	if err != nil {
		log.Error("key store", "err", err)
		os.Exit(2)
	}
	if keyStore.Backend == "mem" {
		log.Warn("secrets kept in process memory; use -secrets=vault in production")
	}

	images := controlplane.StaticImages{}
	for _, kv := range strings.Split(*image, ",") {
		if n, v, ok := strings.Cut(kv, "="); ok {
			images[n] = v
		}
	}
	core := &controlplane.Core{
		Scheduler: scheduler.New(nil),
		Images:    images,
		Prebuilds: &controlplane.MemPrebuilds{},
		Secrets:   store,
		Mirrors:   mirrors.Endpoints{NPM: *npmMirror, GoProxy: *goMirror, Registry: *registryMirror},
		CacheURL:  *cacheURL,
		Log:       log,
	}
	adapter := &github.Adapter{
		WebhookSecret: []byte(secret),
		API:           &github.Client{AppID: *appID, PrivateKey: key},
		Secrets:       store,
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
				if m, ok := store.(*secrets.Mem); ok {
					m.Sweep()
				}
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
