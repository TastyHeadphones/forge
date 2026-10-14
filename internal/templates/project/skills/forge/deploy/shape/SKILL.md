---
name: deploy/shape
description: What an environment DECLARES, as a value — `forge env shape`, the projection it prints, why it never carries a secret value, and the declaration `forge env build` / `forge env deploy` record on the control plane so the console works with no daemon online.
---

# The declared shape

`forge env render <env>` prints the objects an environment renders — megabytes
of YAML on a real environment. `forge env shape <env>` prints what they ARE.

```
forge env shape prod            # the human summary
forge env shape prod --json     # {project, env, kind, shape, provenance}
```

The projection is small on purpose: it is meant to be stored per environment,
read per view, and compared as a set, so "what changed" is answerable without
re-rendering anything.

| Field | What it holds |
|---|---|
| `kind` | `persistent` \| `self_managed` \| `local` — DERIVED from what the env binds, never selected (see below) |
| `workloads` | every workload of every runtime, with its cluster and the release artifact its image comes from |
| `secrets` | each declared secret by NAME, its provider, and the workloads that read it |
| `domains` | every hostname the env binds — a Gateway's host, its listeners' overrides, each route's override |
| `clusters` | every kubectl context the env deploys to |
| `objects` | one entry per rendered object per cluster it lands on: identity, `hash`, `config_hash`, images, and whether it holds data or an external address |
| `delivery_policy` | what the env's releases must prove before they ship (signing, trusted builder, vulnerability gate, approvals, ...), every control with its mode. Absent means every control at its default, so an env that declares nothing records nothing: `deploy/delivery-policy` |

## The two hashes answer different questions

`hash` is the object after secret redaction: what a drift check compares the
live object against. `config_hash` is the same object with every release-bound
image digest normalized to its artifact key — so two shapes with equal
`config_hash`es differ, at most, in which release they pin. That makes
"promote a new release" and "the KCL moved" one comparison instead of a
re-render and a YAML diff.

## It never carries a secret value

Declared secrets appear as names and providers. A rendered `kind: Secret` has
every `data` / `stringData` value replaced by the hash of that value BEFORE
the object is hashed, so no part of the shape — and no hash input — is ever a
copy of the secret. The hash still moves when a value moves, which keeps "a
secret changed" visible without recording what it changed to.

This matters because a shape is stored indefinitely and readable by everyone
in the org: a value in one is a permanent leak, not something a redeploy
fixes.

## Read-only, and checked rather than promised

No cluster is contacted, no image is built, nothing is pushed. forge cannot
promise the render is side-effect-free — KCL evaluates `file.write` during
evaluation, and forge has no hook to suppress a project's own writes — so it
scans the project before and after, and a render that wrote anything FAILS,
naming the paths. A declaration derived from an impure render is one nobody
can reproduce, which is the whole value of recording it.

Helm charts are deliberately not templated: a declaration must be derivable
with no network and no `helm` on PATH, and a chart's objects belong to the
platform dependency rather than to this project's declaration. Use
`forge env render` for those.

## The kind is a predicate, not a setting

| What the env binds | Kind | Who applies it | Its secrets |
|---|---|---|---|
| no `control_plane` | `local` | forge (file ledger) | provider-defined |
| something `OnHosted` | `persistent` | the platform's converger | write-only |
| nothing hosted, something `OnCluster` | `self_managed` | forge | write-only |
| every workload `OnHost` / `OnCompose` | `local` | `forge env up` | readable |

The `self_managed` / `local` split is what keeps "runs on my laptop" and
"runs on my own cluster" apart. A cluster env classified local would have its
production secrets readable back through the local-secret pull.

The kind is IMMUTABLE once recorded: an env whose kind changed is a different
env, and an ensure that disagrees with the stored row is refused rather than
silently changing it.

## `lifecycle` is DECLARED, and it is a different question

`kind` above is derived from what the env HOSTS. `lifecycle` is declared by
the env itself, and it says WHO APPLIES it:

| `Bundle.lifecycle` | Means |
|---|---|
| unset | **A REAL ENVIRONMENT that declared nothing.** Direct apply works today and prints one notice; it will be refused (F-REFUSE-DIRECT) for an env that declares neither this nor `applier`. |
| `"local"` | A developer's OWN cluster — the `forge env up` inner loop. |
| `"ephemeral"` | A THROWAWAY per-run cluster (CI / e2e), deleted inside the run, so nothing could converge it. |

`lifecycle` has a sibling for the opposite case, a real env that **forge** applies:

```kcl
_bundle = forge.Bundle {
    project = "acme"
    lifecycle = "local"      # dev: your own k3d cluster, applied to directly
    cluster_target = _k3d
}
```

Unset is the safe default deliberately: an env whose author declared nothing
is treated as production, not as scratch. The scaffolded `dev` env declares
`local`; `staging` and `prod` declare nothing, which is correct for them.

The two fields are easy to conflate because both use the word "local", and
they are not the same claim. `kind` local means nothing is hosted;
`lifecycle` local means forge may apply directly. A hosted env can still be
a throwaway cluster, and an env with no control plane at all can still be a
real environment nobody should apply to by hand — which is exactly why the
derived field could not be reused here.

**It is checked, not trusted.** A declared lifecycle may only target a k3d
cluster forge itself stands up — declared in `Bundle.clusters`, or named by
k3d's `k3d-<name>` context convention (the `deploy/k3d.yaml` cluster). EVERY
placement counts, not just the workloads': `cluster_target`, a pinned
Gateway's or route's cluster, a `forge.HelmChart`'s, a raw `forge.Manifests`
group's, a placed `forge.RenderedSecret`'s. An env is only as local as its
least local target, so a cloud env cannot declare itself local to keep direct
apply: the render fails, naming the offending context.

`forge env shape --json` and `forge env status --json` both report
`lifecycle` (on `env status`: top-level `lifecycle` for one env, `environments[].lifecycle` for the all-envs view; empty string when unset), and a direct cluster apply to an env that declares neither `lifecycle` nor `applier` prints
one notice saying direct apply will be refused once F-REFUSE-DIRECT lands. That
is the fields' whole effect today — they refuse nothing — and declaring one now
is what makes retiring direct apply a flag flip rather than a migration.

## `applier = "forge"`: a real env that forge applies on purpose

`lifecycle` says the clusters are scratch. The platform's own env is the opposite:
**real** clusters, applied by forge (an operator or CI running `forge env deploy`),
because the control plane that would converge it is the very thing running there.
A hub applying the env that runs the hub is circular, so for that env forge is the
applier permanently, not as a stopgap.

```kcl
control_plane = forge.ControlPlane {}
applier = "forge"
```

| effect | how |
|---|---|
| exempt from F-REFUSE-DIRECT | `DirectApplyAllowed` is true; no notice is printed |
| never hub-converged | `hubConverged = false` is recorded however fully the clusters are bound to connected clusters; binding says WHERE an env lands, not who puts it there |
| recorded in the shape | `shape.applier = "forge"` in `forge env shape --json`; an env that declares nothing records nothing, so "undeclared" and "applied on purpose" stay distinguishable |

Declaring both `applier` and `lifecycle` is refused at render: they answer one
question opposite ways. Unlike `lifecycle` it is **not** checked against the
clusters, because a real cluster is exactly what it is for.

## Recording it is what makes the console work offline

`forge env build <env>` and `forge env deploy <env>` record this same
projection on the control plane for any env that declares one — the build
does it BEFORE building or cutting anything, so a build that fails half-way
has still said what the env is. `--plan` and `--dry-run` record nothing.

That one idempotent write is what lets a console answer "what kind, which
secrets, which provider" for an environment with no daemon online and no
checkout anywhere, and what makes a never-deployed env read as "declared, not
built yet" instead of as absent.

Two things ride with it. The shape itself, and `declared_by`: the provenance
of the render — commit, branch, dirty flag, tree hash, and the forge version,
because a render is a function of (forge, KCL, config) and the same commit
rendered by two forge versions is two different artifacts. The worktree PATH
is stripped before anything leaves the machine; it names a person's home
directory.

An ensure that has NOT rendered the env — `forge secret set`, a promote —
sends no shape at all, and an absent shape leaves the stored one untouched.
That is load-bearing rather than tidy: an inferred shape from a command that
could not render would erase the declaration a build recorded.
