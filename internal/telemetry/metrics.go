package telemetry

import (
	"context"
	"errors"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// Metric names. They are already in Prometheus form (unit suffix, _total on
// counters), so the OTLP-to-Prometheus translation in the collector or in
// Prometheus' own OTLP receiver keeps them as written. The dashboards and
// alert rules in deploy/observability query these names; a test checks that
// every tachyon_* name they use is listed here.
const (
	MetricJobsQueued       = "tachyon_jobs_queued_total"
	MetricJobsPickedUp     = "tachyon_jobs_picked_up_total"
	MetricJobsBurst        = "tachyon_jobs_burst_total"
	MetricPickup           = "tachyon_job_pickup_seconds"
	MetricJobsFinished     = "tachyon_jobs_finished_total"
	MetricJobRun           = "tachyon_job_run_seconds"
	MetricBillable         = "tachyon_job_billable_seconds_total"
	MetricCost             = "tachyon_job_cost_usd_total"
	MetricCacheRequests    = "tachyon_cache_requests_total"
	MetricCacheBytes       = "tachyon_cache_restore_bytes_total"
	MetricCacheRestore     = "tachyon_cache_restore_seconds"
	MetricHostCPUBusy      = "tachyon_host_cpu_busy_ratio"
	MetricHostCPUSteal     = "tachyon_host_cpu_steal_ratio"
	MetricHostOversub      = "tachyon_host_oversubscription_ratio"
	MetricHostCPUReserved  = "tachyon_host_cpu_reserved_ratio"
	MetricHostWarmVMs      = "tachyon_host_warm_vms"
	MetricHostsConnected   = "tachyon_hosts_connected"
	MetricFleetCPUReserved = "tachyon_fleet_cpu_reserved_ratio"
	MetricRowsDropped      = "tachyon_job_records_dropped_total"
	MetricRowsPending      = "tachyon_job_records_pending"
)

// MetricNames lists every metric the Go services export.
var MetricNames = []string{
	MetricJobsQueued, MetricJobsPickedUp, MetricJobsBurst, MetricPickup, MetricJobsFinished,
	MetricJobRun, MetricBillable, MetricCost, MetricCacheRequests,
	MetricCacheBytes, MetricCacheRestore, MetricHostCPUBusy, MetricHostCPUSteal,
	MetricHostOversub, MetricHostCPUReserved, MetricHostWarmVMs, MetricHostsConnected,
	MetricFleetCPUReserved, MetricRowsDropped, MetricRowsPending,
}

// PickupBuckets brackets the pickup targets (p50 < 2 s, p95 < 5 s) and the
// SLO bound (p95 < 10 s under failure), so histogram_quantile is exact at
// each of them.
var PickupBuckets = []float64{0.25, 0.5, 1, 1.5, 2, 3, 5, 7.5, 10, 15, 30, 60, 120, 300}

// RunBuckets covers job wall time from seconds to the 6 h job deadline.
var RunBuckets = []float64{5, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 21600}

// Metrics records fleet metrics from job records. The zero value is not
// usable; use NewMetrics. A nil *Metrics is a no-op.
type Metrics struct {
	meter          metric.Meter
	queued         metric.Int64Counter
	pickedUp       metric.Int64Counter
	burst          metric.Int64Counter
	pickup         metric.Float64Histogram
	finished       metric.Int64Counter
	run            metric.Float64Histogram
	billable       metric.Float64Counter
	cost           metric.Float64Counter
	cacheRequests  metric.Int64Counter
	cacheBytes     metric.Int64Counter
	cacheRestore   metric.Float64Histogram
	hostCPUBusy    metric.Float64ObservableGauge
	hostCPUSteal   metric.Float64ObservableGauge
	hostOversub    metric.Float64ObservableGauge
	hostReserved   metric.Float64ObservableGauge
	hostWarmVMs    metric.Int64ObservableGauge
	hostsConnected metric.Int64ObservableGauge
	fleetReserved  metric.Float64ObservableGauge
}

// NewMetrics creates the instruments on meter (nil = no-op meter).
func NewMetrics(meter metric.Meter) (*Metrics, error) {
	if meter == nil {
		meter = noop.NewMeterProvider().Meter("")
	}
	m := &Metrics{meter: meter}
	var errs []error
	var e error
	m.queued, e = meter.Int64Counter(MetricJobsQueued, metric.WithUnit("{job}"),
		metric.WithDescription("Jobs submitted to the control plane"))
	errs = append(errs, e)
	m.pickedUp, e = meter.Int64Counter(MetricJobsPickedUp, metric.WithUnit("{job}"),
		metric.WithDescription("Jobs whose runner came online"))
	errs = append(errs, e)
	m.burst, e = meter.Int64Counter(MetricJobsBurst, metric.WithUnit("{job}"),
		metric.WithDescription("Jobs handed to burst capacity; their pickup is the burst provider's"))
	errs = append(errs, e)
	m.pickup, e = meter.Float64Histogram(MetricPickup, metric.WithUnit("s"),
		metric.WithDescription("Webhook received to runner online"),
		metric.WithExplicitBucketBoundaries(PickupBuckets...))
	errs = append(errs, e)
	m.finished, e = meter.Int64Counter(MetricJobsFinished, metric.WithUnit("{job}"),
		metric.WithDescription("Finished jobs by prebuild match, capacity and outcome"))
	errs = append(errs, e)
	m.run, e = meter.Float64Histogram(MetricJobRun, metric.WithUnit("s"),
		metric.WithDescription("Job started to job finished, by whether the host was oversubscribed"),
		metric.WithExplicitBucketBoundaries(RunBuckets...))
	errs = append(errs, e)
	m.billable, e = meter.Float64Counter(MetricBillable, metric.WithUnit("s"),
		metric.WithDescription("Billable runner seconds (VM resumed to job finished)"))
	errs = append(errs, e)
	m.cost, e = meter.Float64Counter(MetricCost, metric.WithUnit("{USD}"),
		metric.WithDescription("Capacity cost of finished jobs"))
	errs = append(errs, e)
	m.cacheRequests, e = meter.Int64Counter(MetricCacheRequests, metric.WithUnit("{request}"),
		metric.WithDescription("Actions cache restore lookups by result (hit|miss)"))
	errs = append(errs, e)
	m.cacheBytes, e = meter.Int64Counter(MetricCacheBytes, metric.WithUnit("By"),
		metric.WithDescription("Bytes served by Actions cache restores"))
	errs = append(errs, e)
	m.cacheRestore, e = meter.Float64Histogram(MetricCacheRestore, metric.WithUnit("s"),
		metric.WithDescription("Actions cache restore duration"))
	errs = append(errs, e)
	m.hostCPUBusy, e = meter.Float64ObservableGauge(MetricHostCPUBusy, metric.WithUnit("1"),
		metric.WithDescription("Host CPU busy fraction from hostd heartbeats"))
	errs = append(errs, e)
	m.hostCPUSteal, e = meter.Float64ObservableGauge(MetricHostCPUSteal, metric.WithUnit("1"),
		metric.WithDescription("Host CPU steal fraction from hostd heartbeats"))
	errs = append(errs, e)
	m.hostOversub, e = meter.Float64ObservableGauge(MetricHostOversub, metric.WithUnit("1"),
		metric.WithDescription("Scheduler CPU oversubscription ratio per host"))
	errs = append(errs, e)
	m.hostReserved, e = meter.Float64ObservableGauge(MetricHostCPUReserved, metric.WithUnit("1"),
		metric.WithDescription("Reserved CPU over physical vCPUs per host; above 1 = oversubscribed"))
	errs = append(errs, e)
	m.hostWarmVMs, e = meter.Int64ObservableGauge(MetricHostWarmVMs, metric.WithUnit("{vm}"),
		metric.WithDescription("Paused VMs ready for assignment, per host and size"))
	errs = append(errs, e)
	m.hostsConnected, e = meter.Int64ObservableGauge(MetricHostsConnected, metric.WithUnit("{host}"),
		metric.WithDescription("Hosts with an open hostlink stream"))
	errs = append(errs, e)
	m.fleetReserved, e = meter.Float64ObservableGauge(MetricFleetCPUReserved, metric.WithUnit("1"),
		metric.WithDescription("Reserved CPU over physical vCPUs across non-draining hosts"))
	errs = append(errs, e)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return m, nil
}

func vcpuAttr(v int) attribute.KeyValue { return attribute.String("vcpu", strconv.Itoa(v)) }

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// JobQueued counts a submitted job. region is the requested region, or
// "any".
func (m *Metrics) JobQueued(ctx context.Context, region string, vcpu int) {
	if m == nil {
		return
	}
	if region == "" {
		region = "any"
	}
	m.queued.Add(ctx, 1, metric.WithAttributes(attribute.String("region", region), vcpuAttr(vcpu)))
}

// JobBurst counts a job handed to burst capacity. The pickup SLO counts it
// as picked up: Tachyon placed it, on rented rather than owned hosts.
func (m *Metrics) JobBurst(ctx context.Context, region string, vcpu int) {
	if m == nil {
		return
	}
	if region == "" {
		region = "any"
	}
	m.burst.Add(ctx, 1, metric.WithAttributes(attribute.String("region", region), vcpuAttr(vcpu)))
}

// RunnerOnline records pickup latency once the job's runner is online.
func (m *Metrics) RunnerOnline(ctx context.Context, r *Record) {
	if m == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("region", orUnknown(r.Region)),
		vcpuAttr(r.VCPU),
		attribute.String("prebuild_match", orUnknown(r.PrebuildMatch)),
		attribute.String("capacity", r.Capacity()),
	)
	m.pickedUp.Add(ctx, 1, attrs)
	if d, ok := r.Pickup(); ok {
		m.pickup.Record(ctx, d.Seconds(), attrs)
	}
}

// JobFinished records a finished job's outcome, run time, billable time
// and cost.
func (m *Metrics) JobFinished(ctx context.Context, r *Record, p Pricing) {
	if m == nil {
		return
	}
	region := attribute.String("region", orUnknown(r.Region))
	capacity := attribute.String("capacity", r.Capacity())
	m.finished.Add(ctx, 1, metric.WithAttributes(region, vcpuAttr(r.VCPU), capacity,
		attribute.String("prebuild_match", orUnknown(r.PrebuildMatch)),
		attribute.String("outcome", orUnknown(r.Outcome))))
	if d, ok := r.Run(); ok {
		m.run.Record(ctx, d.Seconds(), metric.WithAttributes(region, vcpuAttr(r.VCPU),
			attribute.Bool("oversubscribed", r.HostLoad > 1)))
	}
	if d, ok := r.Billable(); ok {
		m.billable.Add(ctx, d.Seconds(), metric.WithAttributes(region, vcpuAttr(r.VCPU), capacity))
		m.cost.Add(ctx, r.Cost(p), metric.WithAttributes(region, capacity))
	}
}

// CacheRestore records one Actions cache restore lookup. The cache service
// calls it; bytes and d are zero on a miss.
func (m *Metrics) CacheRestore(ctx context.Context, region string, hit bool, bytes int64, d time.Duration) {
	if m == nil {
		return
	}
	result := "miss"
	if hit {
		result = "hit"
	}
	reg := attribute.String("region", orUnknown(region))
	m.cacheRequests.Add(ctx, 1, metric.WithAttributes(reg, attribute.String("result", result)))
	if hit {
		m.cacheBytes.Add(ctx, bytes, metric.WithAttributes(reg))
		m.cacheRestore.Record(ctx, d.Seconds(), metric.WithAttributes(reg))
	}
}

// HostSample is one host's latest state for the host gauges.
type HostSample struct {
	HostID   string
	Region   string
	CPUBusy  float64
	CPUSteal float64
	Ratio    float64 // scheduler's allowed oversubscription ratio
	Load     float64 // reserved CPU over physical vCPUs
	WarmVMs  map[uint32]uint32
}

// ObserveHosts registers the host and fleet gauges. hosts and fleetReserved
// are called on every collection.
func (m *Metrics) ObserveHosts(hosts func() []HostSample, fleetReserved func() float64) error {
	if m == nil {
		return nil
	}
	_, err := m.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		hs := hosts()
		o.ObserveInt64(m.hostsConnected, int64(len(hs)))
		for _, h := range hs {
			attrs := metric.WithAttributes(attribute.String("host", h.HostID), attribute.String("region", orUnknown(h.Region)))
			o.ObserveFloat64(m.hostCPUBusy, h.CPUBusy, attrs)
			o.ObserveFloat64(m.hostCPUSteal, h.CPUSteal, attrs)
			o.ObserveFloat64(m.hostOversub, h.Ratio, attrs)
			o.ObserveFloat64(m.hostReserved, h.Load, attrs)
			for vcpu, n := range h.WarmVMs {
				o.ObserveInt64(m.hostWarmVMs, int64(n), metric.WithAttributes(
					attribute.String("host", h.HostID), attribute.String("region", orUnknown(h.Region)),
					attribute.String("vcpu", strconv.FormatUint(uint64(vcpu), 10))))
			}
		}
		if fleetReserved != nil {
			o.ObserveFloat64(m.fleetReserved, fleetReserved())
		}
		return nil
	}, m.hostCPUBusy, m.hostCPUSteal, m.hostOversub, m.hostReserved, m.hostWarmVMs, m.hostsConnected, m.fleetReserved)
	return err
}

// ObserveBatcher registers gauges for the job-record export queue.
func (m *Metrics) ObserveBatcher(b *Batcher) error {
	if m == nil || b == nil {
		return nil
	}
	_, err := m.meter.Int64ObservableCounter(MetricRowsDropped, metric.WithUnit("{row}"),
		metric.WithDescription("Job records dropped because ClickHouse fell behind"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(b.Dropped()))
			return nil
		}))
	if err != nil {
		return err
	}
	_, err = m.meter.Int64ObservableGauge(MetricRowsPending, metric.WithUnit("{row}"),
		metric.WithDescription("Job records waiting to be written to ClickHouse"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(b.Pending()))
			return nil
		}))
	return err
}
