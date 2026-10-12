---
name: deploy/flux
description: Flux in forge — what forge no longer does (install Flux into a deploy target, write a desired-state pointer at one), who applies a real env instead, and the one thing that remains (`forge.flux_chart()`, the pinned chart a consumer declares when it runs a Flux of its own). Use when a skill or an old note says an env without a control plane reconciles through an in-cluster Flux.
---

# Flux

**forge never installs Flux into a deploy target and never writes a
desired-state pointer (OCIRepository + Kustomization) at one.** An earlier
design did both for an env with no control plane; #461 removed it and the code
has since been deleted. If a note says `forge cluster up` installs
`source-controller`/`kustomize-controller`, or that `forge env deploy` "writes the
pointer and waits", it is describing a path that does not exist.

## Who applies a real environment

| the env declares | applied by |
|---|---|
| `forge.ControlPlane` and clusters bound to connected clusters (`forge cluster connect`) | the control plane's hub Flux converges the recorded bundle; forge records and waits (`deploy/cluster-connect`) |
| `applier = "forge"` | **forge**, directly, on purpose, and the hub never converges it (`deploy/shape`) |
| `lifecycle = "local"` / `"ephemeral"` | forge, directly: the clusters are scratch it creates and deletes |
| none of these | forge, directly, with one notice that this will be refused (F-REFUSE-DIRECT) once an env must say which of the above it is |

A Flux on a cluster exists only because a control plane declared one. Flux is a
platform dependency of the control plane, not something forge puts in your
cluster as a side effect.

## What remains: `forge.flux_chart()`

An env that needs Flux for its own reasons (control-plane's e2e runs one to
reconcile hosted workloads) declares forge's pinned chart as an ordinary bootstrap
`forge.HelmChart`:

```kcl
helm_charts = [
    forge.flux_chart(values = {
        # Merged OVER forge's posture, last-wins.
        sourceController.serviceAccount.annotations = {
            "iam.gke.io/gcp-service-account" = "flux@proj.iam.gserviceaccount.com"
        }
    })
    # Multi-cluster: re-target it like any other chart.
    # forge.flux_chart(cluster = _control_plane)
]
```

`forge cluster up <env>` installs the env's declared bootstrap charts (Flux
first, then charts that ship a CRD bundle, then the rest) into the env's
forge-managed k3d clusters; `forge env deploy <env> --target <chart>` installs
just that one.

The chart is **version-pinned with no knob**, and it runs **two controllers, not
six** (`source-controller`, `kustomize-controller`; helm, notification and the two
image controllers are off, a security decision rather than a resource one). The
posture is `--no-remote-bases` and `--no-cross-namespace-refs`, with no
`multitenancy` block: a consumer isolating several authors through the same Flux
turns that on via `values`.

> **Chart version ≠ Flux version.** There is no fluxcd-published chart; forge
> uses the community chart at `oci://ghcr.io/fluxcd-community/charts/flux2`, whose
> version has its own lineage. Chart **2.19.1** is Flux **v2.9.5**. On a bump,
> re-derive it, do not increment:
> `helm show chart oci://ghcr.io/fluxcd-community/charts/flux2 --version <v> | grep appVersion`

Detection of "this env already provides Flux" is **by chart reference, never by
name**: a chart's `name` is the `--target` selector its author picks freely, so a
name heuristic would let a naming choice decide whether a colliding second Flux
was installed. Declare `forge.FLUX_CHART_OCI` and you are recognised, whatever you
call the chart.
