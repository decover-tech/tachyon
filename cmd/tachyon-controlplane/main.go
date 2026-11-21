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

	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"

	hostlinkv1 "github.com/decover-tech/tachyon/gen/hostlink/v1"
	"github.com/decover-tech/tachyon/internal/controlplane"
	"github.com/decover-tech/tachyon/internal/frontend/github"
	"github.com/decover-tech/tachyon/internal/mirrors"
	"github.com/decover-tech/tachyon/internal/scheduler"
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
	clickhouseURL := flag.String("clickhouse-url", "", "ClickHouse HTTP URL for job records (password in TACHYON_CLICKHOUSE_PASSWORD)")
	clickhouseTable := flag.String("clickhouse-table", "tachyon.job_records", "ClickHouse table for job records")
	clickhouseUser := flag.String("clickhouse-user", "", "ClickHouse user")
	priceOwned := flag.Float64("price-owned-vcpu-hour", 0, "owned capacity cost per vCPU-hour, for cost per job-minute")
	priceBurst := flag.Float64("price-burst-vcpu-hour", 0, "burst capacity cost per vCPU-hour, for cost per job-minute")
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

	secrets := &controlplane.MemSecrets{}
	core := &controlplane.Core{
		Scheduler: scheduler.New(nil),
		Images:    images,
		Prebuilds: &controlplane.MemPrebuilds{},
		Secrets:   secrets,
		Mirrors:   mirrors.Endpoints{NPM: *npmMirror, GoProxy: *goMirror, Registry: *registryMirror},
		CacheURL:  *cacheURL,
		Log:       log,

		Metrics:    metrics,
		JobRecords: jobRecords,
		Pricing:    telemetry.Pricing{OwnedVCPUHour: *priceOwned, BurstVCPUHour: *priceBurst},
	}
	if err := core.RegisterMetrics(); err != nil {
		log.Error("metrics", "err", err)
		os.Exit(1)
	}
	adapter := &github.Adapter{
		WebhookSecret: []byte(secret),
		API:           &github.Client{AppID: *appID, PrivateKey: key},
		Secrets:       secrets,
		Log:           log,
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
