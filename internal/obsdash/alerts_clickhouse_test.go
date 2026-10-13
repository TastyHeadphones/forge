package obsdash

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/alerting"
)

// TestDefaultAlertSQLOnClickHouse runs every generated default against a real
// ClickHouse holding the clickhouseexporter's tables, with seeded series for
// each case the SQL must get right: cumulative counters with a restart, delta
// counters, an unsampled error ratio, low traffic, a stopped service and a
// young one. The goldens pin the text; this pins what it MEANS.
//
// Set FORGE_ALERT_CLICKHOUSE to the name of a running container with
// clickhouse-client and the exporter's tables (password "pw"), e.g. the one
// internal/templates' ClickStack gate starts.
func TestDefaultAlertSQLOnClickHouse(t *testing.T) {
	container := os.Getenv("FORGE_ALERT_CLICKHOUSE")
	if testing.Short() || container == "" {
		t.Skip("needs a ClickHouse container; set FORGE_ALERT_CLICKHOUSE=<container>")
	}
	run := func(sql string) string {
		t.Helper()
		cmd := exec.Command("docker", "exec", "-i", container, "clickhouse-client", "--password", "pw",
			"--joined_subquery_requires_alias=0", "--multiquery", "--format", "TSV")
		cmd.Stdin = strings.NewReader(sql)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("clickhouse: %v\n%s\n--- sql ---\n%s", err, out, sql)
		}
		return strings.TrimSpace(string(out))
	}
	run(clickhouseSeed)

	in := AlertInput{
		Project: "shop", FrontendService: "shop-frontend",
		Services: []AlertService{
			{"api", "a.v1.S"}, {"quiet", "q.v1.Q"}, {"reset", "r.v1.R"}, {"delta", "d.v1.D"},
			{"fast", "f.v1.F"}, {"stopped", "s.v1.T"}, {"healthy", "h.v1.H"}, {"sparse", "p.v1.P"}, {"young", "y.v1.Y"},
		},
	}
	rules, err := ResolveAlertDefaults(in, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	fired := map[string][]string{}
	for _, r := range rules {
		out := run(r.Expr)
		if out != "" {
			for _, line := range strings.Split(out, "\n") {
				fired[r.ID] = append(fired[r.ID], strings.Split(line, "\t")[0])
			}
		}
	}
	want := map[string][]string{
		// api fails 20% of 200 calls, delta 10% of 60. quiet (3 calls in the window), reset (no errors) stay quiet.
		"error-rate": {"api", "delta"},
		// api's p95 sits in the (1s, 5s] bucket; fast's in the first.
		"p95-latency": {"api"},
		// stopped served traffic through most of the last day, then stopped. sparse (4 active hours) and young (2h) do not count as steady; healthy is serving.
		"service-silent":     {"stopped"},
		"frontend-js-errors": {"shop-frontend"},
	}
	for id, w := range want {
		if strings.Join(fired[id], ",") != strings.Join(w, ",") {
			t.Errorf("%s fired for %v, want %v", id, fired[id], w)
		}
	}

	// A different env sees none of dev's data.
	other, _ := ResolveAlertDefaults(in, "prod", nil)
	for _, r := range other {
		if r.ID == "service-silent" || r.ID == "frontend-js-errors" {
			continue
		}
		if out := run(r.Expr); strings.Contains(out, "api") && r.ID != "error-rate" {
			t.Errorf("%s in env prod saw dev's series: %s", r.ID, out)
		}
	}
}

const clickhouseSeed = `
TRUNCATE TABLE otel_metrics_sum; TRUNCATE TABLE otel_metrics_histogram; TRUNCATE TABLE otel_traces;

-- api: two replicas, cumulative. calls +20/min each (200 over the window), server errors +4/min each (40),
-- plus USER errors that must not count, plus an rpc service the project does not own (health).
INSERT INTO otel_metrics_sum (ResourceAttributes, ServiceName, MetricName, Attributes, TimeUnix, Value, AggregationTemporality, IsMonotonic)
SELECT map('deployment.environment.name','dev','service.instance.id',inst), 'api', 'rpc.server.calls', map('procedure','/a.v1.S/Get'), now() - INTERVAL (5-n) MINUTE, 100 + n*20, 2, true
FROM (SELECT arrayJoin(['i1','i2']) inst) AS a, (SELECT number n FROM numbers(6)) AS b;
INSERT INTO otel_metrics_sum (ResourceAttributes, ServiceName, MetricName, Attributes, TimeUnix, Value, AggregationTemporality, IsMonotonic)
SELECT map('deployment.environment.name','dev','service.instance.id',inst), 'api', 'rpc.server.errors', map('procedure','/a.v1.S/Get','code','internal','error_class','server'), now() - INTERVAL (5-n) MINUTE, 5 + n*4, 2, true
FROM (SELECT arrayJoin(['i1','i2']) inst) AS a, (SELECT number n FROM numbers(6)) AS b;
INSERT INTO otel_metrics_sum (ResourceAttributes, ServiceName, MetricName, Attributes, TimeUnix, Value, AggregationTemporality, IsMonotonic)
SELECT map('deployment.environment.name','dev','service.instance.id','i1'), 'api', 'rpc.server.errors', map('procedure','/a.v1.S/Get','code','not_found','error_class','user'), now() - INTERVAL (5-n) MINUTE, 50 + n*300, 2, true FROM (SELECT number n FROM numbers(6));
INSERT INTO otel_metrics_sum (ResourceAttributes, ServiceName, MetricName, Attributes, TimeUnix, Value, AggregationTemporality, IsMonotonic)
SELECT map('deployment.environment.name','dev','service.instance.id','h1'), 'api', 'rpc.server.errors', map('procedure','/grpc.health.v1.Health/Check','error_class','server'), now() - INTERVAL (5-n) MINUTE, n*1000, 2, true FROM (SELECT number n FROM numbers(6));
-- quiet: 3 calls and 1 error in the window: a ratio of 33% on no traffic
INSERT INTO otel_metrics_sum (ResourceAttributes, ServiceName, MetricName, Attributes, TimeUnix, Value, AggregationTemporality, IsMonotonic)
SELECT map('deployment.environment.name','dev','service.instance.id','q1'), 'quiet', 'rpc.server.calls', map('procedure','/q.v1.Q/Get'), now() - INTERVAL (5-n) MINUTE, 10 + intDiv(n,2), 2, true FROM (SELECT number n FROM numbers(6));
INSERT INTO otel_metrics_sum (ResourceAttributes, ServiceName, MetricName, Attributes, TimeUnix, Value, AggregationTemporality, IsMonotonic)
SELECT map('deployment.environment.name','dev','service.instance.id','q1'), 'quiet', 'rpc.server.errors', map('procedure','/q.v1.Q/Get','code','internal','error_class','server'), now() - INTERVAL (5-n) MINUTE, 1 + intDiv(n,5), 2, true FROM (SELECT number n FROM numbers(6));
-- reset: a restart mid-window (100,120,140,5,25,45): 85 calls, no errors
INSERT INTO otel_metrics_sum (ResourceAttributes, ServiceName, MetricName, Attributes, TimeUnix, Value, AggregationTemporality, IsMonotonic)
SELECT map('deployment.environment.name','dev','service.instance.id','i3'), 'reset', 'rpc.server.calls', map('procedure','/r.v1.R/Get'), now() - INTERVAL (5-n) MINUTE, [100,120,140,5,25,45][n+1], 2, true FROM (SELECT number n FROM numbers(6));
-- delta temporality: 60 calls, 6 errors (10%)
INSERT INTO otel_metrics_sum (ResourceAttributes, ServiceName, MetricName, Attributes, TimeUnix, Value, AggregationTemporality, IsMonotonic)
SELECT map('deployment.environment.name','dev','service.instance.id','d1'), 'delta', 'rpc.server.calls', map('procedure','/d.v1.D/Get'), now() - INTERVAL (5-n) MINUTE, 10, 1, true FROM (SELECT number n FROM numbers(6));
INSERT INTO otel_metrics_sum (ResourceAttributes, ServiceName, MetricName, Attributes, TimeUnix, Value, AggregationTemporality, IsMonotonic)
SELECT map('deployment.environment.name','dev','service.instance.id','d1'), 'delta', 'rpc.server.errors', map('procedure','/d.v1.D/Get','error_class','server'), now() - INTERVAL (5-n) MINUTE, 1, 1, true FROM (SELECT number n FROM numbers(6));
-- the same errors in another env must not count for dev
INSERT INTO otel_metrics_sum (ResourceAttributes, ServiceName, MetricName, Attributes, TimeUnix, Value, AggregationTemporality, IsMonotonic)
SELECT map('deployment.environment.name','prod','service.instance.id','p1'), 'healthy', 'rpc.server.errors', map('procedure','/h.v1.H/Get','error_class','server'), now() - INTERVAL (5-n) MINUTE, n*1000, 2, true FROM (SELECT number n FROM numbers(6));

-- silence: stopped = counter moved every 5 min from 25h ago to 30 min ago; healthy moves to now;
-- sparse = four active hours; young = 2h of history that ended 30 min ago.
INSERT INTO otel_metrics_sum (ResourceAttributes, ServiceName, MetricName, Attributes, TimeUnix, Value, AggregationTemporality, IsMonotonic)
SELECT map('deployment.environment.name','dev','service.instance.id','s1'), 'stopped', 'rpc.server.calls', map('procedure','/s.v1.T/Get'), now() - INTERVAL (n*5) MINUTE, 1000 - n, 2, true FROM (SELECT number n FROM numbers(6, 294));
INSERT INTO otel_metrics_sum (ResourceAttributes, ServiceName, MetricName, Attributes, TimeUnix, Value, AggregationTemporality, IsMonotonic)
SELECT map('deployment.environment.name','dev','service.instance.id','h1'), 'healthy', 'rpc.server.calls', map('procedure','/h.v1.H/Get'), now() - INTERVAL (n*5) MINUTE, 1000 - n, 2, true FROM (SELECT number n FROM numbers(0, 300));
INSERT INTO otel_metrics_sum (ResourceAttributes, ServiceName, MetricName, Attributes, TimeUnix, Value, AggregationTemporality, IsMonotonic)
SELECT map('deployment.environment.name','dev','service.instance.id','p1'), 'sparse', 'rpc.server.calls', map('procedure','/p.v1.P/Get'), now() - INTERVAL (n*60) MINUTE, 50 - n, 2, true FROM (SELECT arrayJoin([3,5,9,14]) n);
INSERT INTO otel_metrics_sum (ResourceAttributes, ServiceName, MetricName, Attributes, TimeUnix, Value, AggregationTemporality, IsMonotonic)
SELECT map('deployment.environment.name','dev','service.instance.id','y1'), 'young', 'rpc.server.calls', map('procedure','/y.v1.Y/Get'), now() - INTERVAL (n*5) MINUTE, 100 - n, 2, true FROM (SELECT number n FROM numbers(6, 18));

-- latency: api replicas add 10 (<=0.1s) 5 (<=0.5s) 3 (<=1s) 2 (<=5s) per minute; fast is all under 0.1s
INSERT INTO otel_metrics_histogram (ResourceAttributes, ServiceName, MetricName, Attributes, TimeUnix, Count, Sum, BucketCounts, ExplicitBounds, AggregationTemporality)
SELECT map('deployment.environment.name','dev','service.instance.id',inst), 'api', 'rpc.server.duration', map('procedure','/a.v1.S/Get'), now() - INTERVAL (5-n) MINUTE,
       20*(n+1), 0, [10*(n+1),5*(n+1),3*(n+1),2*(n+1),0], [0.1,0.5,1,5], 2
FROM (SELECT arrayJoin(['i1','i2']) inst) AS a, (SELECT number n FROM numbers(6)) AS b;
INSERT INTO otel_metrics_histogram (ResourceAttributes, ServiceName, MetricName, Attributes, TimeUnix, Count, Sum, BucketCounts, ExplicitBounds, AggregationTemporality)
SELECT map('deployment.environment.name','dev','service.instance.id','f1'), 'fast', 'rpc.server.duration', map('procedure','/f.v1.F/Get'), now() - INTERVAL (5-n) MINUTE,
       30*(n+1), 0, [30*(n+1),0,0,0,0], [0.1,0.5,1,5], 2 FROM (SELECT number n FROM numbers(6));

-- frontend: ~2 errors per 10 min for an hour, then 40 in the last 10 minutes. fetch failures and page loads are not JS errors.
INSERT INTO otel_traces (Timestamp, TraceId, SpanId, SpanName, SpanKind, ServiceName, ResourceAttributes, SpanAttributes, Duration, StatusCode)
SELECT now() - INTERVAL (n*5) MINUTE - INTERVAL 20 SECOND, 't', concat('b', toString(n)), 'onerror', 'Internal', 'shop-frontend', map('deployment.environment.name','dev','rum.sessionId','s'), map(), 1, 'Error' FROM (SELECT number n FROM numbers(2, 12));
INSERT INTO otel_traces (Timestamp, TraceId, SpanId, SpanName, SpanKind, ServiceName, ResourceAttributes, SpanAttributes, Duration, StatusCode)
SELECT now() - INTERVAL 1 MINUTE - INTERVAL n SECOND, 't', concat('c', toString(n)), 'onerror', 'Internal', 'shop-frontend', map('deployment.environment.name','dev','rum.sessionId','s'), map(), 1, 'Error' FROM (SELECT number n FROM numbers(40));
INSERT INTO otel_traces (Timestamp, TraceId, SpanId, SpanName, SpanKind, ServiceName, ResourceAttributes, SpanAttributes, Duration, StatusCode)
SELECT now() - INTERVAL 1 MINUTE - INTERVAL n SECOND, 't', concat('d', toString(n)), 'HTTP GET', 'Client', 'shop-frontend', map('deployment.environment.name','dev','rum.sessionId','s'), map('component','fetch'), 1, 'Error' FROM (SELECT number n FROM numbers(100));
INSERT INTO otel_traces (Timestamp, TraceId, SpanId, SpanName, SpanKind, ServiceName, ResourceAttributes, SpanAttributes, Duration, StatusCode)
SELECT now() - INTERVAL 1 MINUTE - INTERVAL n SECOND, 't', concat('e', toString(n)), 'documentLoad', 'Internal', 'shop-frontend', map('deployment.environment.name','dev','rum.sessionId','s'), map(), 1, 'Unset' FROM (SELECT number n FROM numbers(500));
`

// dockerQuerier is an alerting.Querier that runs inside the ClickHouse container
// with the evaluator's own guard settings, as `forge observe alerts check` does.
type dockerQuerier struct{ container string }

func (q dockerQuerier) Query(ctx context.Context, _ string, query string, opts alerting.QueryOptions) ([]alerting.Row, error) {
	if err := alerting.ValidateExpr(query); err != nil {
		return nil, err
	}
	args := []string{"exec", "-i", q.container, "clickhouse-client", "--password", "pw", "--format", "JSONEachRow",
		"--output_format_json_quote_64bit_integers=0", "--query", query}
	for k, v := range opts.Settings() {
		args = append(args, fmt.Sprintf("--%s=%v", k, v))
	}
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, out)
	}
	var rows []alerting.Row
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var r map[string]any
		if err := dec.Decode(&r); err != nil {
			return nil, err
		}
		rows = append(rows, alerting.Row(r))
	}
	return rows, nil
}

type nopNotifier struct{}

func (nopNotifier) Notify(context.Context, []alerting.Alert) error { return nil }

// TestDefaultAlertsThroughTheEvaluator drives the generated defaults through
// pkg/alerting's real Evaluator: the guard accepts every expr, the readonly and
// row-cap settings are accepted by ClickHouse, and the labels the evaluator
// derives from the result columns are the ones the rules promise.
func TestDefaultAlertsThroughTheEvaluator(t *testing.T) {
	container := os.Getenv("FORGE_ALERT_CLICKHOUSE")
	if testing.Short() || container == "" {
		t.Skip("needs a ClickHouse container; set FORGE_ALERT_CLICKHOUSE=<container>")
	}
	cmd := exec.Command("docker", "exec", "-i", container, "clickhouse-client", "--password", "pw", "--joined_subquery_requires_alias=0", "--multiquery")
	cmd.Stdin = strings.NewReader(clickhouseSeed)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed: %v\n%s", err, out)
	}
	in := AlertInput{FrontendService: "shop-frontend", Services: []AlertService{
		{"api", "a.v1.S"}, {"quiet", "q.v1.Q"}, {"reset", "r.v1.R"}, {"delta", "d.v1.D"}, {"fast", "f.v1.F"},
		{"stopped", "s.v1.T"}, {"healthy", "h.v1.H"}, {"sparse", "p.v1.P"}, {"young", "y.v1.Y"}}}
	rules, err := ResolveAlertDefaults(in, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := alerting.NewEvaluator(alerting.EvaluatorDeps{
		Querier: dockerQuerier{container}, Notifier: nopNotifier{}, Store: alerting.NewMemoryStore(),
		Limits: alerting.Limits{MaxExecutionTime: 20 * time.Second, MaxInstancesPerRule: 200},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, r := range rules {
		spec := alerting.RuleSpec{Alert: r.Alert, Expr: r.Expr, Labels: r.Labels, Annotations: r.Annotations, Source: alerting.SourceDefault}
		if err := spec.Validate(); err != nil {
			t.Fatalf("%s: the evaluator's own validation refuses the default: %v", r.ID, err)
		}
		after, err := ev.EvaluateRule(context.Background(), spec.Rule("t", "dev", nil), nil)
		if err != nil {
			t.Fatalf("%s: %v", r.ID, err)
		}
		for _, inst := range after {
			if inst.Labels["alertname"] != r.Alert || inst.Labels["severity"] != r.Labels["severity"] {
				t.Errorf("%s: instance labels %v lack alertname/severity", r.ID, inst.Labels)
			}
			got[r.ID] = append(got[r.ID], inst.Labels["service"])
		}
		sort.Strings(got[r.ID])
	}
	want := map[string]string{
		"error-rate": "api,delta", "p95-latency": "api", "service-silent": "stopped", "frontend-js-errors": "shop-frontend",
	}
	for id, w := range want {
		if strings.Join(got[id], ",") != w {
			t.Errorf("%s: instances for services %v, want %s", id, got[id], w)
		}
	}
}
