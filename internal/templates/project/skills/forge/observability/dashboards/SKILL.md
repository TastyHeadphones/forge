---
name: observability/dashboards
description: The dashboards forge generates from your services (overview, per-service RED per rpc, frontend), the HyperDX files and the renderer-neutral spec they come from, how to override one, why alerts are not provisioned, and how a deliverer binds source names.
---

# Observability: generated dashboards

`forge generate` (step "observability dashboards") derives dashboards from the
project's own metadata: the proto services and rpcs, the frontends and whether a
database exists. It runs for service projects with observability on and at least
one service.

| File                                                          | What                                                                                                                                                                                             |
| ------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `deploy/observability/dashboards/<project>-overview_gen.json` | requests, server errors, p95/p99, request rate and error rate per service, RED table, top errors, DB latency, recent error logs                                                                  |
| `.../<project>-service-<svc>_gen.json`                        | per service: a requests / error-rate / latency row for EVERY rpc, a RED-by-rpc table, DB calls, Go runtime (memory, GC goal, goroutines, allocation), error logs and logs scoped to that service |
| `.../<project>-frontend_gen.json`                             | with a web frontend: JS errors, sessions, page views, failed API calls, LCP/INP/CLS p75, page-load percentiles, browser API latency and calls by URL                                             |
| `.../saved-searches_gen.ndjson`                               | one error-log search per service                                                                                                                                                                 |
| `deploy/observability/dashboard-spec_gen.json`                | the renderer-neutral model all of the above come from                                                                                                                                            |

Every dashboard has `service.name` and `deployment.environment.name` filters.

## What the panels match

Panels are keyed on what the runtime really emits, not on a convention:

- RPC edge: the otelconnect server interceptor opens a **server** span named
  `<proto package>.<Service>/<Method>` (no leading slash), e.g.
  `orders.v1.OrdersService/GetOrder`. Status is `Error` only for server faults
  (Internal, Unknown, Unavailable, DeadlineExceeded, Unimplemented); a client
  error such as NotFound is not an error span, so the error rate is a **server**
  error rate.
- Log lines carry `procedure` WITH a leading slash, which is how a service's
  logs are scoped (`LogAttributes['procedure']`).
- DB: the ORM's bun hook opens client spans with `db.system` and `db.operation`.
- Runtime: `go.memory.used`, `go.memory.gc.goal`, `go.memory.allocated`,
  `go.goroutine.count`. These are per PROCESS, and one process serves every
  service, so on a service dashboard narrow with the `service.name` filter.
- Browser: spans from the HyperDX SDK carry the `rum.sessionId` resource
  attribute; `documentLoad`, `webvitals` and fetch/XHR spans (`component`).

## Overriding

Never edit a `*_gen.json`: forge rewrites it and refuses a hand-edit as drift.
Copy `x_gen.json` to `x.json`, give it a new `name`, edit it. While `x.json`
exists forge stops writing (and removes a pristine) `x_gen.json`. Any other
`*.json` in the directory is yours and is never touched.

## Dashboards in the release

`forge env build <env> --release <v>` puts the dashboards into the release bundle, so the Reliant console shows the set that belongs to the release that is running (and a rollback shows the older one):

| In the bundle                                      | What                                                                                                                        |
| -------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------- |
| `observability/dashboards/<project>-<id>_gen.json` | each dashboard of `dashboard-spec_gen.json`, plus `<project>-custom-metrics_gen.json` drawn from the build's metrics schema |
| `observability/dashboards/<name>.json`             | a dashboard you own                                                                                                         |
| `observability/metrics-schema.json`                | the schema, `forge.metrics/v1`                                                                                              |

They ride in the manifests layer, outside every `manifests/<cluster>/` tree, so nothing is applied from them.

A file you own is one dashboard object in the spec vocabulary (an `id`, a `title`, `filters`, `annotations` and a `panels` array; the objects of `dashboard-spec_gen.json`'s `dashboards`). Name it like a generated dashboard to replace that one: `shop-overview.json` replaces `shop-overview_gen.json`. A file with `tiles` is HyperDX's format; it is for the local stack, ships nowhere and replaces nothing. Ad-hoc console edits are saved as code by writing the dashboard JSON here and committing it.

Every file is validated at the cut, and `forge observe dashboards check` runs the same checks and lists what would ship. Errors read `deploy/observability/dashboards/x.json:12:7: panels[2].kind: "cube" is not one of: number, line, table, search`.

`annotations: [{"source": "releases"}]` on a dashboard asks the console to draw a marker for every promotion on its time-series panels. Generated dashboards declare it.

The _Workloads_ dashboard (CPU/memory against requests and limits, restarts and exit reason, image versions, Kubernetes events) and the _Releases_ dashboard (request, error and latency by `service.version`) are generated for every project. They read node-derived and release data that a local `forge env up` does not have, so they ship in the spec and the bundle and are not written to `deploy/observability/dashboards/`.

## Alerts

Dashboards embed no alerts (HyperDX 2.40 validates an alert declared in a provisioned file and never evaluates it, hyperdx PR #3233). Alerting is declared in KCL instead: `forge.AlertRule`, with generated defaults (telemetry, customer-metadata and platform) in `alert-defaults_gen.json`. Depth: `observability/alerts`.

## The seam: spec, then renderer

`internal/obsdash` builds a `Spec` (dashboards > panels > series with a signal,
an aggregate and predicates on OpenTelemetry field names; no ClickHouse or
HyperDX names) and renders it. `RenderHyperDX` is one renderer; the Reliant
console draws the same Spec with uPlot. A hosted deliverer pushes the rendered
JSON through the HyperDX API.

Tiles name their source (`Logs`, `Traces`, `Metrics`). The file provisioner
stores that verbatim but the UI resolves sources by random per-database id, so
the delivery step swaps names for ids: locally the compose entrypoint does it
(and upserts the saved searches); through the API, use `GET /api/v2/sources`.
