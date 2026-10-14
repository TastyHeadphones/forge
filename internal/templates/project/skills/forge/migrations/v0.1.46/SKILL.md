---
name: v0.1.46
description: Upgrade a project to forge v0.1.46. One section per change — (1) `forge.flux_chart()` is hardened and sized by default (behavior change - per-namespace controller RBAC replaces the chart's cluster-wide Secret read and impersonate-any-ServiceAccount grants, multitenancy on, NetworkPolicies narrowed, concurrency and resources raised, a PriorityClass, Pod Security restricted on flux-system), and gains `rbac`, `placement`, `scrape_namespaces` and friends. Use when deploy/kcl declares `forge.flux_chart(`, or a chart override re-declares Flux's ClusterRoles, NetworkPolicies or controller args.
version: v0.1.46
detection: grep -qsE 'forge\.flux_chart\(|flux_chart\(' deploy/kcl/*/*.k deploy/kcl/lib/*.k
---

# Upgrading to v0.1.46

Each section below is independent: apply the ones whose "Use when" matches.

## 1. Hardened `forge.flux_chart()`

Use when an env declares `forge.flux_chart(...)`, or carries chart overrides that re-declare Flux's RBAC, NetworkPolicies or controller args by hand.

### What changed (BEHAVIOR CHANGE)

`forge.flux_chart()` with no arguments used to install the community chart with only `--no-remote-bases`, `--no-cross-namespace-refs` and four controllers off. It now installs a Flux hardened and sized for one controller set shared by several authors. Every default below is overridable (see Steps).

| Area | Before | Now (default) |
|---|---|---|
| Controller RBAC | `crd-controller` read every Secret/ConfigMap cluster-wide; `cluster-reconciler-impersonator` could impersonate ANY ServiceAccount; leader election was a cluster-wide ConfigMap write | `rbac = "namespaced"`: Flux CRs cluster-wide; leader election is a Role in `flux-system`; impersonation (pinned by `resourceNames` to `reliant-deploy-tenant`) and Secret reads exist only where a RoleBinding names `flux-tenant-reconciler` / `flux-tenant-source` |
| Multitenancy | off | on, `privileged = False` (no cluster-admin binding), `--default-service-account=reliant-deploy-tenant` |
| Config watcher | on | off (`DisableConfigWatchers`); a rotated kubeconfig is picked up at the next interval |
| Head-of-line | `--concurrent=4`, `--concurrent-ssa=4`, `--requeue-dependency=30s`, a stuck health check blocks a fix-forward | kustomize-controller `--concurrent=16 --concurrent-ssa=8 --requeue-dependency=10s`, `CancelHealthCheckOnNewRevision`; source-controller `--concurrent=8` |
| Resources | 100m/64Mi, no limits | kustomize 500m/512Mi (limits 2/2Gi); source 250m/256Mi (limits 1/1Gi) |
| Priority | none | PriorityClass `forge-delivery` (1000000), declared with the chart |
| NetworkPolicies | `allow-egress`, `allow-scraping` (:8080 from every namespace), `allow-webhooks` | `allow-egress` only (ingress from `flux-system` pods only); `allow-scraping` only for `scrape_namespaces`; no `allow-webhooks` |
| Namespace | no labels | Pod Security `restricted` (enforce/warn/audit) on `flux-system` |

### Steps

1. **Per-namespace RBAC needs a binding in every namespace that holds Flux objects** (OCIRepository, Kustomization, or the Secret they reference). Without it the controller can neither impersonate there nor read the Secret, and the source/apply fails `Forbidden` (fail-closed). Declare it with the namespace:

   ```python
   forge.flux_namespace_bindings("flux-acme")   # two RoleBindings, for forge.Manifests
   ```

   A Go writer binds `flux.ReconcilerRole` (kustomize-controller) and `flux.SourceRole` (source-controller) by name. The destination namespace needs nothing: the apply, health check and prune run as the impersonated identity.
2. **Cannot write bindings yet?** Step in: `forge.flux_chart(rbac = "pinned")` keeps the chart's cluster-wide reads but pins impersonation to one ServiceAccount. `rbac = "chart"` restores the chart's own RBAC and policies exactly (and `network_policies = "chart"`, `pod_security = ""`, `priority_class_name = ""`, `priority_class_create = False` undo the rest).
3. **Impersonating a different ServiceAccount:** `impersonate_service_account = "..."`. It changes the default, the pin and the role together.
4. **Remove your hand-written copies.** If the env re-declared the chart's `crd-controller`, `cluster-reconciler-impersonator`, the NetworkPolicies, `rbac.create = False`, `policies.create = False`, the controller `additionalArgs`, or a flux-system PSA label, delete them: they now duplicate (and, for `extraObjects`, collide with) the defaults. `values.extraObjects` is APPENDED to forge's, not substituted. `values` still merges over the posture, last wins, so a value you want different (`kustomizeController.resources...`) stays a one-line override. A list value (`additionalArgs`) replaces the default list; use the typed arguments (`concurrency`, `feature_gates`) instead.
5. **Metrics:** nothing outside `flux-system` reaches :8080 by default. Name the scraper: `scrape_namespaces = ["observability"]`.
6. **Chart objects the old render left behind** (the previous `allow-webhooks`, `cluster-reconciler-impersonator`) are removed by the chart prune (`migrations/v0.1.45/helm-chart-prune`) on the first apply, if forge stamped them; objects dropped before that stamp must be deleted by hand once.
7. **Placement** is opt-in (a default must run on every cluster): `placement = forge.flux_placement_delivery_v1()` for a dedicated tainted node pool, and `runtime_class_name = forge.FLUX_RUNTIME_CLASS_GVISOR` only on a target that has the RuntimeClass (a pod naming one the cluster lacks never schedules). Provision the pool yourself; forge has no node-pool primitive.

### Verify

`forge env render <env> --target flux` shows no `cluster-reconciler-impersonator` ClusterRole, a `flux-tenant-reconciler` ClusterRole whose impersonate rule carries `resourceNames`, one NetworkPolicy (`allow-egress`), and `kustomize-controller` args including `--feature-gates=CancelHealthCheckOnNewRevision=true,DisableConfigWatchers=true`. After deploy: `kubectl auth can-i impersonate serviceaccounts --as=system:serviceaccount:flux-system:kustomize-controller -n default` is `no`, and `kubectl auth can-i get secrets -A --as=system:serviceaccount:flux-system:kustomize-controller` is `no`.
