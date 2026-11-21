// Package observability holds the deployable observability config: the
// OpenTelemetry Collector, Prometheus SLO rules, Grafana dashboards and the
// ClickHouse job-record schema. These tests keep that config consistent with
// the metrics and rows the services actually emit.
package observability

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/decover-tech/tachyon/internal/telemetry"
)

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readYAML(t *testing.T, path string, v any) {
	t.Helper()
	if err := yaml.Unmarshal(read(t, path), v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// knownMetrics is every metric the Go services and hostd export.
func knownMetrics(t *testing.T) map[string]bool {
	known := map[string]bool{}
	for _, n := range telemetry.MetricNames {
		known[n] = true
	}
	hostd := regexp.MustCompile(`"(tachyon_hostd_[a-z0-9_]+)"`).FindAllStringSubmatch(
		string(read(t, "../../hostd/src/telemetry.rs")), -1)
	if len(hostd) == 0 {
		t.Fatal("no hostd metric names found in hostd/src/telemetry.rs")
	}
	for _, m := range hostd {
		known[m[1]] = true
	}
	return known
}

type ruleFile struct {
	Groups []struct {
		Name  string `yaml:"name"`
		Rules []struct {
			Record      string            `yaml:"record"`
			Alert       string            `yaml:"alert"`
			Expr        string            `yaml:"expr"`
			Labels      map[string]string `yaml:"labels"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

type dashboard struct {
	UID    string `json:"uid"`
	Title  string `json:"title"`
	Panels []struct {
		ID         int    `json:"id"`
		Type       string `json:"type"`
		Title      string `json:"title"`
		Datasource *struct {
			UID string `json:"uid"`
		} `json:"datasource"`
		Targets []struct {
			Expr       string `json:"expr"`
			RawSQL     string `json:"rawSql"`
			Datasource struct {
				UID string `json:"uid"`
			} `json:"datasource"`
		} `json:"targets"`
	} `json:"panels"`
}

func loadDashboards(t *testing.T) map[string]dashboard {
	paths, err := filepath.Glob("grafana/dashboards/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no dashboards: %v", err)
	}
	out := map[string]dashboard{}
	for _, p := range paths {
		var d dashboard
		if err := json.Unmarshal(read(t, p), &d); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		out[p] = d
	}
	return out
}

var (
	metricRef = regexp.MustCompile(`\btachyon_[a-z0-9_]+`)
	recordRef = regexp.MustCompile(`\b[a-z_]*:?tachyon[a-z0-9_]*(?::[a-z0-9_]+)+`)
)

// baseMetric strips the series suffixes Prometheus adds to histograms.
func baseMetric(s string) string {
	for _, suf := range []string{"_bucket", "_sum", "_count"} {
		if strings.HasSuffix(s, suf) && !strings.HasSuffix(s, "_total") {
			return strings.TrimSuffix(s, suf)
		}
	}
	return s
}

// checkExpr fails on any tachyon metric or recording rule an expression
// uses that nothing produces.
func checkExpr(t *testing.T, where, expr string, known, records map[string]bool) {
	t.Helper()
	for _, r := range recordRef.FindAllString(expr, -1) {
		if !records[r] {
			t.Errorf("%s: unknown recording rule %s", where, r)
		}
		expr = strings.ReplaceAll(expr, r, "")
	}
	for _, m := range metricRef.FindAllString(expr, -1) {
		if !known[baseMetric(m)] {
			t.Errorf("%s: unknown metric %s", where, m)
		}
	}
}

func loadRules(t *testing.T) (ruleFile, map[string]bool) {
	var rf ruleFile
	readYAML(t, "prometheus/rules/tachyon.rules.yml", &rf)
	records := map[string]bool{}
	for _, g := range rf.Groups {
		for _, r := range g.Rules {
			if r.Record != "" {
				if records[r.Record] {
					t.Errorf("duplicate recording rule %s", r.Record)
				}
				records[r.Record] = true
			}
		}
	}
	return rf, records
}

func TestRules(t *testing.T) {
	known := knownMetrics(t)
	rf, records := loadRules(t)
	alerts := map[string]bool{}
	for _, g := range rf.Groups {
		for _, r := range g.Rules {
			name := r.Record + r.Alert
			if (r.Record == "") == (r.Alert == "") || strings.TrimSpace(r.Expr) == "" {
				t.Errorf("%s/%s: need exactly one of record/alert, and an expr", g.Name, name)
			}
			checkExpr(t, "rule "+name, r.Expr, known, records)
			if r.Alert != "" {
				alerts[r.Alert] = true
				if sev := r.Labels["severity"]; sev != "page" && sev != "ticket" {
					t.Errorf("alert %s: severity %q", r.Alert, sev)
				}
				if r.Annotations["summary"] == "" || r.Annotations["description"] == "" {
					t.Errorf("alert %s: needs summary and description", r.Alert)
				}
			}
		}
	}
	// The three SLOs from docs/architecture.md must each be alerted on.
	for _, a := range []string{"TachyonPickupSLOFastBurn", "TachyonControlPlaneSLOFastBurn", "TachyonPickupLatencySLO"} {
		if !alerts[a] {
			t.Errorf("missing SLO alert %s", a)
		}
	}
}

func TestDashboards(t *testing.T) {
	known := knownMetrics(t)
	_, records := loadRules(t)
	var ds struct {
		Datasources []struct {
			UID  string `yaml:"uid"`
			Type string `yaml:"type"`
		} `yaml:"datasources"`
	}
	readYAML(t, "grafana/provisioning/datasources/tachyon.yaml", &ds)
	uids := map[string]bool{}
	for _, d := range ds.Datasources {
		uids[d.UID] = true
	}

	dashUIDs := map[string]bool{}
	for path, d := range loadDashboards(t) {
		if d.UID == "" || dashUIDs[d.UID] || d.Title == "" {
			t.Errorf("%s: missing or duplicate uid/title", path)
		}
		dashUIDs[d.UID] = true
		ids := map[int]bool{}
		for _, p := range d.Panels {
			if ids[p.ID] {
				t.Errorf("%s: duplicate panel id %d", path, p.ID)
			}
			ids[p.ID] = true
			if p.Type == "row" {
				continue
			}
			if len(p.Targets) == 0 {
				t.Errorf("%s: panel %q has no queries", path, p.Title)
			}
			for _, q := range p.Targets {
				if !uids[q.Datasource.UID] {
					t.Errorf("%s: panel %q uses unprovisioned datasource %q", path, p.Title, q.Datasource.UID)
				}
				if q.Expr == "" && q.RawSQL == "" {
					t.Errorf("%s: panel %q has an empty query", path, p.Title)
				}
				checkExpr(t, path+": "+p.Title, q.Expr, known, records)
				if q.RawSQL != "" && !strings.Contains(q.RawSQL, "tachyon.job_records") {
					t.Errorf("%s: panel %q does not query tachyon.job_records", path, p.Title)
				}
			}
		}
	}

	// The dashboards cover every fleet view the issue asks for.
	var all strings.Builder
	for _, d := range loadDashboards(t) {
		for _, p := range d.Panels {
			all.WriteString(strings.ToLower(p.Title) + "\n")
		}
	}
	for _, want := range []string{"pickup p95 by region and size", "prebuild hit rate", "cache hit rate",
		"cache restore throughput", "host cpu busy", "oversubscription slowdown", "cost per job-minute"} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("no dashboard panel for %q", want)
		}
	}
}

// TestClickHouseSchemaMatchesJobRow keeps the table in step with the rows
// the control plane inserts (JSONEachRow fails on unknown columns).
func TestClickHouseSchemaMatchesJobRow(t *testing.T) {
	sql := string(read(t, "clickhouse/001_job_records.sql"))
	start := strings.Index(sql, "CREATE TABLE IF NOT EXISTS tachyon.job_records")
	if start < 0 {
		t.Fatal("no CREATE TABLE tachyon.job_records")
	}
	body := sql[start:]
	body = body[strings.Index(body, "(")+1 : strings.Index(body, "\nENGINE")]
	col := regexp.MustCompile(`(?m)^\s*([a-z_]+)\s+[A-Z]`)
	var cols []string
	for _, m := range col.FindAllStringSubmatch(body, -1) {
		if m[1] != "inserted_at" {
			cols = append(cols, m[1])
		}
	}
	var fields []string
	rt := reflect.TypeFor[telemetry.JobRow]()
	for i := range rt.NumField() {
		fields = append(fields, strings.Split(rt.Field(i).Tag.Get("json"), ",")[0])
	}
	slices.Sort(cols)
	slices.Sort(fields)
	if !slices.Equal(cols, fields) {
		t.Errorf("columns %v\nJobRow   %v", cols, fields)
	}
}

func TestCollectorPipelines(t *testing.T) {
	var cfg struct {
		Receivers  map[string]any `yaml:"receivers"`
		Processors map[string]any `yaml:"processors"`
		Exporters  map[string]any `yaml:"exporters"`
		Service    struct {
			Pipelines map[string]struct {
				Receivers  []string `yaml:"receivers"`
				Processors []string `yaml:"processors"`
				Exporters  []string `yaml:"exporters"`
			} `yaml:"pipelines"`
		} `yaml:"service"`
	}
	readYAML(t, "otel-collector/config.yaml", &cfg)
	if _, ok := cfg.Service.Pipelines["metrics"]; !ok {
		t.Fatal("no metrics pipeline")
	}
	for name, p := range cfg.Service.Pipelines {
		for _, r := range p.Receivers {
			if _, ok := cfg.Receivers[r]; !ok {
				t.Errorf("pipeline %s: undefined receiver %s", name, r)
			}
		}
		for _, r := range p.Processors {
			if _, ok := cfg.Processors[r]; !ok {
				t.Errorf("pipeline %s: undefined processor %s", name, r)
			}
		}
		for _, r := range p.Exporters {
			if _, ok := cfg.Exporters[r]; !ok {
				t.Errorf("pipeline %s: undefined exporter %s", name, r)
			}
		}
	}
}

func TestPrometheusConfig(t *testing.T) {
	var cfg struct {
		RuleFiles     []string `yaml:"rule_files"`
		ScrapeConfigs []struct {
			JobName string `yaml:"job_name"`
		} `yaml:"scrape_configs"`
	}
	readYAML(t, "prometheus/prometheus.yml", &cfg)
	jobs := map[string]bool{}
	for _, s := range cfg.ScrapeConfigs {
		jobs[s.JobName] = true
	}
	// The control plane availability SLO reads probe_success for this job.
	if !jobs["tachyon-controlplane"] || !jobs["otel-collector"] {
		t.Errorf("scrape jobs = %v", jobs)
	}
	if len(cfg.RuleFiles) == 0 {
		t.Error("no rule_files")
	}
}
