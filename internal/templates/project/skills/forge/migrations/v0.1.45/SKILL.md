---
name: v0.1.45
description: Upgrade a project to forge v0.1.45. One section per change — (1) the typed `forge.Observability` declaration replaces the bare `_observability` toggle; (2) continuous profiles, on by default beside the local ClickStack (`profiles_port`, the `otel-profiles` collector). Use when deploy/kcl/<env>/main.k has `_observability` but no `observability =` on its forge.Bundle, has no `profiles_port`, or a workload still sets OTEL_EXPORTER_OTLP_* by hand to reach a collector, or `forge.OTEL_ENV` points at `otel-collector.observability:4317`.
version: v0.1.45
detection: (grep -qs '^_observability = ' deploy/kcl/*/main.k && ! grep -qs '^    observability = ' deploy/kcl/*/main.k) || (grep -qs '^_observability = ' deploy/kcl/dev/main.k && ! grep -qs 'profiles_port' deploy/kcl/dev/main.k)
---

# Upgrading to v0.1.45

Each section below is independent: apply the ones whose "Use when" matches.


## 1. Typed observability destination

Use when `deploy/kcl/<env>/main.k` has `_observability` but no `observability =` on its `forge.Bundle`.

Where an environment's telemetry goes is now **declared** on the Bundle as `forge.Observability` (see `observability/destination`). `forge env up` and the cluster render read it; the ad-hoc endpoint injection that used to live in the up layer is gone.

### What changed

- `_observability = True/False` still works as the local switch, but it now feeds a typed declaration instead of being read by `forge env up` directly. A scaffolded `deploy/kcl/dev/main.k` ends its Bundle with `observability = _observability_decl`.
- `forge env up` exports `OTEL_EXPORTER_OTLP_ENDPOINT`/`_PROTOCOL` to host processes **from `output.observability`**, not from the clickstack workload's port. The result is the same pair, now in KCL's render, so `forge env render dev` shows it on each workload.
- `forge.OTEL_ENV`'s endpoint was `http://otel-collector.observability:4317`, which nothing deployed. It now points at the node agent Service (`otel-agent.forge-observability`), and no longer sets a sampler (sampling is `Observability.sampling`).
- New: a destination other than local ClickStack, with a node-agent DaemonSet on cluster runtimes.

### Steps

1. In `deploy/kcl/dev/main.k`, replace the `_observability` block with the one forge scaffolds (copy it from a fresh `forge project new`), or add, below your port declarations:

   ```python
   _observability_decl = forge.Observability {
       local_port = _otlp_http_port
   } if _observability else None
   ```

   and add `observability = _observability_decl` to the `forge.Bundle`.
2. Staging/prod: leave `observability` unset (telemetry off, as before) or declare a destination. A workload that set `OTEL_EXPORTER_OTLP_*` by hand keeps working: an author's value wins.
3. Delete any `env` entry that pointed a workload at `otel-collector.observability:4317`.
4. `forge env render dev` — each host workload now carries the endpoint/protocol pair.

### Verify

`forge env render dev | grep OTEL_EXPORTER_OTLP` shows `http://127.0.0.1:<port>` and `http/protobuf` on every host workload. `forge env up dev` and `forge doctor` still show traces, metrics and logs arriving.

## 2. Continuous profiles beside the local ClickStack

Use when `deploy/kcl/dev/main.k` declares `_observability` but no `profiles_port`.

### What changed

- `pkg/observe` profiles every process that has an OTLP endpoint (10s CPU + heap + goroutine, about once a minute) and pushes OTLP profiles; `OTEL_PROFILES_EXPORTER=none` turns one process off.
- They go to a second collector, `otel-profiles` (contrib 0.162.0), which writes into ClickStack's ClickHouse. `profiles-schema` applies `deploy/observability/profiles-schema.sql` on every up.
- `forge.Observability` gained `profiles_port`, and `signals.profiles` is now unset by default: **on** with the local ClickStack (no destination and a `profiles_port`), off elsewhere, where the render sets `OTEL_PROFILES_EXPORTER=none`. An explicit `True`/`False` wins. The declaration, not `forge env up`, derives `OTEL_EXPORTER_OTLP_PROFILES_ENDPOINT` for host processes.
- Without `profiles_port` nothing breaks: profiles stay off (`OTEL_PROFILES_EXPORTER=none`) until you add it.

### Steps

1. `forge generate` writes `deploy/observability/otel-profiles.yaml` and `deploy/observability/profiles-schema.sql` (both yours).
2. `docker-compose.yml`: copy the `profiles-schema` and `otel-profiles` services from a fresh `forge project new` (they replace the `SEAM: profiles` comment), and add `--feature-gates=+service.profilesSupport` to `otel-forwarder`'s `command` if you have one.
3. `deploy/kcl/dev/main.k`, below `_hyperdx_port`:

   ```python
   _profiles_otlp_port = plugin.resolve_port("<project>-dev-otlp-profiles", 4328) if _observability and not _destination else 0
   ```

   add `profiles_port = _profiles_otlp_port` to `_observability_decl`, and append to the ClickStack branch of `_telemetry_workloads`:

   ```python
   fw.Workload {
       name = "otel-profiles"
       runtime = forge.OnCompose {
           service = "otel-profiles"
           env = {"CLICKSTACK_PROFILES_OTLP_PORT" = str(_profiles_otlp_port)}
       }
   }
   ```

### Verify

`forge env render dev | grep PROFILES` shows `OTEL_EXPORTER_OTLP_PROFILES_ENDPOINT=http://127.0.0.1:<port>/v1development/profiles` on each host workload. After `forge env up dev` and a minute of traffic, `forge observe profile --list` names your services.
