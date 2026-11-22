//! hostd metrics over OpenTelemetry. When `OTEL_EXPORTER_OTLP_ENDPOINT` is
//! set, metrics are pushed over OTLP/gRPC to the collector (see
//! deploy/observability); otherwise the global no-op meter is used and
//! recording costs nothing.
//!
//! Names are already Prometheus-style, so they reach Grafana as written.
//! The per-job phase record itself is the control plane's: hostd only
//! reports phases over hostlink.

use anyhow::Result;
use opentelemetry::metrics::{Counter, Histogram, Meter, ObservableGauge};
use opentelemetry::KeyValue;
use opentelemetry_sdk::metrics::{PeriodicReader, SdkMeterProvider};
use opentelemetry_sdk::Resource;
use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use crate::link::HostInfo;
use crate::pb;

pub const METRIC_JOBS: &str = "tachyon_hostd_jobs_total";
pub const METRIC_JOB_START: &str = "tachyon_hostd_job_start_seconds";
pub const METRIC_RUNNING: &str = "tachyon_hostd_running_jobs";
pub const METRIC_WARM_VMS: &str = "tachyon_hostd_warm_vms";
pub const METRIC_CPU_BUSY: &str = "tachyon_hostd_cpu_busy_ratio";
pub const METRIC_CPU_STEAL: &str = "tachyon_hostd_cpu_steal_ratio";

/// Buckets for assignment-to-VM-resumed: the host's share of pickup.
const JOB_START_BUCKETS: [f64; 10] = [0.05, 0.1, 0.25, 0.5, 1.0, 2.0, 3.0, 5.0, 10.0, 30.0];

#[derive(Debug, Default, Clone)]
struct HostState {
    warm: HashMap<u32, u32>,
    cpu: Option<(f64, f64)>,
    running: u64,
}

pub struct Metrics {
    jobs: Counter<u64>,
    job_start: Histogram<f64>,
    state: Arc<Mutex<HostState>>,
    _gauges: (
        ObservableGauge<u64>,
        ObservableGauge<u64>,
        ObservableGauge<f64>,
        ObservableGauge<f64>,
    ),
}

impl Metrics {
    pub fn new(meter: &Meter) -> Self {
        let state = Arc::new(Mutex::new(HostState::default()));
        let s = state.clone();
        let running = meter
            .u64_observable_gauge(METRIC_RUNNING)
            .with_unit("{job}")
            .with_description("Jobs running on this host")
            .with_callback(move |o| o.observe(s.lock().unwrap().running, &[]))
            .build();
        let s = state.clone();
        let warm = meter
            .u64_observable_gauge(METRIC_WARM_VMS)
            .with_unit("{vm}")
            .with_description("Paused VMs ready for assignment, per size")
            .with_callback(move |o| {
                for (vcpu, n) in &s.lock().unwrap().warm {
                    o.observe(u64::from(*n), &[KeyValue::new("vcpu", vcpu.to_string())]);
                }
            })
            .build();
        let s = state.clone();
        let busy = meter
            .f64_observable_gauge(METRIC_CPU_BUSY)
            .with_unit("1")
            .with_description("Host CPU busy fraction since the previous heartbeat")
            .with_callback(move |o| {
                if let Some((busy, _)) = s.lock().unwrap().cpu {
                    o.observe(busy, &[]);
                }
            })
            .build();
        let s = state.clone();
        let steal = meter
            .f64_observable_gauge(METRIC_CPU_STEAL)
            .with_unit("1")
            .with_description("Host CPU steal fraction since the previous heartbeat")
            .with_callback(move |o| {
                if let Some((_, steal)) = s.lock().unwrap().cpu {
                    o.observe(steal, &[]);
                }
            })
            .build();
        Self {
            jobs: meter
                .u64_counter(METRIC_JOBS)
                .with_unit("{job}")
                .with_description("Jobs finished on this host, by outcome")
                .build(),
            job_start: meter
                .f64_histogram(METRIC_JOB_START)
                .with_unit("s")
                .with_description("Assignment received to VM resumed")
                .with_boundaries(JOB_START_BUCKETS.to_vec())
                .build(),
            state,
            _gauges: (running, warm, busy, steal),
        }
    }

    /// Metrics on the global meter provider (no-op unless `init_otlp` set one).
    pub fn global() -> Self {
        Self::new(&opentelemetry::global::meter("tachyon-hostd"))
    }

    /// Updates the host gauges; called on every heartbeat.
    pub fn update_host(&self, warm: &HashMap<u32, u32>, cpu: Option<(f64, f64)>, running: usize) {
        let mut s = self.state.lock().unwrap();
        s.warm.clone_from(warm);
        if cpu.is_some() {
            s.cpu = cpu;
        }
        s.running = running as u64;
    }

    pub fn job_resumed(&self, vcpu: u32, prebuild_match: &str, since_assign: Duration) {
        let pm = if prebuild_match.is_empty() {
            "unknown"
        } else {
            prebuild_match
        };
        self.job_start.record(
            since_assign.as_secs_f64(),
            &[
                KeyValue::new("vcpu", vcpu.to_string()),
                KeyValue::new("prebuild_match", pm.to_string()),
            ],
        );
    }

    pub fn job_finished(&self, vcpu: u32, outcome: pb::job_result::Outcome) {
        self.jobs.add(
            1,
            &[
                KeyValue::new("vcpu", vcpu.to_string()),
                KeyValue::new("outcome", outcome_name(outcome)),
            ],
        );
    }
}

/// OUTCOME_INFRA_ERROR -> infra_error, matching the control plane's labels.
pub fn outcome_name(o: pb::job_result::Outcome) -> String {
    match o {
        pb::job_result::Outcome::Unspecified => "unknown".into(),
        o => o
            .as_str_name()
            .trim_start_matches("OUTCOME_")
            .to_ascii_lowercase(),
    }
}

/// Installs an OTLP/gRPC meter provider as the global one when
/// `OTEL_EXPORTER_OTLP_ENDPOINT` (or the metrics-specific variable) is set.
/// The caller shuts the returned provider down on exit to flush.
pub fn init_otlp(info: &HostInfo) -> Result<Option<SdkMeterProvider>> {
    let configured = [
        "OTEL_EXPORTER_OTLP_ENDPOINT",
        "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
    ]
    .iter()
    .any(|k| std::env::var(k).is_ok_and(|v| !v.is_empty()));
    if !configured {
        return Ok(None);
    }
    let exporter = opentelemetry_otlp::MetricExporter::builder()
        .with_tonic()
        .build()?;
    let reader = PeriodicReader::builder(exporter, opentelemetry_sdk::runtime::Tokio)
        .with_interval(Duration::from_secs(15))
        .build();
    let provider = SdkMeterProvider::builder()
        .with_reader(reader)
        .with_resource(Resource::new(resource(info)))
        .build();
    opentelemetry::global::set_meter_provider(provider.clone());
    Ok(Some(provider))
}

fn resource(info: &HostInfo) -> Vec<KeyValue> {
    vec![
        KeyValue::new("service.name", "tachyon-hostd"),
        KeyValue::new("service.version", env!("CARGO_PKG_VERSION")),
        KeyValue::new("host.id", info.host_id.clone()),
        KeyValue::new("host", info.host_id.clone()),
        KeyValue::new("region", info.region.clone()),
        KeyValue::new("failure_domain", info.failure_domain.clone()),
    ]
}

#[cfg(test)]
mod tests {
    use super::*;
    use opentelemetry::metrics::MeterProvider;
    use opentelemetry_sdk::metrics::data::{
        Gauge, Histogram as HistogramData, ResourceMetrics, Sum,
    };
    use opentelemetry_sdk::metrics::reader::MetricReader;
    use opentelemetry_sdk::metrics::{InstrumentKind, ManualReader, Pipeline, Temporality};
    use std::sync::Weak;

    /// A ManualReader the test can keep a handle to after giving it to the
    /// provider.
    #[derive(Debug, Clone)]
    struct Shared(Arc<ManualReader>);

    impl MetricReader for Shared {
        fn register_pipeline(&self, p: Weak<Pipeline>) {
            self.0.register_pipeline(p)
        }
        fn collect(
            &self,
            rm: &mut ResourceMetrics,
        ) -> opentelemetry_sdk::metrics::MetricResult<()> {
            self.0.collect(rm)
        }
        fn force_flush(&self) -> opentelemetry_sdk::metrics::MetricResult<()> {
            self.0.force_flush()
        }
        fn shutdown(&self) -> opentelemetry_sdk::metrics::MetricResult<()> {
            self.0.shutdown()
        }
        fn temporality(&self, kind: InstrumentKind) -> Temporality {
            self.0.temporality(kind)
        }
    }

    fn collect(r: &Shared) -> ResourceMetrics {
        let mut rm = ResourceMetrics {
            resource: Resource::empty(),
            scope_metrics: vec![],
        };
        r.collect(&mut rm).unwrap();
        rm
    }

    fn find<'a>(rm: &'a ResourceMetrics, name: &str) -> &'a dyn std::any::Any {
        rm.scope_metrics
            .iter()
            .flat_map(|s| &s.metrics)
            .find(|m| m.name == name)
            .unwrap_or_else(|| panic!("metric {name} not exported"))
            .data
            .as_any()
    }

    #[test]
    fn records_job_and_host_metrics() {
        let reader = Shared(Arc::new(ManualReader::builder().build()));
        let provider = SdkMeterProvider::builder()
            .with_reader(reader.clone())
            .build();
        let m = Metrics::new(&provider.meter("test"));

        m.job_resumed(4, "full", Duration::from_millis(300));
        m.job_finished(4, pb::job_result::Outcome::InfraError);
        m.update_host(&HashMap::from([(2, 3), (8, 1)]), Some((0.5, 0.01)), 2);
        m.update_host(&HashMap::from([(2, 3), (8, 1)]), None, 2); // keeps last CPU sample

        let rm = collect(&reader);
        let jobs = find(&rm, METRIC_JOBS).downcast_ref::<Sum<u64>>().unwrap();
        assert_eq!(jobs.data_points[0].value, 1);
        assert!(jobs.data_points[0]
            .attributes
            .contains(&KeyValue::new("outcome", "infra_error")));

        let start = find(&rm, METRIC_JOB_START)
            .downcast_ref::<HistogramData<f64>>()
            .unwrap();
        assert_eq!(start.data_points[0].count, 1);
        assert!((start.data_points[0].sum - 0.3).abs() < 1e-9);
        assert_eq!(start.data_points[0].bounds, JOB_START_BUCKETS.to_vec());

        let warm = find(&rm, METRIC_WARM_VMS)
            .downcast_ref::<Gauge<u64>>()
            .unwrap();
        let total: u64 = warm.data_points.iter().map(|d| d.value).sum();
        assert_eq!(total, 4);
        let running = find(&rm, METRIC_RUNNING)
            .downcast_ref::<Gauge<u64>>()
            .unwrap();
        assert_eq!(running.data_points[0].value, 2);
        let busy = find(&rm, METRIC_CPU_BUSY)
            .downcast_ref::<Gauge<f64>>()
            .unwrap();
        assert_eq!(busy.data_points[0].value, 0.5);
        find(&rm, METRIC_CPU_STEAL);
    }

    #[test]
    fn outcome_names() {
        use pb::job_result::Outcome;
        assert_eq!(outcome_name(Outcome::Succeeded), "succeeded");
        assert_eq!(outcome_name(Outcome::InfraError), "infra_error");
        assert_eq!(outcome_name(Outcome::Unspecified), "unknown");
    }

    #[test]
    fn otlp_disabled_without_endpoint() {
        if std::env::var_os("OTEL_EXPORTER_OTLP_ENDPOINT").is_some()
            || std::env::var_os("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT").is_some()
        {
            return;
        }
        let info = HostInfo {
            host_id: "h1".into(),
            region: "fra".into(),
            failure_domain: "r1".into(),
            arch: "x86_64".into(),
            vcpu: 16,
            memory_gb: 64,
            peer_address: String::new(),
            host_group: String::new(),
        };
        assert!(init_otlp(&info).unwrap().is_none());
        let r = resource(&info);
        assert!(r.contains(&KeyValue::new("service.name", "tachyon-hostd")));
        assert!(r.contains(&KeyValue::new("region", "fra")));
    }
}
