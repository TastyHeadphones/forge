---
name: observability
description: Observability — the local ClickStack (HyperDX) that forge env up runs by default, forge env status runtime checks, querying logs/traces/metrics, dashboards.
---

# Observability

Every Forge project ships a full observability stack for the dev loop: **ClickStack** (ClickHouse, the HyperDX UI and an OTLP collector) as the `clickstack` service in `docker-compose.yml`. Nothing to register, no key to copy. It is **on by default**: `forge env up dev` starts it and lists the **HyperDX UI** under **Compose services**.

It is one container (HyperDX's no-auth *local mode* image), about 0.8 GB resident. Every port is published on `127.0.0.1` only; ClickHouse is not published and runs behind a generated dev-only user, not the passwordless `default`.

## How the data gets there

| Signal | Path |
|---|---|
| Traces, metrics | Host processes started by `forge env up` get `OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:<port>` and `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf` and push OTLP/HTTP to the collector. Compose services use `http://otel-collector:4318`. The app never learns the backend, only the standard variables. |
| Logs | The collector tails `.forge/logs/<env>/<service>.log` — the files `forge env up` already tees every host process into — parsing the runtime's JSON lines. `service.name` is the file name (the workload name); `trace_id` / `span_id` become the log's trace link. This is the **one** local log path (the runtime does not also export logs over OTLP). The pipeline is `deploy/observability/otel-collector.yaml` — yours to edit. |

A value you set yourself (shell, `config.k`, a workload's `env`) wins: `forge env up` fills `OTEL_EXPORTER_OTLP_ENDPOINT` and `_PROTOCOL` as a pair, only when neither is set. Frontend dev servers get the endpoint (for their `/_otel` proxy) and the env name; `frontend_*.log` is not tailed, as the browser SDK ships the console.

**To turn it off**, set `_observability = False` in `deploy/kcl/dev/main.k`: no container starts and no endpoint is exported. Trace context still propagates. Profiles are not collected locally yet.

## forge env status — the runtime checks

Verify the entire observability pipeline is working:

```bash
forge env status dev                    # Table + every runtime check
forge env status dev --signal traces    # Check only traces
forge env status dev --signal metrics   # Check only metrics
forge env status dev --signal logs      # Check only logs
forge env status dev --signal profiles  # Check only pprof
forge env status dev --signal app       # Check only /healthz + /readyz
forge env status dev --json             # Machine-readable output
forge env status dev --verbose          # Show evidence for passing checks
```

Checks: compose infra, app health, pprof (and that the process answering it IS
the app), traces, metrics (gauge, sum and histogram) and logs from the last 15
minutes in ClickHouse, each naming the services that sent them, and Delve.

They read ClickHouse itself (`docker compose exec` into `clickstack`, with its
own credentials), so a pass means rows are in the store, not that a UI answers.
With ClickStack off they SKIP and say so.

**These live on `forge env status`, not `forge doctor`.** They all need an
ADDRESS, and only `forge env status <env>` resolves one — it renders the same
KCL `forge env up` does and overlays the ports the live stack actually bound.
`forge doctor` only answers "is this PROJECT well-formed" and takes no `--env`.

A check that cannot obtain the facts it needs reports **UNDETERMINED** (`?`),
never a pass and never a skip. Three outcomes, not two.

Run it after `forge env up dev` to verify the pipeline before investigating.

**The runtime checks verify the telemetry pipeline, NOT app-flow correctness.** They (like `forge env smoke`) are green when containers, endpoints, and signal ingestion are healthy — they can be green while the actual app flow is broken (e.g. a cross-cluster dial failing). To prove an app-flow invariant holds, use a declarative, exit-coded app-health assertion (model: a project `doctor:<flow>` task) plus a full `task test:e2e`. They tell you observability works; they do not certify the app does.

## Accessing HyperDX

`forge env up dev` and `forge env status dev` list the **HyperDX UI** URL under **Compose services** (its port is declared once in `deploy/kcl/dev/main.k`, `_hyperdx_port`, default 8180). There is no sign-in. Three sources are pre-created — **Logs**, **Traces**, **Metrics** — so search works on first open.

### Dashboards

HyperDX dashboard JSON dropped into `deploy/observability/dashboards/` is loaded automatically, within about 15 seconds and without a restart (HyperDX's file provisioner, `DASHBOARD_PROVISIONER_DIR`). A provisioned dashboard is read-only in the UI and replaced by name on the next load: edit the file, not the browser copy. A dashboard you build by hand in the UI is separate and never overwritten. The directory is yours; forge scaffolds it empty.

## Querying Logs

In HyperDX → Search → Logs, filter on the service (the workload name), severity, or any attribute the log line carried:

```
ServiceName:api SeverityText:error
ServiceName:api LogAttributes.procedure:"/services.users.v1.UsersService/Create"
TraceId:abc123
```

Logs are structured JSON with consistent attribute keys (`procedure`, `request_id`, `trace_id`, `duration_ms`, `user_id`, `status`, `code`, `error_class`). Emit the same attribute keys from your own log sites so dashboards stay queryable. A line that is not JSON (air's rebuild output, a panic) is kept as plain text under the same service.

Or ask ClickHouse directly:

```bash
docker compose exec clickstack sh -c 'clickhouse-client -u "$CLICKHOUSE_USER" --password "$CLICKHOUSE_PASSWORD" \
  -q "SELECT Timestamp, ServiceName, SeverityText, Body FROM default.otel_logs ORDER BY Timestamp DESC LIMIT 20"'
```

## Error levels: who has to act

A log level is a routing decision: ERROR is what alerts match, what a Sentry
forwarder sends, what a post-deploy log gate fails on. So forge sets the level
of every error record from **fault attribution** — `svcerr.Classify(err)` —
not from how the call site spelled its log line:

| `error_class` | what it means | example | level | pages (ERROR → Sentry / alerts) |
|---|---|---|---|---|
| `server` | our system failed, or cannot tell that it did not | Internal, Unknown, DataLoss, Unavailable, any unrecognised error | ERROR | yes |
| `server` | server-side, but "retry", not "page" | DeadlineExceeded, Aborted, Unimplemented | WARN | no |
| `user` | the caller or user must act; the system behaved correctly | InvalidArgument, NotFound, AlreadyExists, PermissionDenied, Unauthenticated, FailedPrecondition (InsufficientBalance, Expired), OutOfRange, ResourceExhausted (PlanLimit), or `svcerr.WithClass(err, svcerr.ClassUser)` | INFO (`observe.WithUserErrorLevel`) | no |
| `canceled` | the caller went away | `context.Canceled`, CodeCanceled | same as `user` | no |

Nothing is hidden: a user error is still written, with `error`,
`error_class=user` and `code`, at a level production keeps — it just stops
claiming to be an incident. Its volume stays countable: the RPC error counter
(and `<pkg>.errors` on the component chain) carries `error_class` and `code`.

The policy holds at all three places a record comes from:

1. `rpc failed` / `stream failed` at the RPC edge (`observe.LoggingInterceptor`);
2. component failures on the in-process chain (`observe.LogMiddleware`);
3. **every hand-written `logger.Error`** — `observe.NewErrorClassHandler`
   wraps the process's slog handler (serverkit's `NewLogger` already does) and
   lowers a record carrying a non-server error to the user-error level, adding
   `error_class`. It only ever lowers; it never raises a WARN to ERROR.

**If you forward logs to Sentry (or count them), wrap
`observe.NewErrorClassHandler` OUTSIDE that handler**, so it sees the
classified level — the forwarder keeps gating on `level >= ERROR` and never
needs to know about svcerr:

```go
handler := sentryHandler(serverkit.NewLogger(cfg).Handler()) // forwards >= ERROR
logger := slog.New(observe.NewErrorClassHandler(handler))
```

Marking: most user errors need no marker — return the right `svcerr` kind
(`svcerr.FailedPrecondition`, `svcerr.PlanLimit`, …). Use
`svcerr.WithClass(err, svcerr.ClassUser)` when the CODE and the FAULT disagree
(`Unavailable` is the honest code for "the user's machine is offline", and a
server fault by kind), or when an error never crosses an RPC boundary (a worker
logging a provider's "credential rejected"). `svcerr.WithClass(err,
svcerr.ClassServer)` is the reverse: a 4xx the client must see that is really
our bug. A library error svcerr cannot see into — a workflow engine's
application error that crossed a serialization boundary — is taught with
`observe.WithErrorClassifier(func(error) svcerr.Class)` (return
`svcerr.ClassNone` for "no opinion").

## Querying Traces

In HyperDX → Search → Traces, filter by:
- Service name (the workload name)
- Trace ID (from log lines — a log with a `TraceId` links to its trace)
- Duration range
- Status code

Trace IDs are injected into every log line written with a span-bearing context, connecting logs to traces.

## The environment contract

Apps never learn the collector's backend. The platform renders the standard OpenTelemetry variables, and `forge/pkg/observe` obeys them:

| Variable | Meaning |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | base URL of the collector (host processes `http://127.0.0.1:4318`) |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `http/protobuf` (default, 4318) or `grpc` (4317) |
| `OTEL_EXPORTER_OTLP_HEADERS` | `k=v,k2=v2`, e.g. `authorization=<key>`; per-signal `OTEL_EXPORTER_OTLP_{TRACES,METRICS,LOGS}_{ENDPOINT,PROTOCOL,HEADERS}` override it |
| `OTEL_SERVICE_NAME` | wins over the compiled-in service name (the caller's default is only a fallback) |
| `OTEL_RESOURCE_ATTRIBUTES` | merged into the resource; carries `deployment.environment.name=<env>` |
| `OTEL_SDK_DISABLED=true` | every export off |
| `OTEL_LOGS_EXPORTER=otlp` | the explicit switch for OTLP logs (off by default) |

`Config.OTLPEndpoint` (the typed `otlp_endpoint` config field) overrides the endpoint variables; protocol and headers always come from the environment. With no endpoint nothing is exported, but `/metrics` still works and the W3C TraceContext+Baggage propagator is **always** installed, so `traceparent` is honoured and forwarded regardless. With export on, Go runtime metrics are recorded too.

### Logs: exactly one shipping path

The server writes JSON lines to stdout; a record logged with a context holding a span (`logger.InfoContext(ctx, ...)`) carries `trace_id` and `span_id`. Locally a collector reads the `.forge/logs/<env>/*.log` files; on a cluster a node agent reads container logs. The OTLP logs exporter (`OTEL_LOGS_EXPORTER=otlp`) does not replace stdout and is **mutually exclusive with file/stdout collection** — turn it on only where no agent collects the same stream, or every line ships twice.

## Resource identity

`service.name`, `service.version` and `service.instance.id` come from `observe.Config`. Everything else on the OpenTelemetry resource comes from the standard `OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES` environment variables, which the SDK merges in (`resource.WithFromEnv`) and which win over the `Config` values. The deployment environment is the **forge env name** (`dev`, `staging`, `prod`): the platform renders it as `OTEL_RESOURCE_ATTRIBUTES=deployment.environment.name=<env>`. It is deliberately not the app's `environment` config field, which is a CORS/HSTS mode switch and reads `production` in both staging and prod.

## Querying Metrics

In HyperDX → Search → Metrics (or a chart tile), pick the metric by name. The RPC edge is measured by the otelconnect server interceptor: `rpc.server.call.duration` (histogram, attributes `rpc.method` — the full `pkg.Service/Method` — and `rpc.response.status_code`, `OK` or the upper-case Connect code such as `NOT_FOUND`). Other series the app exports: `db.sql.*` / `go.sql.*` for the DB pool and queries, and `<pkg>_calls` / `<pkg>_errors` / `<pkg>_duration` for the per-package in-process method metrics (component chain). Every series carries `ServiceName`.

## In-process component observability (the middleware chain)

Forge instruments three boundaries: the RPC edge (the interceptor chain built
by `observe.Chain` in `cmd/<bin>/cmd/serve.go`: one span, one metric sample and
one log record per RPC), every internal component→component call (a
generated `middleware_gen.go` decorator routed through the owned
`observe_chain.go` seam), and the ORM (`pkg/orm` registers the `bunotel` hook,
so each query is a child span). Success-log sampling, opting a component in or
out, and the `enforce-component-observe` lint: `observability/component-chain`.

## Audit log (recipe)

Every RPC already produces a structured audit record: the scaffold wires
`fmw.AuditInterceptor(logger, middleware.ClaimsFromContext)` into the `Audit`
field of `observe.Chain(observe.Deps{…})` in the generated `cmd serve.go`. That
is slog-only — the record goes to your logs (queryable in HyperDX) with
`log_type=audit`, message `audit.event`.

For a **queryable, DB-persisted** audit trail (a compliance record you can page
through, plus an admin `ListAuditEvents` RPC), there is **no pack to install**.
The mechanical write-side is the versioned `forge/pkg/audit` library; the
app-specific read-side (the table + the RPC) is code you own.

1. **Own the `audit_log` table** with the normal entity flow — no bespoke
   migration path:

   ```bash
   # declare `// forge:entity message AuditEvent` in the service proto, then:
   forge scaffold
   ```

   Then edit the birth migration so the table matches what the library
   store reads/writes (see `audit.Entry`): `id`, `timestamp`, `user_id`,
   `email`, `procedure`, `peer_address`, `duration_ms`, `status`, `error_code`,
   `error_message`, `metadata` (JSONB), `created_at`. Index `user_id`,
   `procedure`, and `timestamp` for the query filters.

2. **Wire the DB-backed interceptor** from the library — one line. Construct
   the store and swap the base `Audit` field in `cmd serve.go`'s
   `observe.Deps`:

   ```go
   import "github.com/reliant-labs/forge/pkg/audit"

   store := audit.NewDBAuditStore(db)
   chainDeps := observe.Deps{
       // ...recovery/logging/tracing/metrics/auth/ratelimit as scaffolded...
       Audit: audit.Interceptor(logger, middleware.ClaimsFromContext, store),
   }
   ```

   `audit.Interceptor` logs to slog exactly as the base interceptor does AND
   persists each event to the store off the request path (fire-and-forget, so
   audit writes never add latency). A nil store falls back to slog-only. It is a
   thin convenience over `fmw.AuditInterceptorWithSink(logger, claimsFrom, sink)`
   — reach for that plus `audit.Sink(store, logger)` if you already hold a
   custom sink.

3. **Own the read-side `ListAuditEvents` RPC.** Add a proto service + handler
   the normal way (`proto/audit/v1/…` + a handler package), and back the
   handler with the SAME store so reads see the writes:

   ```go
   entries, err := store.Query(ctx, audit.Filter{
       UserID: req.Msg.GetUserId(),  // filters are AND-combined
       Since:  req.Msg.GetSince().AsTime(),
       Limit:  int(req.Msg.GetLimit()), // <= 0 ⇒ default 100
   })
   ```

   Audit logs enumerate who did what, so scope the read handler (restrict a
   non-admin caller to their own `user_id` via `middleware.ClaimsFromContext`)
   — an unscoped handler is a cross-user enumeration hole.

## Outbound instrumentation

- **Plain HTTP downstreams**: pass `infra.DefaultClient()` (providers.go) as
  the adapter's `Deps.HTTPClient` — a fresh client over the shared
  otelhttp-instrumented transport (client spans + W3C propagation + 30s
  timeout). The compose site wires any `HTTPClient *http.Client` Deps field
  to it automatically. If your providers.go predates `DefaultClient`, add the
  method + the `httpBase = otelhttp.NewTransport(http.DefaultTransport)`
  line from a fresh scaffold.
- **Connect clients** (a service split out of the binary): build the stack
  with `observe.NewClientStack` (forge/pkg/observe) — otelconnect client
  interceptor, request-ID forwarding, timeout — then
  `genconnect.NewXClient(stack.HTTPClient, baseURL, stack.ClientOptions...)`.

## Rules

- Run `forge env status dev` after `forge env up dev` to verify observability before investigating issues.
- The HyperDX port is declared in `deploy/kcl/dev/main.k` and listed by `forge env up dev` / `forge env status dev`.
- `deploy/observability/otel-collector.yaml` and `deploy/observability/dashboards/` are yours: forge scaffolds them once and never rewrites them.
- Use the `logevents.go` helpers for structured log events — do not create ad-hoc attribute keys.
- Trace IDs propagate automatically via OpenTelemetry context — no manual instrumentation needed.
