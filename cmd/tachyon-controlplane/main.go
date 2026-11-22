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
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/abuse"
	"github.com/decover-tech/tachyon/internal/cache"
	"github.com/decover-tech/tachyon/internal/controlplane"
	"github.com/decover-tech/tachyon/internal/frontend/github"
	"github.com/decover-tech/tachyon/internal/hostauth"
	"github.com/decover-tech/tachyon/internal/images"
	"github.com/decover-tech/tachyon/internal/mirrors"
	"github.com/decover-tech/tachyon/internal/mirrors/gitmirror"
	"github.com/decover-tech/tachyon/internal/prebuild"
	"github.com/decover-tech/tachyon/internal/scheduler"
	"github.com/decover-tech/tachyon/internal/secrets"
	"github.com/decover-tech/tachyon/internal/store/postgres"
	"github.com/decover-tech/tachyon/internal/telemetry"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

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
	nightlyHour := flag.Int("nightly-hour", 3, "UTC hour after which each opted-in repo's nightly clean run starts; -1 disables it")
	nightlyRepos := flag.String("nightly-repos", "", "comma-separated owner/name repos opted in to the nightly clean run, which re-runs every job of a default-branch workflow run (deploy jobs too) and needs the App's actions:write and checks:write permissions; empty disables it")
	mirrorPrimary := flag.String("mirror-primary", "", "primary tachyon-mirror base URL; with -mirror-secondary, health-checked and overrides the static -mirror-* flags")
	mirrorSecondary := flag.String("mirror-secondary", "", "secondary tachyon-mirror base URL")
	gitRepos := flag.String("mirror-git-repos", "", "comma-separated public owner/name repos the mirrors' -git-repos serve; only jobs for these repos fetch git through the mirror (never list private repos)")
	allowedOwners := flag.String("github-allowed-owners", "", "comma-separated GitHub orgs/users whose repos get runners (empty: any installation)")
	rerunInfra := flag.String("github-rerun-infra", "", "comma-separated GitHub App installation IDs opted in to re-running jobs that failed for infrastructure reasons (needs Actions write)")
	clickhouseURL := flag.String("clickhouse-url", "", "ClickHouse HTTP URL for job records (password in TACHYON_CLICKHOUSE_PASSWORD)")
	clickhouseTable := flag.String("clickhouse-table", "tachyon.job_records", "ClickHouse table for job records")
	clickhouseUser := flag.String("clickhouse-user", "", "ClickHouse user")
	priceOwned := flag.Float64("price-owned-vcpu-hour", 0, "owned capacity cost per vCPU-hour, for cost per job-minute")
	priceBurst := flag.Float64("price-burst-vcpu-hour", 0, "burst capacity cost per vCPU-hour, for cost per job-minute")
	unknownPlan := flag.String("unknown-tenant-plan", "paid", "plan assumed for tenants with no account record (free, trial, paid; empty refuses them)")
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

	// Metrics go over OTLP to the collector when the standard OTEL_*
	// endpoint variables are set; otherwise they are not exported.
	var meter metric.Meter
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != "" {
		mp, err := telemetry.NewOTLPMeterProvider(ctx, "tachyon-controlplane", version, 0)
		if err != nil {
			log.Error("otel", "err", err)
			os.Exit(1)
		}
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = mp.Shutdown(sctx)
		}()
		meter = mp.Meter("github.com/decover-tech/tachyon/controlplane")
	}
	metrics, err := telemetry.NewMetrics(meter)
	if err != nil {
		log.Error("metrics", "err", err)
		os.Exit(1)
	}
	var jobRecords controlplane.JobRecordSink
	if *clickhouseURL != "" {
		b := &telemetry.Batcher{
			Sink: &telemetry.ClickHouse{
				URL:      *clickhouseURL,
				Table:    *clickhouseTable,
				User:     *clickhouseUser,
				Password: os.Getenv("TACHYON_CLICKHOUSE_PASSWORD"),
			},
			Log: log,
		}
		if err := metrics.ObserveBatcher(b); err != nil {
			log.Error("metrics", "err", err)
			os.Exit(1)
		}
		batcherDone := make(chan struct{})
		go func() { b.Run(ctx); close(batcherDone) }()
		defer func() { <-batcherDone }()
		jobRecords = b
	}

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
	// No account store yet: every tenant gets -unknown-tenant-plan.
	gate := &abuse.Gate{Accounts: &abuse.MemAccounts{}, UnknownPlan: abuse.Plan(*unknownPlan)}
	core := &controlplane.Core{
		Scheduler: scheduler.New(nil),
		Images:    pins,
		Prebuilds: prebuilds,
		Secrets:   keys,
		Limits:    gate,
		Anomalies: &abuse.Monitor{Alerter: abuse.LogAlerter{Log: log}, Suspender: gate, Log: log},
		Mirrors:   mirrors.Endpoints{NPM: *npmMirror, GoProxy: *goMirror, Registry: *registryMirror},
		CacheURL:  *cacheURL,
		Log:       log,

		MaxDeferral: *maxDeferral,
		OffPeak:     &telemetry.OffPeak{},

		Metrics:    metrics,
		JobRecords: jobRecords,
		Pricing:    telemetry.Pricing{OwnedVCPUHour: *priceOwned, BurstVCPUHour: *priceBurst},
	}
	if err := core.RegisterMetrics(); err != nil {
		log.Error("metrics", "err", err)
		os.Exit(1)
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
		EgressAllowlist: egressHosts("github.com", *npmMirror, *goMirror, *registryMirror, *mirrorPrimary, *mirrorSecondary),
	}
	core.Prebuild = pbManager
	if k := os.Getenv("TACHYON_CACHE_KEY"); k != "" {
		core.CacheSigner = &cache.Signer{Key: []byte(k)}
	}
	if *mirrorPrimary != "" || *mirrorSecondary != "" {
		mon := &mirrors.Monitor{
			Primary: *mirrorPrimary, Secondary: *mirrorSecondary,
			Prober: mirrors.HTTPProber{Client: &http.Client{Timeout: 5 * time.Second}},
			Log:    log,
		}
		core.MirrorPair = mon
		if *gitRepos != "" {
			core.GitMirrored = gitmirror.AllowList(strings.Split(*gitRepos, ","))
		}
		go mon.Run(ctx, 5*time.Second)
	}
	adapter := &github.Adapter{
		WebhookSecret: []byte(secret),
		API:           ghClient,
		Secrets:       keys,
		Log:           log,
	}
	for _, o := range strings.Split(*allowedOwners, ",") {
		if o = strings.TrimSpace(o); o != "" {
			adapter.AllowedOwners = append(adapter.AllowedOwners, o)
		}
	}
	if *rerunInfra != "" {
		optedInInfra := map[int64]bool{}
		for _, s := range strings.Split(*rerunInfra, ",") {
			id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
			if err != nil {
				log.Error("bad -github-rerun-infra installation ID", "value", s)
				os.Exit(2)
			}
			optedInInfra[id] = true
		}
		adapter.RerunInfra = func(inst int64, _ string) bool { return optedInInfra[inst] }
	}
	core.Results = adapter
	optedIn := map[string]bool{}
	for _, r := range strings.Split(*nightlyRepos, ",") {
		if r = strings.TrimSpace(r); r != "" {
			optedIn[strings.ToLower(r)] = true
		}
	}
	if *nightlyHour >= 0 && *nightlyHour < 24 && len(optedIn) > 0 {
		adapter.Nightly = &github.Nightly{
			API:   ghClient,
			Hour:  *nightlyHour,
			Repos: func(repo string) bool { return optedIn[strings.ToLower(repo)] },
			PrebuildMatch: func(jobID string) prebuild.Match {
				if rec := core.Record(jobID); rec != nil {
					return prebuild.Match(rec.PrebuildMatch)
				}
				return ""
			},
		}
	}
	// Opt-in features request extra App permissions; enable them on the
	// client only when their flags are set, so a default deployment's
	// installation tokens stay at the base least-privilege set.
	if adapter.RerunInfra != nil {
		ghClient.Features = append(ghClient.Features, github.FeatureInfraRerun)
	}
	if adapter.Nightly != nil {
		ghClient.Features = append(ghClient.Features, github.FeatureNightly)
	}
	if len(ghClient.Features) > 0 {
		log.Info("GitHub opt-in features enabled; the App registration must grant their permissions",
			"features", ghClient.Features, "permissions", github.PermissionsFor(ghClient.Features...))
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
