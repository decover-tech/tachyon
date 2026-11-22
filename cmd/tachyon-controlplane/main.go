// tachyon-controlplane serves GitHub webhooks (HTTP) and the hostd link
// (gRPC). With TACHYON_DATABASE_URL set, jobs, pending runners, hosts, the
// prebuild registry and job records are kept in Postgres and reloaded on
// start; without it state is in memory (development only). Secrets are
// always in memory until the managed key store lands.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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
	"google.golang.org/grpc/credentials"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/controlplane"
	"github.com/decover-tech/tachyon/internal/frontend/github"
	"github.com/decover-tech/tachyon/internal/hostauth"
	"github.com/decover-tech/tachyon/internal/images"
	"github.com/decover-tech/tachyon/internal/mirrors"
	"github.com/decover-tech/tachyon/internal/prebuild"
	"github.com/decover-tech/tachyon/internal/scheduler"
	"github.com/decover-tech/tachyon/internal/secrets"
	"github.com/decover-tech/tachyon/internal/store/postgres"
	"github.com/decover-tech/tachyon/internal/telemetry"
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
	maxDeferral := flag.Duration("max-deferral", controlplane.DefaultMaxDeferral, "longest a tachyon-deferrable job waits for a quiet fleet")
	imageTrust := flag.String("image-trust", "", "trusted base image signing keys (PEM); enables signed images with canary rollout instead of -image")
	imageManifests := flag.String("image-manifests", "", "comma-separated signed base image manifests to publish at startup")
	canaryHosts := flag.String("canary-hosts", "", "comma-separated host IDs that take new base images first")
	tlsCert := flag.String("hostlink-cert", "", "hostlink server certificate (PEM)")
	tlsKey := flag.String("hostlink-key", "", "hostlink server private key (PEM)")
	clientCA := flag.String("hostlink-client-ca", "", "CA bundle that host client certificates must chain to (PEM)")
	trustDomain := flag.String("hostlink-trust-domain", hostauth.DefaultTrustDomain, "SPIFFE trust domain of host identities")
	insecureLink := flag.Bool("hostlink-insecure", false, "serve hostlink without mTLS or host authentication (local development only)")
	keyStore := secrets.Config{}
	flag.StringVar(&keyStore.Backend, "secrets", "vault", "key store for JIT configs and tokens: vault or mem (development only)")
	flag.StringVar(&keyStore.VaultAddr, "vault-addr", os.Getenv("VAULT_ADDR"), "Vault/OpenBao address")
	flag.StringVar(&keyStore.VaultMount, "vault-mount", "secret", "KV v2 mount")
	flag.StringVar(&keyStore.VaultPrefix, "vault-prefix", "tachyon", "key prefix under the mount")
	flag.StringVar(&keyStore.VaultNamespace, "vault-namespace", os.Getenv("VAULT_NAMESPACE"), "Vault namespace (optional)")
	flag.StringVar(&keyStore.VaultTokenFile, "vault-token-file", "", "file holding the Vault token, re-read per request (default: $TACHYON_VAULT_TOKEN)")
	flag.DurationVar(&keyStore.JITTTL, "jit-ttl", secrets.DefaultJITTTL, "TTL of stored JIT runner configs")
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

	var pins controlplane.Images
	var rollout *images.Rollout
	if *imageTrust != "" {
		rollout, err = loadRollout(*imageTrust, *imageManifests, *canaryHosts)
		if err != nil {
			log.Error("base image rollout", "err", err)
			os.Exit(1)
		}
		pins = rollout
	} else {
		static := controlplane.StaticImages{}
		for _, kv := range strings.Split(*image, ",") {
			if n, v, ok := strings.Cut(kv, "="); ok {
				static[n] = v
			}
		}
		pins = static
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	keyStore.VaultToken = secrets.Redacted(os.Getenv("TACHYON_VAULT_TOKEN"))
	keys, err := secrets.Open(keyStore)
	if err != nil {
		log.Error("key store", "err", err)
		os.Exit(2)
	}
	if keyStore.Backend == "mem" {
		log.Warn("secrets kept in process memory; use -secrets=vault in production")
	}
	memPrebuilds := &controlplane.MemPrebuilds{}
	var prebuilds controlplane.Prebuilds = memPrebuilds
	setPrebuildConfig := memPrebuilds.SetConfig
	var db *postgres.Store
	if dsn := os.Getenv("TACHYON_DATABASE_URL"); dsn != "" {
		db, err = postgres.Open(ctx, dsn)
		if err != nil {
			log.Error("open database", "err", err)
			os.Exit(1)
		}
		defer db.Close()
		storePrebuilds := &controlplane.StorePrebuilds{Store: db, OnError: func(op string, err error) {
			log.Warn("prebuild registry", "op", op, "err", err)
		}}
		prebuilds = storePrebuilds
		setPrebuildConfig = func(repo string, c *prebuild.Config) {
			if err := storePrebuilds.SetConfig(repo, c); err != nil {
				log.Warn("prebuild registry", "op", "set config", "repo", repo, "err", err)
			}
		}
	} else {
		log.Warn("TACHYON_DATABASE_URL not set: state is in memory and lost on restart")
	}
	core := &controlplane.Core{
		Scheduler: scheduler.New(nil),
		Images:    pins,
		Prebuilds: prebuilds,
		Secrets:   keys,
		Mirrors:   mirrors.Endpoints{NPM: *npmMirror, GoProxy: *goMirror, Registry: *registryMirror},
		CacheURL:  *cacheURL,
		Log:       log,

		MaxDeferral: *maxDeferral,
		OffPeak:     &telemetry.OffPeak{},
	}
	ghClient := &github.Client{AppID: *appID, PrivateKey: key}
	pbManager := &prebuild.Manager{
		Source:      &github.PrebuildSource{Client: ghClient},
		Images:      pins,
		Registry:    prebuildRegistry{Prebuilds: prebuilds, set: setPrebuildConfig},
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
		Secrets:       keys,
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
		report := time.NewTicker(time.Hour)
		defer report.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				adapter.Reconcile(ctx)
				core.RetryDeferred(ctx)
				if rollout != nil {
					for _, tr := range rollout.Advance() {
						log.Info("base image rollout", "image", tr.Ref.String(), "from", tr.From, "to", tr.To, "reason", tr.Reason)
					}
				}
				core.ReconcileReplicas()
				pbManager.Tick(ctx)
				if m, ok := keys.(*secrets.Mem); ok {
					m.Sweep()
				}
			case <-report.C:
				r := core.OffPeak.Report()
				log.Info("off-peak class", "window", r.Window, "avg_util", r.AvgUtil, "base_util", r.AvgBaseUtil,
					"gain", r.Gain, "relative_gain", r.RelativeGain, "started", r.Started, "overdue", r.Overdue,
					"p95_wait", r.P95Wait, "max_wait", r.MaxWait)
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

// loadRollout reads the trusted image keys and publishes the given signed
// manifests. Rollout state is in memory, like the rest of this build.
func loadRollout(trustFile, manifests, canary string) (*images.Rollout, error) {
	pem, err := os.ReadFile(trustFile)
	if err != nil {
		return nil, err
	}
	keys, err := images.ParsePublicKeys(pem)
	if err != nil {
		return nil, err
	}
	r := &images.Rollout{Trusted: keys}
	for _, h := range strings.Split(canary, ",") {
		if h = strings.TrimSpace(h); h != "" {
			r.Policy.CanaryHosts = append(r.Policy.CanaryHosts, h)
		}
	}
	for _, path := range strings.Split(manifests, ",") {
		if path = strings.TrimSpace(path); path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var s images.Signed
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if _, err := r.Publish(&s); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return r, nil
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

// prebuildRegistry gives the prebuild manager SetConfig on top of whichever
// registry (in memory or Postgres) the control plane runs with.
type prebuildRegistry struct {
	controlplane.Prebuilds
	set func(repo string, c *prebuild.Config)
}

func (r prebuildRegistry) SetConfig(repo string, c *prebuild.Config) { r.set(repo, c) }
