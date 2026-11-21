# Observability

One job record drives billing, the minutes-saved report and fleet alerts ([design](../../docs/architecture.md#observability-metering-and-billing)).

```
hostd ──OTLP/gRPC──┐
                   ├─> OTel Collector ──> Prometheus ──> Grafana (fleet, SLOs)
control plane ─OTLP/HTTP─┘        └─> ClickHouse otel.* (logs, traces)
control plane ──HTTP JSONEachRow──> ClickHouse tachyon.job_records ──> Grafana (job analytics)
```

| Path | What |
| --- | --- |
| `otel-collector/config.yaml` | OTLP in; Prometheus exporter for metrics; ClickHouse for logs and traces |
| `prometheus/rules/tachyon.rules.yml` | SLO recording rules and burn-rate alerts |
| `grafana/dashboards/tachyon-fleet.json` | Pickup p50/p95 per region and size, prebuild and cache hit rates, cache throughput, host utilization, oversubscription slowdown, cost per job-minute, SLO status |
| `grafana/dashboards/tachyon-jobs.json` | ClickHouse: pickup, prebuild hit rate and minutes saved per repo, oversubscription slowdown normalized per repo, cost by capacity and tenant |
| `clickhouse/001_job_records.sql` | The job-record table |
| `docker-compose.yml` | The whole stack locally |

SLOs and their alerts:

| SLO | Signal | Alerts |
| --- | --- | --- |
| 99.9% of queued jobs picked up | `tachyon_jobs_picked_up_total` + `tachyon_jobs_burst_total` over `tachyon_jobs_queued_total` | `TachyonPickupSLOFastBurn` (page), `TachyonPickupSLOSlowBurn` (ticket) |
| 99.95% control plane availability | blackbox probe of `/healthz` (`probe_success`) | `TachyonControlPlaneSLOFastBurn` (page), `TachyonControlPlaneSLOSlowBurn` (ticket) |
| p95 pickup < 10 s, also during failures | `tachyon_job_pickup_seconds` | `TachyonPickupLatencySLO` (page); `TachyonPickupLatencyTarget` at 5 s per region and size (ticket) |

`go test ./deploy/...` checks that every metric the rules and dashboards use is one the services export (`internal/telemetry/metrics.go`, `hostd/src/telemetry.rs`), that every recording rule they use is defined, and that the ClickHouse columns match `telemetry.JobRow`.
