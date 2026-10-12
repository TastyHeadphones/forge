---
name: v0.1.45
description: Move the local observability switch from the bare `_observability = False` toggle to the typed `forge.Observability` declaration on the Bundle. Use when deploy/kcl/<env>/main.k has `_observability` but no `observability =` on its forge.Bundle, or when a workload still sets OTEL_EXPORTER_OTLP_* by hand to reach a collector, or when `forge.OTEL_ENV` points at `otel-collector.observability:4317`.
version: v0.1.45
detection: grep -qs '^_observability = ' deploy/kcl/*/main.k && ! grep -qs '^    observability = ' deploy/kcl/*/main.k
---

# Typed observability destination

Where an environment's telemetry goes is now **declared** on the Bundle as `forge.Observability` (see `observability/destination`). `forge env up` and the cluster render read it; the ad-hoc endpoint injection that used to live in the up layer is gone.

## What changed

- `_observability = True/False` still works as the local switch, but it now feeds a typed declaration instead of being read by `forge env up` directly. A scaffolded `deploy/kcl/dev/main.k` ends its Bundle with `observability = _observability_decl`.
- `forge env up` exports `OTEL_EXPORTER_OTLP_ENDPOINT`/`_PROTOCOL` to host processes **from `output.observability`**, not from the clickstack workload's port. The result is the same pair, now in KCL's render, so `forge env render dev` shows it on each workload.
- `forge.OTEL_ENV`'s endpoint was `http://otel-collector.observability:4317`, which nothing deployed. It now points at the node agent Service (`otel-agent.forge-observability`), and no longer sets a sampler (sampling is `Observability.sampling`).
- New: a destination other than local ClickStack, with a node-agent DaemonSet on cluster runtimes.

## Steps

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

## Verify

`forge env render dev | grep OTEL_EXPORTER_OTLP` shows `http://127.0.0.1:<port>` and `http/protobuf` on every host workload. `forge env up dev` and `forge doctor` still show traces, metrics and logs arriving.
