---
name: v0.1.46
description: Upgrade a project to forge v0.1.46. One section per change — (1) host-wide heavy slots: `forge slot run` / `forge slot status` (pkg/hostslot) queue heavy work machine-wide, `forge generate` and `forge lint` take a slot on their own, and the scaffolded Taskfile runs `test` and `test:integration` under a `SLOT` prefix; (2) `forge ci gate` runs checks concurrently (after / mutates_tree / heavy), caches PASS verdicts by tree hash, and unit-tests only the affected packages on a branch — with a wider ci.gate.extra_checks schema (lane, group, after, heavy, mutates_tree, cache, probe, skip_exit_code, env, replaces) for moving a hand-written gate script onto it. Use when Taskfile.yml has a `test` task but no `SLOT:` var, or when several agents or worktrees share one machine and builds and test runs slow each other down.
version: v0.1.46
detection: test -f Taskfile.yml && grep -qs '^  test:' Taskfile.yml && ! grep -qs 'forge slot run' Taskfile.yml
---

# Upgrading to v0.1.46

Each section below is independent: apply the ones whose "Use when" matches.

## 1. Host-wide heavy slots

Use when `Taskfile.yml` has a `test` task but no `SLOT:` var.

### What changed

On a shared machine the cost of a heavy job is set by how many others run
beside it. Measured on a 16 vCPU / 32 GiB workspace VM: `go build ./...` took
36–50 s beside 0–3 other heavy jobs and 101–155 s beside 8 or more, and a full
local gate went from ~40 min to ~2.3 h. The loss was memory-cgroup reclaim, so
past a handful of concurrent heavy jobs every one slows down. forge now queues
them:

- **`pkg/hostslot`**, a host-wide counting semaphore. A slot is an OS file lock
  on one of N files under the user cache dir; a crashed holder releases it.
  N defaults to min(CPUs / 4, memory GiB / 8), read from the cgroup v2 limit
  when there is one, at least 1. `FORGE_HEAVY_SLOTS=<n>` overrides it,
  `FORGE_HEAVY_SLOTS=off` switches it off, `FORGE_SLOTS_DIR` moves the files.
- **`forge slot run --class heavy -- <cmd>`** runs a command holding a slot,
  printing who holds the slots while it waits. **`forge slot status`** lists
  the count, why, the holders and the queue.
- **`forge generate` and `forge lint` take a slot themselves.** A `⏳ forge:
  waiting for a heavy slot` line is a queue, not a hang.
- **Re-entrant.** Anything already inside a slot (a gate check, a child of
  `forge slot run`) runs at once: the slot travels to children in
  `FORGE_SLOT_HELD`, and an ancestor holding one counts too.

### Steps

1. **Taskfile.yml** is yours, so `forge generate` does not touch it. Add the
   `SLOT` var beside `GOTESTRACE` (or copy it from a fresh `forge project new`):

   ```yaml
   vars:
     SLOT:
       sh: forge slot status --class heavy >/dev/null 2>&1 && echo 'forge slot run --class heavy --' || true
   ```

   and prefix the `go test` lines of `test` and `test:integration`, quoting the
   line because it now starts with `{{`:

   ```yaml
   - '{{.SLOT}} go test {{.GOTESTRACE}} -count=1 -timeout 120s {{if .CLI_ARGS}}{{.CLI_ARGS}}{{else}}./...{{end}}'
   ```

   In `test:frontend`, run each frontend's suite as
   `(cd "$dir" && {{.SLOT}} npm test -- --passWithNoTests)`.

   Keep `--class heavy` in the `SLOT` value: a forge older than v0.1.46 rejects
   the flag loudly, where a bare `forge slot run -- ...` would print its help
   and exit 0 without running the tests. The probe leaves `SLOT` empty when
   forge is absent or too old, so the tasks still run.

2. **Do not** put `ci` / `ci:checks` under `SLOT`: the gate takes slots itself,
   one per heavy check, and one slot around the whole run would let every
   check inside it skip the queue.

3. **Size it** where the machine is shared by more (or fewer) heavy jobs than
   its CPUs and memory suggest: set `FORGE_HEAVY_SLOTS` in the environment the
   agents and terminals inherit. A CI runner that runs one job per machine can
   leave the default (no other job contends) or set `off`.

### Verify

`forge slot status` prints the count and its source (e.g.
`heavy slots: 4 (min(16 CPUs / 4, 32 GiB / 8))`). `task test` while
`forge slot status` runs in another terminal lists `go test ...` as a holder.
`task --list` still works on a machine without forge.

## 2. A concurrent, cached, affected-aware `forge ci gate`

Use when a project runs `task ci` / `forge ci gate`, or keeps a gate script of
its own (`scripts/ci-gate.sh`) that `forge ci gate` could now replace.

### What changed

Nothing to migrate for the gate to work: the change is in forge. What a project
sees:

- **Checks run concurrently.** `verify-generated` (it rewrites the tree) runs
  alone before the checks that read the tree; frontend rows wait for
  `deps-<fe>`; heavy checks hold host slots (section 1); the rest run under
  `--jobs`. The table keeps plan order and adds the wall time and a `head:` line.
- **Per-check logs** in `tmp/ci/<lane>/<check>.log` (`.forge/ci/<lane>/` when
  git does not ignore `tmp/`; `CI_LOG_DIR` moves them). The console shows start
  and finish lines and a failure's tail instead of every check's full output.
- **Verdict cache.** On a committed tree a check that already passed on that
  tree reads `PASS (cached <sha>)`, in any worktree of the repository.
  `CI_NO_CACHE=1` runs everything; a dirty tree turns it off.
- **Affected unit tests.** On a branch, `unit-tests` runs `task test -- <pkgs>`
  for the packages the change reaches; anything but Go source changed, the
  default branch, or `CI_FULL=1` runs the whole suite. The line before the
  table says which.
- **`CI_ONLY`** (checks or groups, plus their `after` dependencies) and
  **`CI_REPORT`** (write the Markdown table to a file).
- `ci.gate` gained `env`, `path` and `affected_ignore`; each extra check gained
  `lane`, `group`, `after`, `heavy`, `mutates_tree`, `cache`, `probe`,
  `skip_exit_code`, `env` and `replaces` (`ci/gate` has the schema).

### Steps

1. **`.gitignore`**: keep `tmp/` ignored (the scaffold does), so the logs never
   dirty the tree the gate judges.
2. **`task test`** must accept Go package patterns after `--` (the scaffolded
   task does: `{{if .CLI_ARGS}}{{.CLI_ARGS}}{{else}}./...{{end}}`). A task that
   ignores them still passes, but runs the whole suite every time.
3. **Mark your extras.** For each `ci.gate.extra_checks` entry: `heavy: true`
   for a whole-module build, test run or render; `mutates_tree: true` if it
   rewrites tracked files (a formatter, pre-commit); `cache: false` if it reads
   the network or a sibling checkout; `after:` if it needs another check's
   output; `group:` if CI should select it with `CI_ONLY`.
4. **Retiring a gate script.** Each step becomes an extra: put the step's
   body in a script (`scripts/ci-step.sh <step>` is a fine shape), have it exit
   77 with a one-line reason when it cannot run (`skip_exit_code: 77`), move
   `PATH` / `GOPRIVATE` exports to `ci.gate.path` / `ci.gate.env`, a step that
   overrides one of forge's to `replaces: <the forge-owned check name>`, and image or cluster
   lanes to `lane: docker` / `lane: k3d`. Then point `task ci` and
   `task ci:checks` at `forge ci gate all` / `forge ci gate checks`.
5. **`ci.gate.affected_ignore`**: list files no Go test reads (`*.md`,
   `docs/**`) so a docs edit does not force the whole suite.

### Verify

`forge ci gate --list` shows each check's lane, group and scheduling. Run
`task ci:checks` twice on a committed tree: the second run's rows read
`PASS (cached <sha>)` (except `vuln-*` and `cache: false` extras). On a branch
that changes one package, `forge ci gate test` prints `unit tests: affected
mode — N of M test packages …`.
