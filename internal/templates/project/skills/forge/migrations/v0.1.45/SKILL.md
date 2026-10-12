---
name: v0.1.45
description: Upgrade a project to forge v0.1.45. One section per change — (1) the typed `forge.Observability` declaration replaces the bare `_observability` toggle; (2) continuous profiles, on by default beside the local ClickStack (`profiles_port`, the `otel-profiles` collector); (3) `gen/go.mod` requiring the retired `github.com/reliant-labs/forge/pkg` module, which `forge generate` now rewrites (the root `go.mod` is yours to fix); (4) the gitignored `.npmrc` (`legacy-peer-deps`) that stops `npm install` crashing in a `--link-forge` project. Use when deploy/kcl/<env>/main.k has `_observability` but no `observability =` on its forge.Bundle or no `profiles_port`, when `grep forge/pkg gen/go.mod go.mod` matches or `forge env up` reports "go module graph is stale" on every start, or when a bridged frontend's `npm install` dies with "Cannot read properties of null (reading 'edgesOut')".
version: v0.1.45
detection: (grep -qs '^_observability = ' deploy/kcl/*/main.k && ! grep -qs '^    observability = ' deploy/kcl/*/main.k) || (grep -qs '^_observability = ' deploy/kcl/dev/main.k && ! grep -qs 'profiles_port' deploy/kcl/dev/main.k) || grep -qsE '^[[:space:]]*(require[[:space:]]+)?github\.com/reliant-labs/forge/pkg[[:space:]]+v' go.mod gen/go.mod
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

## 3. Retired `forge/pkg` module in `gen/go.mod`

Use when `grep forge/pkg gen/go.mod go.mod` matches, or `forge env up` reports "go module graph is stale" on every start.

### What happened

forge v0.1.44 collapsed `github.com/reliant-labs/forge/pkg` into the one module
`github.com/reliant-labs/forge`. Import paths did not change
(`github.com/reliant-labs/forge/pkg/orm` is still that path); only the module
that provides them did.

A project scaffolded before this release could end up with this in `gen/go.mod`:

```
require github.com/reliant-labs/forge/pkg v0.1.15
```

The scaffold wrote the correct `require github.com/reliant-labs/forge vX.Y.Z`,
but its own `go mod tidy` ran while `gen/` still had no Go files and deleted it
as unused. The first real tidy, after codegen, then found
`forge/pkg/forgepb` imported with no forge requirement, and `go mod tidy`
(which ignores `go.work`) resolved it to the longest module path providing it:
the retired `forge/pkg` module. Symptoms: `forge env up` says the module graph
is stale on every start, and a root `go.mod` that also requires `forge` makes
every `forge/pkg/*` import "ambiguous".

### What forge does now

- `forge project new` no longer tidies a `gen/` that holds no Go files, so the
  template's forge require survives.
- `forge generate` re-asserts `gen/go.mod`'s forge requirement before codegen:
  it drops `github.com/reliant-labs/forge/pkg` and adds
  `github.com/reliant-labs/forge` at the root `go.mod`'s pin. Nothing for you to
  do in `gen/`: run `forge generate`, and it prints
  `gen/go.mod now requires github.com/reliant-labs/forge`.

### Steps

1. Run `forge generate`. `gen/go.mod` is migrated.
2. Check the ROOT `go.mod`. `forge generate` refuses (it will not edit your
   `go.mod`) while it still requires the retired module:

   ```bash
   grep -n 'reliant-labs/forge' go.mod
   # if it lists forge/pkg:
   go mod edit -droprequire=github.com/reliant-labs/forge/pkg
   go get github.com/reliant-labs/forge@$(grep -m1 '^forge_version' forge.yaml | awk '{print $2}')
   go mod tidy
   ```

3. Commit `gen/go.mod` and `gen/go.sum` if your project tracks them.

### Verify

```bash
grep -c 'forge/pkg ' go.mod gen/go.mod      # 0 and 0
(cd gen && GOWORK=off go mod tidy -diff)    # exits 0
```

## 4. `npm install` crash in a `--link-forge` project

Use when a bridged project's frontend `npm install` dies with "Cannot read properties of null (reading 'edgesOut')". Nothing to run: `forge generate` handles it.

A project bridged to a forge checkout (`forge project new --link-forge`, or a
`go.work` that `use`s one) also gets a gitignored npm workspace root that links
`web-runtime` into the install. That brings `web-runtime`'s `vitest ^4.1`
devDependency into the tree, and npm 10.x (what Node 22 ships) and 11.0.0 crash
resolving any tree holding vitest >= 4.1:

```
npm error Cannot read properties of null (reading 'edgesOut')
```

Reproducible without forge: a `package.json` containing only
`"vitest": "^4.1.11"` crashes the same way (npm 11.21.0 installs it).
Unbridged projects are unaffected: the scaffolded frontends pin vitest `^3`.

`forge generate` now writes a gitignored `/.npmrc` containing
`legacy-peer-deps=true` next to the bridge's root `package.json`, before it
installs anything. Nothing to do. If the project already has its own
`.npmrc`, forge leaves it alone and warns: add `legacy-peer-deps=true` to it,
or use a newer npm. Dropping the bridge (delete `.forge-link/` and the root
`package.json`) removes the need.
