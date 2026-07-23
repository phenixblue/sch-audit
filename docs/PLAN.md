# sch-audit

A durable, queryable, human- and machine-readable record of Kubernetes scheduling
decisions — across the default scheduler, STORK, and any other scheduler
participating in the cluster.

## Problem

Kubernetes scheduling decisions are ephemeral. The Events API (`Scheduled`,
`FailedScheduling`) has a default ~1 hour TTL and a cluster-wide cap around 1,000
objects. Once pruned, the "why did the scheduler put this pod here" story is gone.
This is especially painful when comparing placement behavior across multiple
schedulers and volume plugins (default-scheduler + CSI, STORK + Portworx,
FADA topology constraints) in KubeVirt environments, where the interaction between
volume binding mode, topology, and scheduler identity determines placement.

## Goals

- Durable, queryable historical record of every scheduling decision
- Works across multiple schedulers without modifying them (Tier 1)
- Optional higher-fidelity capture via scheduling framework hooks (Tier 2)
- Human-readable via `kubectl`
- Machine-readable via the Kubernetes API (any client)
- Visual dashboard for quick digestion, built directly off the CRD

## Non-goals (for v1)

- Replacing full observability stacks (Loki/Prometheus/Grafana) — this
  complements them, and can feed them
- Live scheduler decision *prevention* or policy enforcement
- Per-node real-time scoring for every scheduler (STORK score capture is
  investigated in Tier 2, not guaranteed)

## Architecture

### Tier 1 — reconstruction (v1 target)

A controller watches:
1. **Pod informer** — triggers on `spec.nodeName` transitioning from empty to
   set. This is the universal signal: it fires regardless of which scheduler
   performed the bind.
2. **Event informer** — filtered to `reason in (Scheduled, FailedScheduling,
   Preempted)` on `involvedObject.kind=Pod`. Supplies `reportingComponent`
   (which scheduler) and, for failures, the predicate-failure string per node.

On a new binding, the controller:
- Resolves volume context: walks `pod.spec.volumes` → PVC → StorageClass →
  provisioner, mapped to a driver label (`FADA`, `PX-CSI`, `vsphere-csi`, etc.)
- Computes scheduling latency: pod creation timestamp → bind timestamp
- Denormalizes everything into a `SchedulingDecision` custom resource (no
  owner reference to the Pod — KubeVirt virt-launcher pods are short-lived
  and we want the record to outlive them)
- Is idempotent per `pod-uid` label to avoid duplicate records on reconcile

### Tier 2 — enrichment (post-v1, investigate)

A scheduling-framework plugin hooking `Score`/`Reserve`/`PreBind` to capture
real per-node scores at decision time — the only way to see *why* a node was
chosen over a scored-but-rejected runner-up, since Events never expose scores
for nodes that passed filtering. Needed specifically for the FADA/STORK
scheduling-nuance investigations; not required for basic historical tracking.
Whether STORK's extender interface exposes an equivalent hook needs research
before committing to this tier.

## CRD: `SchedulingDecision`

- Group: `scheduling.purestorage.io/v1alpha1`
- Scope: Cluster
- Kind: `SchedulingDecision`, short name `sdec`
- Treated as an immutable log entry, not a reconciled object

Fields: `podName`, `podNamespace`, `podUID`, `schedulerName`, `chosenNode`,
`outcome` (`Scheduled`/`FailedScheduling`/`Preempted`), `reasonSummary`,
`decisionTimestamp`, `schedulingLatencyMs`, `candidateNodes[]` (`name`,
`filterResult`, `score` — score populated only by Tier 2), `volumeContext`
(`pvcName`, `storageClass`, `driverType`, `bindingMode`, `topologyConstraint`),
`sourceRef` (`eventUID`, `auditRequestID`).

Printer columns expose Pod, Scheduler, Node, VolumeDriver, Outcome, LatencyMs,
Age directly to `kubectl get sdec`.

## Retention

- `expires-at` label per CR, swept by a CronJob; keep a rolling ~72h hot
  window in-cluster for live `kubectl`/dashboard queries
- Anything older exports out of the cluster the same way as Kubernetes
  Events would (event-exporter style watcher pointed at the CRD instead of
  the Events API), landing in Loki/Elasticsearch for long-term retention —
  this reuses the CRD as the canonical schema across both retention tiers

## Dashboard / tooling

Reads only from the CRD surface (no dependency on Loki/audit logs):
- `kubectl get schedulingdecisions` for the free CLI table (printer columns)
- Small purpose-built web dashboard (in-cluster or via `kubectl proxy`):
  summary stat cards, node-placement heatmap by scheduler, filterable recent-
  decisions table, latency-by-scheduler chart
- Stretch: Grafana panel via the Kubernetes/Infinity datasource pointed
  directly at the CRD's list endpoint, for teams already standardized on
  Grafana

## Repo structure (proposed)

```
sch-audit/
  api/v1alpha1/           # CRD types, deepcopy, CRD YAML generation (kubebuilder)
  controllers/            # SchedulingDecision reconciler (Tier 1)
  config/
    crd/                  # generated CustomResourceDefinition manifests
    rbac/                 # ClusterRole/ClusterRoleBinding for the controller
    manager/              # controller Deployment manifest
  cmd/manager/            # main.go entrypoint
  dashboard/              # standalone web dashboard reading the CRD via k8s API
  docs/
    PLAN.md               # this document
  hack/                   # dev scripts (kind cluster, sample workloads)
```

## Milestones

1. **Scaffold** — kubebuilder project init, CRD types + generated manifests,
   RBAC, empty reconciler that just logs
2. **Tier 1 reconciler** — Pod + Event watches, volume-context resolution,
   SchedulingDecision creation, idempotency
3. **Retention** — expiry labels + sweep CronJob
4. **Dashboard v1** — read-only web UI against the live CRD (stat cards,
   heatmap, table, latency chart)
5. **Validate against a real cluster** — deploy alongside default-scheduler +
   STORK, confirm decisions are captured correctly for FADA, PX-CSI, and
   vsphere-csi backed workloads
6. **Tier 2 investigation** — spike a scheduling-framework plugin for
   per-node score capture; assess STORK extender compatibility

## Open questions

- Does STORK's extender interface expose enough to attribute `schedulerName`
  cleanly, or does it always show as the underlying scheduler it wraps?
- Cluster-scoped vs namespaced CRD — cluster-scoped is simpler for a
  cross-namespace dashboard view, but namespaced would align RBAC more
  tightly with existing per-namespace access patterns. Defaulting to
  cluster-scoped for v1.
- Export sink for cold storage (Loki vs Elasticsearch vs S3) — deferred
  until Tier 1 is validated and volume/retention needs are clearer.

## Design revisions

**2026-07-22 — spec/status split, transition history.** The original CRD
design above treated a `SchedulingDecision` as a single immutable write-once
record (everything under `.spec`, no status subresource). Validating against
a real OpenShift cluster running STORK + Portworx (Milestone 5, pulled
forward) showed this was wrong: a StorageClass with `Immediate`
`volumeBindingMode` makes STORK (and any scheduler) report a transient
`FailedScheduling` ("pod has unbound immediate PersistentVolumeClaims")
while the PVC is still provisioning, then `Scheduled` once it binds and the
scheduler retries. Under the original design, the reconciler committed
whichever outcome it observed first and never revisited it, so retried pods
were permanently misrecorded as failed even though they were actually
running.

The CRD was revised so `.spec` holds only what's stable for a pod's whole
lifetime (`podName`, `podNamespace`, `podUID`, `schedulerName`,
`volumeContext`) and is set once, while a new `.status` subresource holds the
latest observed outcome (`outcome`, `chosenNode`, `reasonSummary`,
`decisionTimestamp`, `schedulingLatencyMs`) plus `status.transitions[]`, an
ordered history of every observed outcome for the pod. The reconciler now
appends a transition whenever it observes a new outcome instead of writing
once and never touching the record again; a retry loop or a
preempted-after-scheduled sequence shows up as multiple transitions rather
than silently overwriting or freezing on the wrong one. `candidateNodes` and
`sourceRef` moved from top-level spec fields to per-transition fields, since
they describe a single scheduling attempt, not the pod as a whole. Printer
columns for Node/Outcome/LatencyMs now read from `.status` instead of
`.spec`.

**2026-07-23 — dashboard implemented as `cmd/dashboard`, not a standalone
`dashboard/` app.** The repo structure above proposed a separate
`dashboard/` top-level directory, envisioned as possibly its own tech stack.
Milestone 4 instead ships it as a fourth Go binary (`cmd/dashboard`,
alongside `manager` and `sweep`) that serves a single self-contained HTML
page (inline CSS/JS, no build step, no CDN dependencies) compiled in via
`go:embed`, backed by one JSON endpoint (`/api/decisions`) that all stat
cards/heatmap/latency-chart/table rendering happens against client-side. It
shares the same container image, Dockerfile stage, and RBAC (read-only
`get`/`list`/`watch` on `schedulingdecisions`, already granted) as the other
two binaries — one build/publish pipeline for the whole project, consistent
with how retention's `cmd/sweep` was done in Milestone 3.

**2026-07-23 — Milestone 5 (validate against a real cluster) status.**
default-scheduler, STORK, and PX-CSI (Portworx) were validated end-to-end
against a live OpenShift cluster (Portworx + STORK + OpenShift
Virtualization) during Milestones 2-4's development: real Pods, VMs, and
Portworx-backed PVCs produced correct `SchedulingDecision` records,
including the transient-`FailedScheduling`-then-`Scheduled` retry sequence
that motivated the spec/status redesign above, and the dashboard rendering
that data correctly. FADA (Pure Storage FlashArray CSI) and vsphere-csi
weren't reachable from this environment (no FlashArray or vSphere cluster
available), so those two are validated only via envtest integration tests
(`controllers/schedulingdecision_controller_test.go`'s
"resolves volume context for a StorageClass's provisioner" table, which
also covers PX-CSI) rather than a real array/vSphere-backed cluster. The
provisioner→driver-label mapping is a static lookup shared across all
entries in `provisionerDriverLabels`, so this is low-risk, but it's a real
gap if either array type surfaces its own scheduling quirks the way STORK's
Immediate-binding retry loop did.
