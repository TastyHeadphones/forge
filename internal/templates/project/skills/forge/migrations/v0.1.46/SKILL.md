---
name: v0.1.46
description: Upgrade a project to forge v0.1.46. One section per change — (1) host-wide heavy slots: `forge slot run` / `forge slot status` (pkg/hostslot) queue heavy work machine-wide, `forge generate` and `forge lint` take a slot on their own, and the scaffolded Taskfile runs `test` and `test:integration` under a `SLOT` prefix. Use when Taskfile.yml has a `test` task but no `SLOT:` var, or when several agents or worktrees share one machine and builds and test runs slow each other down.
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
