package obsdash

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// AlertDefaultsVersion identifies the serialized shape of AlertDefaultsFile.
const AlertDefaultsVersion = "forge.alert-defaults/v1"

// AlertDefaultsFile is the project-relative file `forge generate` writes: the
// service metadata the default alert rules are derived from, plus the catalog
// of defaults with their tunable parameters. `forge env shape` reads it back to
// resolve the defaults for one env (the SQL names the env, so a default is
// rendered per env, not once per project). It sits beside the dashboard spec,
// not in the dashboards directory the HyperDX provisioner scans.
const AlertDefaultsFile = "deploy/observability/alert-defaults_gen.json"

// Alert severities, the values of the `severity` label.
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
	SeverityInfo     = "info"
)

// AlertService is one Connect service the defaults watch. RPCService is the
// procedure prefix the RPC interceptors label metrics with:
// "<package>.<ProtoService>", the second segment of "/orders.v1.OrdersService/GetOrder".
type AlertService struct {
	Name       string `json:"name"`
	RPCService string `json:"rpc_service"`
}

// AlertInput is what the defaults are derived from.
type AlertInput struct {
	Project string `json:"project"`
	// Services are the project's services that have at least one rpc.
	Services []AlertService `json:"services"`
	// FrontendService is the browser SDK's service.name ("<project>-frontend"),
	// empty when the project has no web frontend.
	FrontendService string `json:"frontend_service,omitempty"`
}

// AlertInputOf derives the defaults' input from the same Input the dashboards
// are built from, so the two can never disagree about what a project has.
func AlertInputOf(in Input) AlertInput {
	out := AlertInput{Project: in.Project, Services: []AlertService{}}
	for _, s := range in.Services {
		if len(s.Methods) == 0 {
			continue
		}
		out.Services = append(out.Services, AlertService{Name: s.Name, RPCService: s.Package + "." + s.ProtoService})
	}
	sort.Slice(out.Services, func(i, j int) bool { return out.Services[i].Name < out.Services[j].Name })
	if len(in.Frontends) > 0 {
		out.FrontendService = FrontendServiceName(in.Project)
	}
	return out
}

// AlertParam is one numeric knob of a default's query.
type AlertParam struct {
	Name    string  `json:"name"`
	Default float64 `json:"default"`
	Doc     string  `json:"doc"`
}

// AlertDefaultInfo is a default as the generated file records it.
type AlertDefaultInfo struct {
	ID       string       `json:"id"`
	Alert    string       `json:"alert"`
	Title    string       `json:"title"`
	Severity string       `json:"severity"`
	For      string       `json:"for"`
	Interval string       `json:"interval"`
	Signal   string       `json:"signal"`
	Params   []AlertParam `json:"params"`
}

// AlertDefaultsDoc is the serialized AlertDefaultsFile.
type AlertDefaultsDoc struct {
	Version  string             `json:"version"`
	Input    AlertInput         `json:"input"`
	Defaults []AlertDefaultInfo `json:"defaults"`
}

// AlertTuning is how an env adjusts one default (forge.AlertDefault).
type AlertTuning struct {
	Enabled bool
	Labels  map[string]string
	For     string
	Params  map[string]float64
}

// AlertRule is one resolved default: a rule ready for an env's shape.
type AlertRule struct {
	ID          string
	Alert       string
	Expr        string
	For         string
	Interval    string
	Labels      map[string]string
	Annotations map[string]string
}

type alertDef struct {
	AlertDefaultInfo
	annotations map[string]string
	applies     func(AlertInput) bool
	sql         func(env string, in AlertInput, p map[string]float64) string
}

const (
	pWindow      = "window_minutes"
	pMinRequests = "min_requests"
)

// The defaults. Where the signal is rpc traffic they read the RPC server
// METRICS (rpc.server.calls / .errors / .duration, pkg/observe), never spans:
// metrics are not sampled, while the gateway's tail sampler keeps all error
// traces and a fraction of the rest, so a span count of healthy requests
// reports a fraction of the traffic and a ratio of the two is wrong. The one
// span-based default (frontend errors) counts ERROR spans only, which the tail
// sampler always keeps, and compares them to themselves, so sampling cancels.
// The gateway writes no sample-rate attribute, so there is nothing to weight a
// non-error span count by: see the observability/alerts skill.
var alertDefs = []alertDef{
	{
		AlertDefaultInfo: AlertDefaultInfo{
			ID: "error-rate", Alert: "ServiceErrorRateHigh", Title: "Service error rate above 5%",
			Severity: SeverityCritical, For: "5m", Interval: "1m", Signal: "metrics",
			Params: []AlertParam{
				{"threshold", 0.05, "error ratio (0..1) that fires"},
				{pWindow, 5, "minutes the ratio is computed over"},
				{pMinRequests, 20, "fewer requests than this in the window never fire (a ratio of 1 call is noise)"},
			},
		},
		annotations: map[string]string{"summary": "{{ $labels.service }} fails {{ $value }} of its requests (server errors / all requests)"},
		applies:     func(in AlertInput) bool { return len(in.Services) > 0 },
		sql:         errorRateSQL,
	},
	{
		AlertDefaultInfo: AlertDefaultInfo{
			ID: "p95-latency", Alert: "ServiceLatencyP95High", Title: "Service p95 latency above 1s",
			Severity: SeverityWarning, For: "10m", Interval: "1m", Signal: "metrics",
			Params: []AlertParam{
				{"threshold_seconds", 1, "p95 latency in seconds that fires"},
				{pWindow, 5, "minutes the percentile is computed over"},
				{pMinRequests, 20, "fewer requests than this in the window never fire"},
			},
		},
		annotations: map[string]string{"summary": "{{ $labels.service }} p95 latency is {{ $value }}s"},
		applies:     func(in AlertInput) bool { return len(in.Services) > 0 },
		sql:         latencySQL,
	},
	{
		AlertDefaultInfo: AlertDefaultInfo{
			ID: "service-silent", Alert: "ServiceSilent", Title: "A service with steady traffic went quiet",
			Severity: SeverityWarning, For: "0s", Interval: "1m", Signal: "metrics",
			Params: []AlertParam{
				{"silent_minutes", 10, "minutes without a request before it fires"},
				{"min_active_hours", 18, "of the previous 24h, the hours the service must have served requests in to count as steady (a quiet staging service never pages)"},
			},
		},
		annotations: map[string]string{"summary": "{{ $labels.service }} has served no requests for {{ $value }}s, after serving traffic through most of the last day"},
		applies:     func(in AlertInput) bool { return len(in.Services) > 0 },
		sql:         silentSQL,
	},
	{
		AlertDefaultInfo: AlertDefaultInfo{
			ID: "frontend-js-errors", Alert: "FrontendJSErrorSpike", Title: "Frontend JS errors spiking against the 1h baseline",
			Severity: SeverityWarning, For: "10m", Interval: "1m", Signal: "traces",
			Params: []AlertParam{
				{"spike_factor", 3, "times the baseline rate that fires"},
				{pWindow, 10, "minutes of the current window"},
				{"baseline_minutes", 60, "minutes before the window that set the baseline rate"},
				{"min_errors", 10, "fewer errors than this in the window never fire"},
			},
		},
		annotations: map[string]string{"summary": "{{ $labels.service }} reported {{ $value }} JS errors in the current window, over the spike factor times its baseline"},
		applies:     func(in AlertInput) bool { return in.FrontendService != "" },
		sql:         frontendErrorsSQL,
	},
}

// AlertDefaultIDs are the ids a forge.Alerts.defaults map may tune.
func AlertDefaultIDs() []string {
	ids := make([]string, 0, len(alertDefs))
	for _, d := range alertDefs {
		ids = append(ids, d.ID)
	}
	return ids
}

// AlertDefaultAlertNames are the rule names the defaults take: a user rule may
// not reuse one.
func AlertDefaultAlertNames() []string {
	names := make([]string, 0, len(alertDefs))
	for _, d := range alertDefs {
		names = append(names, d.Alert)
	}
	return names
}

// ValidateAlertTuning refuses a tuning that names an unknown default or an
// unknown parameter of one, so a typo is an error at render, not a default
// that silently keeps its old threshold.
func ValidateAlertTuning(tuning map[string]AlertTuning) error {
	ids := map[string]alertDef{}
	for _, d := range alertDefs {
		ids[d.ID] = d
	}
	keys := make([]string, 0, len(tuning))
	for id := range tuning {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	for _, id := range keys {
		d, ok := ids[id]
		if !ok {
			return fmt.Errorf("alerts.defaults names %q, which is not a generated default (have: %s)", id, strings.Join(AlertDefaultIDs(), ", "))
		}
		known := map[string]bool{}
		var names []string
		for _, p := range d.Params {
			known[p.Name] = true
			names = append(names, p.Name)
		}
		pk := make([]string, 0, len(tuning[id].Params))
		for name := range tuning[id].Params {
			pk = append(pk, name)
		}
		sort.Strings(pk)
		for _, name := range pk {
			if !known[name] {
				return fmt.Errorf("alerts.defaults[%q].params has %q, which %s does not take (params: %s)", id, name, d.ID, strings.Join(names, ", "))
			}
			if v := tuning[id].Params[name]; v <= 0 {
				return fmt.Errorf("alerts.defaults[%q].params[%q] = %v: a threshold or window must be positive", id, name, v)
			}
		}
	}
	return nil
}

// AlertDefaults is the catalog for the generated file: every default the input
// applies to, at its default parameters.
func AlertDefaults(in AlertInput) AlertDefaultsDoc {
	doc := AlertDefaultsDoc{Version: AlertDefaultsVersion, Input: in, Defaults: []AlertDefaultInfo{}}
	for _, d := range alertDefs {
		if d.applies(in) {
			doc.Defaults = append(doc.Defaults, d.AlertDefaultInfo)
		}
	}
	return doc
}

// MarshalAlertDefaults is the file's bytes.
func MarshalAlertDefaults(in AlertInput) ([]byte, error) {
	b, err := json.MarshalIndent(AlertDefaults(in), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ParseAlertDefaults reads the generated file.
func ParseAlertDefaults(b []byte) (AlertInput, error) {
	var doc AlertDefaultsDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return AlertInput{}, fmt.Errorf("%s: %w", AlertDefaultsFile, err)
	}
	if doc.Version != AlertDefaultsVersion {
		return AlertInput{}, fmt.Errorf("%s: version %q, this forge reads %q: run `forge generate`", AlertDefaultsFile, doc.Version, AlertDefaultsVersion)
	}
	return doc.Input, nil
}

// ResolveAlertDefaults renders the defaults for one env, with the env's tuning
// applied. A default the env disables is left out. Rules come back sorted by
// alert name.
func ResolveAlertDefaults(in AlertInput, env string, tuning map[string]AlertTuning) ([]AlertRule, error) {
	if err := ValidateAlertTuning(tuning); err != nil {
		return nil, err
	}
	var out []AlertRule
	for _, d := range alertDefs {
		if !d.applies(in) {
			continue
		}
		t, tuned := tuning[d.ID]
		if tuned && !t.Enabled {
			continue
		}
		params := map[string]float64{}
		for _, p := range d.Params {
			params[p.Name] = p.Default
		}
		for k, v := range t.Params {
			params[k] = v
		}
		r := AlertRule{
			ID: d.ID, Alert: d.Alert, Expr: d.sql(env, in, params),
			For: d.For, Interval: d.Interval,
			Labels:      map[string]string{"severity": d.Severity},
			Annotations: map[string]string{},
		}
		for k, v := range d.annotations {
			r.Annotations[k] = v
		}
		if t.For != "" {
			r.For = t.For
		}
		for k, v := range t.Labels {
			r.Labels[k] = v
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alert < out[j].Alert })
	return out, nil
}

// ── SQL ──────────────────────────────────────────────────────────────────────
//
// Native ClickHouse SQL against the OpenTelemetry tables the clickhouseexporter
// creates (otel_metrics_sum, otel_metrics_histogram, otel_traces). Every
// statement returns one row per firing instance: the label columns, then
// `value`, and only when the condition holds. Counters are cumulative by
// default, so each series is turned into its increase over the window from the
// differences of consecutive points, treating a drop as a restart (the new
// value is the increase since zero). Series are grouped by their full
// attribute set first, so two replicas never subtract each other.

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func sqlQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func rpcServiceList(in AlertInput) string {
	q := make([]string, len(in.Services))
	for i, s := range in.Services {
		q[i] = sqlQuote(s.RPCService)
	}
	return strings.Join(q, ", ")
}

// windowFilter keeps the points a window's increase needs: a cumulative series
// needs one point BEFORE the window to difference against (two minutes back
// covers the SDK's default 60s export interval with one missed export to
// spare), a delta series does not (its points are already increases).
func windowFilter(w float64) string {
	return fmt.Sprintf("((AggregationTemporality = 1 AND TimeUnix >= now() - INTERVAL %s MINUTE) OR (AggregationTemporality != 1 AND TimeUnix >= now() - INTERVAL %s MINUTE - INTERVAL 2 MINUTE))", num(w), num(w))
}

func errorRateSQL(env string, in AlertInput, p map[string]float64) string {
	return fmt.Sprintf(`WITH series AS (
    SELECT ServiceName AS service,
           MetricName AS metric,
           Attributes['error_class'] AS error_class,
           arrayMap(x -> x.2, arraySort(x -> x.1, groupArray((TimeUnix, Value)))) AS vals,
           any(AggregationTemporality) AS temporality
    FROM otel_metrics_sum
    WHERE MetricName IN ('rpc.server.calls', 'rpc.server.errors')
      AND %s
      AND ResourceAttributes['deployment.environment.name'] = %s
      AND splitByChar('/', Attributes['procedure'])[2] IN (%s)
    GROUP BY ServiceName, ResourceAttributes, Attributes, MetricName
), increase AS (
    SELECT service, metric, error_class,
           if(temporality = 1, arraySum(vals), arraySum((v, d) -> if(d < 0, v, d), vals, arrayDifference(vals))) AS n
    FROM series
)
SELECT service,
       sumIf(n, metric = 'rpc.server.errors' AND error_class NOT IN ('user', 'canceled')) / sumIf(n, metric = 'rpc.server.calls') AS value
FROM increase
GROUP BY service
HAVING sumIf(n, metric = 'rpc.server.calls') >= %s AND value > %s
ORDER BY service`, windowFilter(p[pWindow]), sqlQuote(env), rpcServiceList(in), num(p[pMinRequests]), num(p["threshold"]))
}

func latencySQL(env string, in AlertInput, p map[string]float64) string {
	return fmt.Sprintf(`WITH series AS (
    SELECT ServiceName AS service,
           ExplicitBounds AS bounds,
           arrayMap(x -> x.2, arraySort(x -> x.1, groupArray((TimeUnix, arrayMap(c -> toFloat64(c), BucketCounts))))) AS snaps,
           any(AggregationTemporality) AS temporality
    FROM otel_metrics_histogram
    WHERE MetricName = 'rpc.server.duration'
      AND %s
      AND ResourceAttributes['deployment.environment.name'] = %s
      AND splitByChar('/', Attributes['procedure'])[2] IN (%s)
    GROUP BY ServiceName, ResourceAttributes, Attributes, ExplicitBounds
), increase AS (
    SELECT service, bounds,
           if(temporality = 1,
              arrayReduce('sumForEach', snaps),
              arrayReduce('sumForEach', arrayMap((cur, prev) -> arrayMap((c, q) -> if(c < q, c, c - q), cur, prev),
                                                 arraySlice(snaps, 2), arraySlice(snaps, 1, length(snaps) - 1)))) AS inc
    FROM series
    WHERE length(snaps) >= 2 OR temporality = 1
), merged AS (
    SELECT service, bounds, sumForEach(inc) AS counts FROM increase GROUP BY service, bounds
)
SELECT service,
       quantilePrometheusHistogram(0.95)(le, cum) AS value,
       max(cum) AS requests
FROM (
    SELECT service,
           arrayJoin(arrayZip(arrayPushBack(bounds, inf), arrayCumSum(counts))) AS pair,
           pair.1 AS le, pair.2 AS cum
    FROM merged
)
GROUP BY service
HAVING requests >= %s AND value > %s
ORDER BY service`, windowFilter(p[pWindow]), sqlQuote(env), rpcServiceList(in), num(p[pMinRequests]), num(p["threshold_seconds"]))
}

func silentSQL(env string, in AlertInput, p map[string]float64) string {
	return fmt.Sprintf(`WITH moves AS (
    SELECT service, arrayJoin(t) AS t
    FROM (
        SELECT ServiceName AS service,
               arraySort(x -> x.1, groupArray((TimeUnix, Value))) AS pts,
               arrayMap(x -> x.1, pts) AS ts,
               arrayMap(x -> x.2, pts) AS vs,
               if(any(AggregationTemporality) = 1, vs, arrayDifference(vs)) AS moved,
               arrayFilter((tm, d) -> d != 0, ts, moved) AS t
        FROM otel_metrics_sum
        WHERE MetricName = 'rpc.server.calls'
          AND TimeUnix >= now() - INTERVAL 24 HOUR - INTERVAL %s MINUTE - INTERVAL 1 MINUTE
          AND ResourceAttributes['deployment.environment.name'] = %s
          AND splitByChar('/', Attributes['procedure'])[2] IN (%s)
        GROUP BY ServiceName, ResourceAttributes, Attributes
    )
)
SELECT service, dateDiff('second', max(t), now()) AS value
FROM moves
GROUP BY service
HAVING max(t) < now() - INTERVAL %s MINUTE
   AND uniqExactIf(toStartOfHour(t), t < now() - INTERVAL %s MINUTE) >= %s
ORDER BY service`, num(p["silent_minutes"]), sqlQuote(env), rpcServiceList(in), num(p["silent_minutes"]), num(p["silent_minutes"]), num(p["min_active_hours"]))
}

func frontendErrorsSQL(env string, in AlertInput, p map[string]float64) string {
	return fmt.Sprintf(`SELECT ServiceName AS service,
       countIf(Timestamp >= now() - INTERVAL %[1]s MINUTE) AS value
FROM otel_traces
WHERE ServiceName = %[2]s
  AND Timestamp >= now() - INTERVAL %[1]s MINUTE - INTERVAL %[3]s MINUTE
  AND ResourceAttributes['deployment.environment.name'] = %[4]s
  AND ResourceAttributes['rum.sessionId'] != ''
  AND lower(StatusCode) = 'error'
  AND SpanAttributes['component'] NOT IN ('fetch', 'xml-http-request')
GROUP BY ServiceName
HAVING value >= %[5]s
   AND value > %[6]s * countIf(Timestamp < now() - INTERVAL %[1]s MINUTE) * %[1]s / %[3]s
ORDER BY service`, num(p[pWindow]), sqlQuote(in.FrontendService), num(p["baseline_minutes"]), sqlQuote(env), num(p["min_errors"]), num(p["spike_factor"]))
}
