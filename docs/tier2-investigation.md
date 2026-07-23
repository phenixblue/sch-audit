# Tier 2 investigation: per-node score capture

Milestone 6. This is a research spike, not an implementation — per
`docs/PLAN.md`, Tier 2 was always "post-v1, investigate," specifically to
answer: *can sch-audit capture real per-node scheduling scores, and does
STORK's extension mechanism support it?*

**Bottom line: no, not for the two schedulers this project actually
targets, without upstream cooperation sch-audit doesn't control.** Details
and a smaller alternative below.

## What Tier 1 is missing, and why

`SchedulingDecision.spec.candidateNodes[]` (nee `spec`, now per-transition —
see the design-revision note in `docs/PLAN.md`) has a `score` field that
Tier 1 leaves permanently empty. Tier 1 only sees Kubernetes `Event`
objects, and a `FailedScheduling` Event is one aggregate string across every
node ("0/6 nodes are available: ..."), not a per-node breakdown — there's
no per-node data to extract even for a *failed* attempt, let alone the
scores of nodes that passed filtering on a *successful* one. That data only
exists transiently inside the scheduler process itself while it's
computing a decision.

## Two ways to reach inside a scheduler, and what each actually exposes

**Scheduler Extender** — a legacy (pre-scheduling-framework) mechanism: an
external HTTP service registered in `KubeSchedulerConfiguration.extenders[]`
that the scheduler calls out to at exactly two points, `filter` and
`prioritize`. No compilation or redeployment of the scheduler binary is
needed — it's pure configuration. But the `prioritize` call only receives
the pod and the *already-filtered* node list; the extender returns its own
per-node scores, which get added on top of whatever the scheduler's
built-in plugins computed — it never sees those built-in plugins' own
per-node scores or a breakdown of why they differed
([kubernetes/enhancements#1819](https://github.com/kubernetes/enhancements/blob/master/keps/sig-scheduling/1819-scheduler-extender/README.md)).
So an extender can tell us *which nodes were still in the running after
Filter* — filling in `candidateNodes[].name` — but not *why one beat
another* on score.

**Scheduling Framework plugins** (`Score`/`Reserve`/`PreBind`/etc.) — the
modern mechanism, with far more extension points and full visibility into
the scheduling cycle, including real per-plugin scores. The catch: plugins
are Go code registered via `app.WithPlugin(...)` and **compiled into the
scheduler binary at build time**
([k8s.io/kubernetes/cmd/kube-scheduler/app](https://pkg.go.dev/k8s.io/kubernetes/cmd/kube-scheduler/app);
reference implementations at
[kubernetes-sigs/scheduler-plugins](https://github.com/kubernetes-sigs/scheduler-plugins)).
There's no way to attach a new plugin to an already-built, already-running
scheduler binary — you either rebuild that exact binary with your plugin
added, or you run a wholly separate scheduler (with your plugin compiled
in) that pods opt into via `schedulerName`.

## What's actually running on the target stack (verified against a live cluster)

Checked directly against the aetos-ocp1 OpenShift + Portworx + STORK
cluster used for Milestone 2-5 validation:

- **STORK is not a custom-compiled scheduler.** `stork-scheduler`'s
  container image is `registry.k8s.io/kube-scheduler-amd64:v1.31.14` — the
  stock upstream binary — configured via a `KubeSchedulerConfiguration`
  (`stork-config` ConfigMap in the `portworx` namespace, owned by the
  Portworx operator's `StorageCluster` CR) that registers one extender:

  ```yaml
  extenders:
  - filterVerb: filter
    prioritizeVerb: prioritize
    urlPrefix: http://stork-service.portworx:8099
    weight: 5
  ```

  STORK's storage-locality logic lives entirely in the separate `stork`
  Deployment answering those two HTTP verbs — the scheduling-framework
  plugin set in that same config is the exact stock list (`NodeAffinity`,
  `NodeResourcesFit`, `PodTopologySpread`, `VolumeBinding`, etc.), unmodified.
  This also resolves the open question from `docs/PLAN.md` about
  `schedulerName` attribution: yes, it's clean — Events/conditions reliably
  show `reportingController: stork` (confirmed repeatedly during Milestone
  2/5 testing), because STORK really is registered as a distinct
  `schedulerName: stork` profile, not a disguised extension of
  default-scheduler.

- **OpenShift's actual default-scheduler has no extender and no custom
  profile.** Its live `config.yaml` (`openshift-kube-scheduler` namespace)
  contains only `clientConnection`/`leaderElection` — no `extenders:`, no
  `profiles:`. The `KubeScheduler` operator CR's only configuration escape
  hatch is `spec.unsupportedConfigOverrides` — a field OpenShift explicitly
  labels unsupported, which the cluster-kube-scheduler-operator can revert
  and which risks support agreements on a production/managed cluster. There
  is no supported path to attach an extender, let alone a custom-compiled
  plugin, to the scheduler actually handling the bulk of cluster workloads.

- **Portworx's `StorageCluster` CR doesn't obviously expose a supported
  knob for adding a second extender** alongside STORK's own (a quick check
  of the live object's spec found nothing; this wasn't an exhaustive schema
  review, so treat as "not found" rather than "confirmed absent" without
  filing the question upstream).

## Recommendation

**Defer full Tier 2** (real per-node score capture). It isn't a sch-audit
engineering problem to solve alone — it requires either Red Hat adding a
supported extender/plugin hook to OpenShift's managed kube-scheduler, or
Portworx accepting an additional extender registration for STORK, or
running a wholly separate opt-in scheduler that most existing workloads
would need to switch to just to get instrumented. None of that is something
to take on speculatively without a concrete user need for per-node score
visibility specifically (as opposed to the outcome/reason/latency data Tier
1 already captures).

**A smaller, real win is still on the table, cheaply, if it's ever wanted:**
registering sch-audit's own observer extender (alongside STORK's, if
Portworx's operator tolerates a second `extenders[]` entry, or standalone
on any cluster where a supported extender path exists) would populate
`candidateNodes[].name` — the post-Filter candidate list — for schedulers
it can attach to. That's real, currently-missing information Tier 1 can't
produce from Events alone, and needs no custom scheduler build. It still
wouldn't include `candidateNodes[].score`, since that's exactly the data an
extender never sees.

## Addendum: API / metrics / log scraping, checked directly

A natural follow-up: is there some *other*, non-plugin way to observe
per-node scores — the Kubernetes API, a metrics endpoint, or log scraping?
Checked each against the live cluster and current upstream docs; all three
are dead ends for this specific question.

- **API**: no dedicated object exists anywhere in stock Kubernetes for
  scheduling internals. Events are the only audit-trail-shaped surface, and
  (as above) a `FailedScheduling` Event is one aggregate string, not a
  per-node breakdown.

- **Metrics**: attempted to scrape `stork-scheduler`'s own `/metrics`
  (it's the stock kube-scheduler binary, so identical metric set to
  default-scheduler) but hit an RBAC delegation error on the auth check —
  moot regardless, since the metric *names* already rule this out. The
  relevant histograms, `scheduler_framework_extension_point_duration_seconds`
  and `scheduler_plugin_execution_duration_seconds`, are labeled only by
  `plugin`/`extension_point`/`status`/`profile` — latency, not decision
  content; no pod, node, or score value label. That's inherent to the
  Prometheus metrics model, not a gap: a per-pod-per-node label would be a
  cardinality explosion. A separate `/metrics/resources` endpoint reports
  each pod's *already-scheduled* node, but that's the same final-outcome
  data Tier 1 already has from the Pod object - no candidates, no scores.

- **Logs**: kube-scheduler has historically had a debug line
  (`"Host %s => Score %d"` in `generic_scheduler.go`) dumping per-node
  scores, but community reports say it needs `-v=10` and is unreliable even
  there - log lines aren't covered by Kubernetes' API stability guarantees
  and can vanish or move between versions without notice. Concretely for
  this project's target stack: OpenShift's `KubeScheduler` operator API
  (`openshift/api`'s `operator/v1` `LogLevel` type) only goes up to
  `TraceAll`, mapped to `-v=8` - *below* the `-v=10` this needs. Even
  OpenShift's maximum *supported* scheduler verbosity can't reach this data;
  doing so would mean falling back to the same `unsupportedConfigOverrides`
  escape hatch already flagged as risky above, with no guarantee the log
  line still exists in-version.

- **Tracing** (checked as a fourth angle, since Kubernetes has been adding
  OpenTelemetry support to some components): the
  [Traces For Kubernetes System Components](https://kubernetes.io/docs/concepts/cluster-administration/system-traces/)
  docs confirm `--tracing-config-file` support for kube-apiserver and
  kubelet, but don't mention kube-scheduler at all. No evidence this is a
  supported mechanism there.

None of these beat the Extender-based partial option above. Real per-node
score values only ever exist transiently inside the scheduler process
during the Score/NormalizeScore extension points - there's no external tap
for that data in stock Kubernetes.
