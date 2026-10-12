---
name: ci/gate
description: The local CI gate — `task ci` / `task ci:checks` / `forge ci gate`. What each lane runs, the four rules (every failure reported, a skip is not a pass and needs CI_SKIP, pinned tools, the job summary), the check list, adding project checks with ci.gate.extra_checks, and why the scaffolded ci.yml has few jobs. Use when a CI job is red and you want it locally, when `task ci` reports SKIPPED, or before adding a job or check to CI.
---

# The local gate

CI is a thin wrapper around a gate you can run on your own machine. The
workflow prepares an environment (checkout, toolchains, caches) and calls ONE
task per job; the task is the definition of the lane, so the same command gives
the same verdict locally:

| Command | What it runs | CI job that calls it |
|---|---|---|
| `task ci` | `ci:checks`, then `test`, then `test:integration`, in ONE summary table. No change detection: it runs everything | none: this is what you run instead of CI |
| `task ci:checks` | the static and build lane (below) plus your `ci.gate.extra_checks` | `checks` |
| `task test` | unit tests + every frontend's | `test` |
| `task test:integration` | the `integration`-tagged suite | `integration-test` |

`task ci` and `task ci:checks` call `forge ci gate [checks|test|integration|all]`.
The check list and the tool pins live in forge, not in a script copied into the
project, so a fix to the gate reaches every project on the next forge bump and
CI cannot drift from the local verdict. `forge ci gate --list` names every check
and its lane. For a project with services and a frontend the `checks` lane is:
`deps-<fe>`, `verify-generated`, `golangci-lint`, `buf-lint`, `migration-safety`,
`lint-<fe>`, `typecheck-<fe>`, `css-health-<fe>`, `go-build`, `build-<fe>`,
`vuln-go`, `vuln-npm`, `license-check`, `validate-kcl`, `deployability`. Which
rows exist follows the same forge.yaml toggles the workflow renders from
(`ci.lint`, `ci.vuln_scan`, ...), so a toggle means the same thing in both.

Four rules every run obeys:

1. **Every failure is reported.** Each check runs even if an earlier one failed;
   a table follows (check, `PASS`/`FAIL`/`SKIPPED`/`N/A`, duration) and the exit
   status is non-zero if any check failed.
2. **A skip is not a pass.** A check that could not run (a missing tool, a dirty
   tree, no network to install a pinned tool) is `SKIPPED` with the reason, and
   fails the gate unless you name it: `CI_SKIP=vuln-go,license-check task ci`.
   The summary still lists it. A name that matches no check is an error, so a
   typo cannot silently skip nothing. `N/A` (no `db/migrations/`, no integration
   tests) is not a skip and does not fail the gate.
3. **Pinned tools.** golangci-lint, buf, govulncheck and go-licenses run at the
   versions the workflow installs (`templates.GolangciLintVersion`,
   `BufVersion`, `GovulncheckVersion`, `GoLicensesVersion`), built into forge's
   tool cache (`$FORGE_TOOLS_CACHE` or the user cache dir) and put first on that
   command's PATH. A copy on PATH at another version is not used, and neither is
   a golangci-lint built with a Go older than your `go.mod`. Your PATH and GOBIN
   are never touched.
4. **The table reaches the job summary.** When `$GITHUB_STEP_SUMMARY` is set the
   table is appended to it as Markdown.

`verify-generated` needs the COMMITTED tree (it attributes every change to
`forge generate`), so on a dirty tree it is `SKIPPED` rather than a misleading
pass. When it fails it resets the tree it dirtied, so the checks after it judge
the committed code. It runs first for the same reason.

Change detection stays in the workflow: skipping jobs a docs-only change cannot
affect is a cost optimization, and locally you scope by running one lane.

## Why the workflow has few jobs

GitHub bills every job rounded UP to the whole minute, and a runner is a fresh
machine: each job repeats checkout, toolchain setup, cache restore and the forge
install before doing any work. When every check was its own job, the cost of a
PR grew with the NUMBER of checks rather than with what they do: the one project
generated from that layout spent roughly half its Actions minutes on setup, about
$850/month at its peak pace, until its org budget blocked its CI entirely. So the
static and build checks share one job behind one setup. To add a check, add it to
the gate (`ci.gate.extra_checks`, below), not a job. Add a job only for something
that needs its own runner (a service container, a different OS).

Main pushes share one concurrency group that never cancels, so every merge ends
in a verdict; PRs cancel a superseded run per ref.

## Adding your own checks

```yaml
# forge.yaml
ci:
  gate:
    extra_checks:
      - name: gen-llm-gateway          # the row, and what CI_SKIP matches; unique
        command: [go, run, ./cmd/gen-llm-gateway, -check]
        dir: .                          # optional, project-relative
        requires: [go]                  # optional; missing => SKIPPED, not PASS
```

Extras run in the `checks` lane, after forge's own checks, so `task ci:checks`,
`task ci` and the CI `checks` job all run them. A name that reuses one of
forge's is rejected.
