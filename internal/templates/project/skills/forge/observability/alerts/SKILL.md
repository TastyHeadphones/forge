---
name: observability/alerts
description: Alert rules declared in KCL per env — `forge.AlertRule` (Prometheus rule shape, the condition inside a native ClickHouse SQL `expr`), `forge.AlertRoute` (labels to a receiver NAME), `forge.AlertDefault` (tune or disable forge's four generated defaults), what render refuses, and what lands in the release shape for the control plane.
---

# Alerts

An env **declares** its alert rules in `deploy/kcl/<env>/main.k`, on `forge.Observability.alerts`. Forge validates them at render, resolves them together with its generated defaults, and records the result in the release's declared shape (`shape.alerts`, `shape.alert_routes`), the same path as `control_plane_secrets`. The control plane reads that record and evaluates the rules, so promoting a release applies its rules and rolling back restores the previous ones. Nothing is created by hand in a UI and no receiver secret is in the repo.

```python
observability = forge.Observability {
    local_port = _otlp_http_port
    alerts = forge.Alerts {
        rules = [
            forge.AlertRule {
                alert = "OrdersBacklog"
                expr = """
SELECT ResourceAttributes['service.name'] AS service, max(Value) AS value
FROM otel_metrics_gauge
WHERE MetricName = 'orders.backlog' AND TimeUnix >= now() - INTERVAL 5 MINUTE
GROUP BY service HAVING value > 500"""
                "for" = "10m"
                labels = {severity = "critical", team = "fulfilment"}
                annotations = {summary = "{{ $labels.service }} backlog is {{ $value }}"}
            }
        ]
        defaults = {"service-silent": forge.AlertDefault {enabled = False}}
        routes = [
            forge.AlertRoute {match = {severity = "critical"}, receiver = "pagerduty-oncall", continue = True}
            forge.AlertRoute {match = {}, receiver = "slack"}
        ]
    }
}
```

## A rule is a SELECT

Rules follow the Prometheus shape, with the condition **inside** `expr`: `alert`, `expr`, `for`, `interval`, `keep_firing_for`, `labels`, `annotations`. There is no separate query/condition object.

- `expr` is **one ClickHouse SELECT** (or `WITH … SELECT`) over the OpenTelemetry tables: `otel_metrics_sum`, `otel_metrics_gauge`, `otel_metrics_histogram`, `otel_traces`, `otel_logs`. The tenant database is already selected.
- **Every row it returns is a firing instance.** Columns other than `value` become the instance's labels; `value` (numeric) is the sample. Put the threshold in the query: `… GROUP BY service HAVING value > 0.05`. A query that returns no rows is healthy.
- `for` holds the condition before it fires (default `5m`). It is a KCL keyword, so write it quoted: `"for" = "10m"`.
- `interval` is the evaluation period, floor `1m`. `keep_firing_for` holds a cleared alert.
- `severity` is a label: `critical | warning | info`, default `warning`. Add any other labels you route on.
- `annotations` expand `{{ $labels.<col> }}` and `{{ $value }}`, nothing more.

## What render refuses

Each is an error naming the rule, at `forge env render` / `shape` / `deploy`, not at the first evaluation:

| Refused | Why |
|---|---|
| missing `alert` or `expr`; a name that is not `[A-Za-z_][A-Za-z0-9_]*` | required fields |
| `for` / `interval` / `keep_firing_for` not a duration (`90s`, `5m`, `1h30m`; no `d` or `w`) | the evaluator parses them with Go |
| `interval` under `1m` | evaluation floor |
| `expr` not starting with SELECT/WITH, or containing `;` | one read-only statement |
| labels `alertname`, `reliant_*` | set by the platform, so a rule cannot impersonate its tenant |
| `severity` other than critical / warning / info | |
| two rules with one `alert` in an env; a rule named like a default | unique per env |
| a route `receiver` that is a URL, a token, or contains `hooks.slack.com` | a route names a receiver; its secret is entered in the control plane console |
| routes with no empty-`match` route | every alert must have somewhere to go |
| `defaults` naming an unknown default or param | a typo would silently keep the old threshold |
| `alerts` on an env with `enabled = False` | nothing to evaluate |

The expr check is syntactic. The evaluator re-checks it and runs it read-only with a timeout and row cap; the database grants are the real boundary.

## Routing

`forge.AlertRoute {match, receiver, continue}`: an alert goes to the first route whose `match` labels it carries; `continue = True` keeps matching so one alert can reach several receivers. A **receiver is a name**, resolved by the control plane to a Slack channel, PagerDuty service and so on. With **no routes declared** the shape records `reliant-inapp` then `slack`: in-app plus Slack, **never a pager**. Declaring routes replaces that, and you own paging.

## The four generated defaults

`forge generate` writes `deploy/observability/alert-defaults_gen.json` (the services and frontend the defaults are derived from, the same input as the dashboards). `forge env shape <env>` renders them per env (the SQL names the env) and records them with `source: default`.

| id | alert | fires when | table |
|---|---|---|---|
| `error-rate` | `ServiceErrorRateHigh` (critical, 5m) | server errors / requests > 5% over 5m, per service, with at least 20 requests; `user` and `canceled` errors excluded | `otel_metrics_sum` (`rpc.server.calls`, `rpc.server.errors`) |
| `p95-latency` | `ServiceLatencyP95High` (warning, 10m) | p95 of `rpc.server.duration` > 1s over 5m, per service, at least 20 requests | `otel_metrics_histogram` |
| `service-silent` | `ServiceSilent` (warning) | no request for 10m by a service that served requests in at least 18 of the previous 24 hours | `otel_metrics_sum` |
| `frontend-js-errors` | `FrontendJSErrorSpike` (warning, 10m) | JS error spans in the last 10m > 3x the rate of the 60m before, at least 10 | `otel_traces` |

Tune or disable per env:

```python
defaults = {
    "error-rate": forge.AlertDefault {params = {threshold = 0.02}, labels = {severity = "warning"}}
    "service-silent": forge.AlertDefault {enabled = False}
}
```

`params` per default: `error-rate` `threshold`, `window_minutes`, `min_requests`; `p95-latency` `threshold_seconds`, `window_minutes`, `min_requests`; `service-silent` `silent_minutes`, `min_active_hours`; `frontend-js-errors` `spike_factor`, `window_minutes`, `baseline_minutes`, `min_errors`. To write a variant, copy a default's SQL from `forge env shape <env> --json` into your own rule (under another name).

## Sampling: why RED comes from metrics

The hosted gateway tail-samples traces: **every error trace is kept, healthy traces are sampled**, and it writes no sample-rate attribute. So a count of non-error spans reports a fraction of the traffic, and an errors-over-spans ratio over-states the error rate. The defaults therefore read the **rpc server metrics** (`rpc.server.*`, not sampled) for rate, errors and latency. The one span-based default counts only error spans and compares them with themselves, so sampling cancels. In your own rules, count metrics, or error spans; do not divide by a count of healthy spans.

Counters are cumulative, so a rule that sums `Value` over a window is wrong; difference per series (grouped by `ResourceAttributes, Attributes`), treating a drop as a restart. `error-rate`'s SQL is the worked example.

## What is recorded

`forge env shape <env> --json | jq '.shape.alerts, .shape.alert_routes'`:

```json
{"alerts": [{"alert": "ServiceErrorRateHigh", "expr": "WITH series AS (…", "for": "5m", "interval": "1m",
             "labels": {"severity": "critical"}, "annotations": {"summary": "…"},
             "source": "default", "default_id": "error-rate"}],
 "alert_routes": [{"receiver": "reliant-inapp", "continue": true}, {"receiver": "slack"}]}
```

The rule fields are `pkg/alerting`'s `RuleSpec` JSON, so the same document feeds an evaluator directly. Disabled rules and defaults are not recorded. Routes keep their order. Shapes with no alerts encode as before.
