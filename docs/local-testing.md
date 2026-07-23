# Local testing

Two ways to exercise the controller against a real cluster: build and load an
image into a KinD cluster (closest to how it'll actually run), or run the
`manager` binary on your host against any cluster's kubeconfig (fastest
iteration loop). Both need the CRD installed first.

Prerequisites: `kind`, `kubectl`, and a container tool (`docker` by default,
see `CONTAINER_TOOL` in the `Makefile`).

## 1. Create a KinD cluster and install the CRD

```sh
kind create cluster --name sch-audit-dev

# Points kubectl/the manager at the new cluster; `kind create cluster` also
# does this automatically, but it's worth confirming your context switched.
kubectl config current-context   # kind-sch-audit-dev

make install                     # applies config/crd via kustomize
kubectl get crd schedulingdecisions.scheduling.purestorage.io
```

`make uninstall` removes the CRD again. Since `SchedulingDecision` is
cluster-scoped, `kubectl get sdec -A` lists everything regardless of
namespace.

## 2a. Run the binary on your host (fastest loop)

No image build, no load into KinD — just point the manager at your
kubeconfig context and run it:

```sh
make run
```

This runs `go run ./cmd/manager/main.go` using `ctrl.GetConfigOrDie()`, which
resolves the cluster the same way `kubectl` does (in-cluster config if
present, otherwise `$KUBECONFIG` or `~/.kube/config`'s current context) — so
make sure `kubectl config current-context` is pointed at the cluster you want
before running it. Leader election is off by default in this mode, which is
fine for a single local instance.

Use this loop for iterating on reconciler logic: edit code, `Ctrl-C`,
`make run` again. Create/delete pods in another terminal and watch
`SchedulingDecision` objects appear:

```sh
kubectl get sdec -A -w
```

## 2b. Build an image and load it into KinD (closer to production)

Use this to verify the Dockerfile, RBAC, and Deployment manifests actually
work together — e.g. before a release, or when debugging something that only
reproduces in-cluster (image permissions, distroless base, probes).

```sh
make docker-build IMG=sch-audit:dev
kind load docker-image sch-audit:dev --name sch-audit-dev
make deploy IMG=sch-audit:dev
```

`make deploy` applies `config/default` (namespace `sch-audit-system`,
RBAC, and the `controller-manager` Deployment) with the image set to
`sch-audit:dev`. Since that tag isn't in a registry, `kind load` is what
makes it resolvable inside the cluster — the Deployment's default
`imagePullPolicy` will otherwise try to pull it and fail with `ErrImagePull`.

Verify it's running and watch logs:

```sh
kubectl -n sch-audit-system get pods
kubectl -n sch-audit-system logs -l control-plane=controller-manager -f
```

After making code changes, repeat the three commands above (rebuild, reload,
`kubectl rollout restart deployment/sch-audit-controller-manager -n
sch-audit-system` — `make deploy` alone won't pick up a same-tag image change
without a restart, since the Deployment spec is unchanged).

Tear down:

```sh
make undeploy
kind delete cluster --name sch-audit-dev
```

## Generating scheduling activity to observe

Either mode needs pods actually being scheduled/failing/preempted to produce
`SchedulingDecision` records. A one-off pod exercises the `Scheduled` path:

```sh
kubectl run test-pod --image=busybox --restart=Never -- sleep 3600
kubectl get sdec -A
kubectl delete pod test-pod
```

For `FailedScheduling`, request more CPU than any node has:

```sh
kubectl run unschedulable-pod --image=busybox --restart=Never \
  --overrides='{"spec":{"containers":[{"name":"unschedulable-pod","image":"busybox","command":["sleep","3600"],"resources":{"requests":{"cpu":"1000"}}}]}}'
```

There's no built-in way to trigger `Preempted` without priority classes and
contention set up deliberately; see `docs/PLAN.md` for the KubeVirt
virt-launcher scenario this outcome is meant to cover.

## 3. Testing retention (the sweep CronJob)

Every `SchedulingDecision` gets a `scheduling.purestorage.io/expires-at`
label (a Unix timestamp) at creation time, set to now plus the manager's
`--retention-window` (default 72h). `cmd/sweep` is a one-shot binary that
lists all decisions and deletes any whose `expires-at` has passed; in a real
deployment it runs hourly via the `sweep` CronJob in
`config/manager/sweep_cronjob.yaml`, sharing the same image and
ServiceAccount as the controller manager.

Waiting 72h isn't practical for testing, so shrink the window instead of
waiting it out:

```sh
# In 2a (host) mode: run the manager with a short window instead of `make run`
# (which doesn't take flags), then run sweep directly against the same
# context once a record exists and has expired.
go run ./cmd/manager/main.go --retention-window=30s
# ...create a pod in another terminal, wait >30s, then:
go run ./cmd/sweep/main.go
kubectl get sdec -A   # the expired decision should be gone
```

In 2b (KinD) mode, the CronJob deploys automatically as part of
`config/default`; check it landed and trigger it on demand rather than
waiting for its hourly schedule (note the `sch-audit-` name prefix kustomize
adds, same as `sch-audit-controller-manager`):

```sh
kubectl -n sch-audit-system get cronjob sch-audit-sweep
kubectl -n sch-audit-system create job --from=cronjob/sch-audit-sweep manual-sweep-1
kubectl -n sch-audit-system logs job/manual-sweep-1
```

`cmd/sweep`'s summary log line (`sweep complete: N total, N deleted, ...`)
is the quickest way to confirm it did what you expect without having to diff
the full `sdec` list before/after.
