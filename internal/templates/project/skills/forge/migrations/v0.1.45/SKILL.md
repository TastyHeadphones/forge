---
name: v0.1.45
description: Upgrade a project to forge v0.1.45. One section per change — (1) the typed `forge.Observability` declaration replaces the bare `_observability` toggle; (2) continuous profiles, on by default beside the local ClickStack (`profiles_port`, the `otel-profiles` collector); (3) `gen/go.mod` requiring the retired `github.com/reliant-labs/forge/pkg` module, which `forge generate` now rewrites (the root `go.mod` is yours to fix); (4) the gitignored `.npmrc` (`legacy-peer-deps`) that stops `npm install` crashing in a `--link-forge` project; (5) `serve.go` opens the otelconnect span OUTSIDE auth (`chainDeps.Trace`), so rejected calls have spans and request logs carry `trace_id`; (6) lean CI over a local gate — `task ci` / `task ci:checks`, a consolidated `.github/workflows/ci.yml`; (7) `forge.ControlPlaneSecret`, a Secret the control plane delivers and renews, replacing a `secret_optional` env var or a hand-minted Secret for a control-plane OTLP token. Use when deploy/kcl/<env>/main.k has `_observability` but no `observability =` on its forge.Bundle or no `profiles_port`, when `grep forge/pkg gen/go.mod go.mod` matches or `forge env up` reports "go module graph is stale" on every start, when a bridged frontend's `npm install` dies with "Cannot read properties of null (reading 'edgesOut')", or when `grep -n 'Extras = append(\[\]connect.Interceptor{otelInt}' cmd/*/cmd/serve.go` matches, or when Taskfile.yml has no `ci:checks` task or `.github/workflows/ci.yml` runs one job per check instead of calling `task ci:checks`, or when `deploy/kcl/<env>/main.k` mounts an OTLP token Secret a control plane writes after the deploy (`secret_optional = True` on a `SecretRef`, or a `required_secrets` entry for a Secret the control plane mints).
version: v0.1.45
detection: grep -qs 'Extras = append(\[\]connect.Interceptor{otelInt}' cmd/*/cmd/serve.go || (grep -qs '^_observability = ' deploy/kcl/*/main.k && ! grep -qs '^    observability = ' deploy/kcl/*/main.k) || (grep -qs '^_observability = ' deploy/kcl/dev/main.k && ! grep -qs 'profiles_port' deploy/kcl/dev/main.k) || grep -qsE '^[[:space:]]*(require[[:space:]]+)?github\.com/reliant-labs/forge/pkg[[:space:]]+v' go.mod gen/go.mod || (test -f Taskfile.yml && ! grep -qs '^  ci:checks:' Taskfile.yml) || (test -f .github/workflows/ci.yml && ! grep -qs 'task ci:checks' .github/workflows/ci.yml)
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

## 5. Tracing outside auth: `serve.go` sets `chainDeps.Trace`

Use when `grep -n 'Extras = append(\[\]connect.Interceptor{otelInt}' cmd/*/cmd/serve.go` matches. `serve.go` is scaffold-once, so this edit is yours; forge will not make it.

### What was wrong

The scaffolded `serve.go` PREPENDED the `otelconnect` interceptor to `chainDeps.Extras`, and its comment said that made the span "cover every interceptor after it, including auth rejections". It did not: `observe.Chain` appends `Extras` **after** auth, so the span sat inside auth.

- A call auth rejected (a 401: missing or bad token) produced **no span**.
- The `rpc failed` line and the audit event for it were written outside any span, so they carried **no `trace_id`**, and the failure could not be found from a trace.
- Profile samples taken during the request had no span to link to.

### What changed

`observe.Deps` has a new `Trace connect.Interceptor` field. `observe.Chain` puts it OUTERMOST (trace, then pprof labels naming the span, then recovery, request-id, logging, tracing, metrics, auth, audit, rate-limit, extras). Inside it, every log line written for the request has the span in its context.

### Steps

In `cmd/<bin>/cmd/serve.go`, find:

```go
if otelInt, oerr := otelconnect.NewInterceptor(otelconnect.WithTrustRemote()); oerr == nil {
    chainDeps.Extras = append([]connect.Interceptor{otelInt}, chainDeps.Extras...)
} else {
```

and replace the one assignment:

```go
    chainDeps.Trace = otelInt
```

Keep `otelconnect.WithTrustRemote()`: it makes the browser's `traceparent` the PARENT of the server span (without it the two are linked, not joined). Update the comment above the `observe.Deps` literal if you copied the old "recovery, request-id, logging, tracing, metrics, then application interceptors" text. If you also build your own chain with `connect.WithInterceptors(...)` instead of `observe.Chain`, put the otelconnect interceptor FIRST.

If you set `Deps.Tracer` (forge's own `TracingInterceptor`) as well as `Deps.Trace`, you get two spans per RPC; use one.

### Frontends

Nothing to edit. `@reliantlabs/forge-web-runtime` (next release) makes the first RPCs a page makes wait, up to 2 seconds, for the HyperDX SDK to finish starting, so their `traceparent` names a span the browser really exports (before, a call made in that window carried a synthesized id that no browser span matched, so the API span's parent was missing). Bump the dependency when it is published.

### Verify

```bash
curl -si -X POST localhost:<port>/<pkg>.<Service>/<Method> -H 'content-type: application/json' -d '{}' \
  -H 'traceparent: 00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01' | head -1   # 401
```

Then, in ClickHouse (`forge observe` or the HyperDX search): a span for that call with `TraceId = 0af7651916cd43dd8448eb211c80319c`, status error or unset, and an `otel_logs` row `rpc failed` with the same `TraceId`.

Also covers `forge env up`:

- A scaffold with no commits yet now tags images `uncommitted` instead of failing with "git tag resolution: exit status 128".
- With `air` missing, `forge env up` installs forge's pinned air (v1.67.4) into its tool cache, or runs the API under `go-run` with a one-line notice if it cannot. Nothing to change.

## 6. Lean CI over a local gate

Use when `Taskfile.yml` has no `ci:checks` task, or `.github/workflows/ci.yml` has one job per check (`lint`, `build`, `verify-generated`, `vuln-scan`, `deployability`, `license-check`) instead of one `checks` job that runs `task ci:checks`.

### What changed

`ci.yml` is scaffold-once (yours), so `forge generate` never rewrites it and your project keeps the old nine-job shape until you adopt the new one. Why it is worth adopting: GitHub bills each job rounded up to the whole minute, and every job repeats checkout, toolchain setup, cache restore and the forge install before doing any work. The one project generated from the old layout spent roughly half its Actions minutes on setup, and about $850/month at its peak pace.

- **A local gate: `forge ci gate`**, behind two new Taskfile tasks. `task ci` runs everything CI runs (`ci:checks`, then `test`, then `test:integration`) in one summary table; `task ci:checks` is the static and build lane. Run `task ci` before you push and you have the verdict CI would give.
- **The workflow became a thin wrapper.** Jobs are `changes` (skips the heavy jobs for a docs-only change), `checks` (one setup, then `task ci:checks`), `test` (`task test`), `integration-test` (`task test:integration`, PRs only, only when the repo has `//go:build integration` files), plus `docker-build` and `e2e` where you already had them. `lint`, `build`, `verify-generated`, `deployability`, `vuln-scan` and `license-check` are no longer jobs: they are rows in the gate. Every job has `timeout-minutes`.
- **Gate semantics.** Every check runs even if an earlier one failed, then one table; non-zero exit if any failed. A check that could not run (missing tool, dirty tree) is `SKIPPED`, which is not a pass and fails the gate unless you name it: `CI_SKIP=vuln-go task ci`. Under Actions the table is appended to the job summary.
- **Tools run at the versions CI pins** (golangci-lint, buf, govulncheck, go-licenses), built into forge's tool cache on first use. A copy on PATH at another version, or a golangci-lint built with a Go older than your `go.mod`, is not used.
- **Your own checks** go under `ci.gate.extra_checks` in forge.yaml (name, command, optional `dir` and `requires`) instead of a new job or a script copied into the repo. They run in `checks`, so `task ci` runs them too.

### Steps

1. **Taskfile.yml.** `forge generate` does not touch it, so add the two tasks (copy them from a fresh `forge project new`, or paste these):

   ```yaml
   ci:
     desc: "The whole PR gate locally (what CI runs): ci:checks + test + test:integration, one summary"
     cmds:
       - forge ci gate all
   ci:checks:
     desc: "Static + build lane of the CI gate (what the CI `checks` job runs)"
     cmds:
       - forge ci gate checks
   ```

   `test` and `test:integration` already exist; if `test:integration` does not, add it (`go test -race -tags=integration -count=1 -timeout=10m ./...`).

2. **ci.yml.** Move your project's own jobs and steps aside, then adopt forge's current file: delete `.github/workflows/ci.yml` and run `forge project rescaffold .github/workflows/ci.yml` (or `forge project upgrade --check .github/workflows/ci.yml` to read the diff, then `--force` to adopt it). Re-apply what was yours:

   - a step you added to `lint` / `build` / `vuln-scan` becomes an `extra_checks` entry (step 4) or, if it needs the runner's own environment, a step in `checks` before `task ci:checks`;
   - a job with its own runner requirement (a service container, another OS) stays a separate job: copy it back;
   - a repo whose code `go:embed`s a Markdown file narrows the docs-only filter in `changes`.

3. **Branch protection.** The required checks were the old job names. Require `Checks`, `Test` and (if you have an integration lane) `Integration Tests` instead; `changes` and the skipped jobs report as satisfied.

4. **Project checks.** For each thing the old workflow did that forge's gate does not:

   ```yaml
   ci:
     gate:
       extra_checks:
         - name: gen-llm-gateway
           command: [go, run, ./cmd/gen-llm-gateway, -check]
           requires: [go]
   ```

   Names must be unique and may not reuse one of forge's (`forge ci gate --list`).

5. **Run it once.** `task ci` on a committed tree. `verify-generated` needs the committed tree (it attributes every change to `forge generate`), so a dirty tree reports it `SKIPPED` rather than a misleading pass.

### Verify

`forge ci gate --list` names every check and its lane. `task ci:checks` prints a summary table whose last line is `PASS` or `FAIL`. `.github/workflows/ci.yml` has jobs `changes`, `checks`, `test`, `integration-test` and each one has `timeout-minutes`; each calls exactly one `task`. `actionlint .github/workflows/ci.yml` is clean.

## 7. Secrets the control plane delivers: `forge.ControlPlaneSecret`

Use when an env declares a control plane (`control_plane = forge.ControlPlane {}`) that mints and renews a Secret your workloads read, and that Secret is modelled today as an env var with `secret_optional = True` (to dodge the first-deploy block), as a `required_secrets` `ExternalSecret` you then provision by hand, or as a Secret someone mints on a calendar.

### What changed

- New primitive `forge.ControlPlaneSecret` on `Bundle.control_plane_secrets`. It declares a Secret whose value the control plane supplies and renews. It is **recorded in the env shape** (`shape.control_plane_secrets`: name, namespace, cluster, connected cluster, kind, keys; never a value), **never blocks a first deploy**, and is **refused without a connected-cluster binding** (and without `control_plane`). There is no k3d exemption: delivery is the control plane dialing the cluster.
- A cluster workload in the same cluster and namespace as a `telemetry_token` gets, automatically, an **optional** Secret volume `forge-otlp-auth` at `/var/run/secrets/forge/otlp` and `OTEL_EXPORTER_OTLP_HEADERS_FILE=/var/run/secrets/forge/otlp/headers`. `pkg/observe` re-reads that file, so a renewal needs **no restart**.
- `forge.ObservabilityDestination` gained `token_from_control_plane = True`: for a destination that is the control plane's OTLP ingest, forge declares the node agent's `ControlPlaneSecret` (`forge-otlp-token` in `agent.namespace`, key `token`) on every cluster that runs the agent. Exclusive with `headers_secret` / `token_secret`.
- The Secret volume source gained `optional` (`v1alpha1.SecretVolumeSource.Optional`, `tiers.SecretVolumeSource.optional`), which an author can also set on a volume of their own.
- `output.control_plane_secrets[]` is a new key in the render contract (an empty list when none). Every `render_contract` golden gained `"control_plane_secrets": []`.

### Steps

1. Delete the workaround. Remove `secret_optional = True` from the `SecretRef` that read the delivered token, and remove the `required_secrets` `ExternalSecret` for it if there is one. Declaring the same Secret as both is refused ("one Secret has one provider").
2. Declare the Secret, on a cluster that names its connected cluster:

   ```python
   control_plane_secrets = [
       forge.ControlPlaneSecret {name = "reliant-otel-auth", cluster = _prod}
   ]
   ```

   `_prod` needs `connected_cluster = "<name>"` (or `connected_clusters = {"<context>": "<name>"}` on the Bundle); register the cluster once with `forge cluster connect <name> --context <ctx> --env <env>`.
3. Drop the env var that carried the headers (`OTEL_EXPORTER_OTLP_HEADERS` from a `SecretRef`). forge wires the file mount for you; a workload that still sets `OTEL_EXPORTER_OTLP_HEADERS` or `OTEL_EXPORTER_OTLP_HEADERS_FILE` itself is left alone, and gets no volume.
4. For a node agent that exports to the control plane's ingest, replace `headers_secret` / `token_secret` plus the `required_secrets` entry with `token_from_control_plane = True` on the destination.
5. `forge env render <env>`, then `forge env deploy <env> --dry-run`: the preflight no longer asks for the Secret, and the pod spec gains the `forge-otlp-auth` volume (one rolling restart on the deploy that adds it).

### Verify

`forge env shape <env> --json | jq .shape.control_plane_secrets` lists the Secret with its `connected_cluster`. `forge env render <env> | grep -A6 forge-otlp-auth` shows `optional: true`. A first `forge env deploy` against a cluster where the Secret does not exist yet passes the preflight.
