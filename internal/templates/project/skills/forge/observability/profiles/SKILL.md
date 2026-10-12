---
name: observability/profiles
description: Continuous profiles — what pkg/observe captures and pushes over OTLP, when profiling is on (local ClickStack by default, signals.profiles elsewhere), the otel-profiles collector and its ClickHouse tables, and exporting flamegraphs with forge observe profile.
---

# Continuous profiles

Every process with an OTLP endpoint profiles itself, in-process (`pkg/observe`, started by `observe.Setup`, which `serverkit.Run` calls). About once a minute, jittered, it captures a **10s CPU profile plus heap and goroutine snapshots**, converts them to OTLP profiles, and pushes them. The collector never scrapes anything; this is the same push path as traces, so it also works from a locked-down pod with no inbound network.

**On by default locally, governed by `signals.profiles` elsewhere.** The env's `forge.Observability` decides, in KCL, like every other signal:

| Env | `signals.profiles` unset | Rendered for host processes |
|---|---|---|
| local ClickStack (no destination, `profiles_port` set: the dev default) | **on** | `OTEL_EXPORTER_OTLP_PROFILES_ENDPOINT=http://127.0.0.1:<profiles_port>/v1development/profiles` |
| a destination, a cluster, `forge.OnHosted`, or no profiles collector | off | `OTEL_PROFILES_EXPORTER=none` |

An explicit value wins either way: `signals = forge.ObservabilitySignals {profiles = False}` turns local profiling off; `True` with a destination sends profiles to the forwarder or node agent at the base endpoint (both carry the `service.profilesSupport` gate), so the destination must accept OTLP profiles. `True` with the local ClickStack and no `profiles_port` is refused at render: ClickStack's own collector cannot store profiles. An endpoint, protocol or profiles endpoint you set yourself (KCL `env`, or exported in the shell) wins over the derived one, and a base endpoint you chose takes profiles along with it.

- **Cost.** ~90 ms CPU to convert a cycle (about 0.15% of one core averaged), plus the runtime's own 100 Hz sampling during the 10s window; ~8.5 MB/day of ClickHouse per instance (raw tier).
- **Never in the way.** The loop runs on its own goroutine, recovers panics, probes the collector with one TCP dial before each cycle (an unreachable collector costs a dial, not a profile), backs off exponentially to 15 min, and skips the CPU profile if someone else (`/debug/pprof/profile`) is already running one. Mutex and block profiles are off.
- **Off for one process.** `OTEL_PROFILES_EXPORTER=none`, or `observe.Config{ProfilesDisabled: true}`.
- **Trace links.** `TracingInterceptor` (and `observe.ProfileLabelsMiddleware` for plain `net/http` handlers) wrap each request in pprof labels naming its span; the profiler turns them into OTLP links, so the raw rows carry `TraceId`/`SpanId`. The CPU profiler samples at 100 Hz, so a span under ~10 ms usually has no samples: links are for slow spans.

Locally, the `otel-profiles` compose service (stock contrib collector **v0.162.0**, digest-pinned, `service.profilesSupport` gate on) receives them and writes to the ClickHouse inside `clickstack`. ClickStack's own collector is v0.155 and cannot write profiles. The producer's translator and pdata are pinned to the same v0.162.0: **the profiles wire format is pre-release, so bump all three together.** Tables and retention live in `deploy/observability/profiles-schema.sql` (yours; applied idempotently by the `profiles-schema` job on every `forge env up`):

| Table | Holds | TTL |
|---|---|---|
| `default.otel_profiles` | raw: one row per sample, with `TraceId`, `SpanId`, `ProfileId`, `StackHash` | 72h |
| `default.profile_stacks` | distinct stacks per service (`FunctionNames` leaf first) | 30d |
| `default.profile_samples_1m` | per-minute totals per `(service, env, type, stack)` | 30d |

**HyperDX cannot draw flamegraphs**, so export and open them elsewhere:

```bash
forge observe profile --list                                   # which services sent profiles
forge observe profile --service api --since 30m -o api.pb.gz
go tool pprof -http=: api.pb.gz                                # flamegraph, top, peek
forge observe profile --service api --format speedscope -o api.json   # open at speedscope.app
forge observe profile --trace <trace-id>                       # CPU burned inside one trace (raw tier, last 72h)
forge observe profile --service api --type alloc_space --format folded | flamegraph.pl > heap.svg
forge observe profile --service api --sql                      # the query, to run yourself
```

Types: `cpu` (nanoseconds), `alloc_space`/`inuse_space` (bytes), `alloc_objects`/`inuse_objects`, `goroutine`. The same aggregated-flamegraph SQL, to run in HyperDX's SQL console or `clickhouse-client`:

```sql
SELECT arrayStringConcat(arrayReverse(s.FunctionNames), ';') AS stack, sum(a.Value) AS value
FROM default.profile_samples_1m a
INNER JOIN (SELECT * FROM default.profile_stacks FINAL WHERE ServiceName = 'api') s USING (ServiceName, StackHash)
WHERE a.ServiceName = 'api' AND a.SampleType = 'cpu' AND a.Minute >= now() - INTERVAL 1 HOUR
GROUP BY stack ORDER BY value DESC
```

Stacks carry function names only (no line numbers): the dedup key is a hash of function and file. `pkg/profilequery` holds the SQL and the pprof/speedscope/folded writers for reuse by other tools.
