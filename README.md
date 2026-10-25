# mayfly

A Kubernetes operator that manages **ephemeral preview environments**. You declare an `Environment`; mayfly creates a namespace with your workloads, keeps them converged, and tears everything down when the TTL expires.

> Built with Kubebuilder and controller-runtime.

```yaml
apiVersion: preview.example.dev/v1alpha1
kind: Environment
metadata:
  name: pr-1234
spec:
  ttl: 2h                      # lifetime, measured from creation
  labels:
    team: platform             # propagated to the namespace and workloads
  workloads:
  - name: web
    image: nginxinc/nginx-unprivileged:1.27-alpine
    port: 8080
    replicas: 2
```

```console
$ kubectl get environments
NAME      READY   EXPIRES                NAMESPACE      AGE
pr-1234   True    2026-10-24T20:30:00Z   mayfly-pr-1234 3m
```

## What it does

For each Environment, the controller:

1. Adds a finalizer.
2. Creates the namespace `mayfly-<name>`, owned by the Environment.
3. Creates a Deployment and a ClusterIP Service per workload, also owned by the Environment.
4. Reports `Ready`, `Progressing` and `Degraded` conditions, plus `expiresAt`, `namespace` and `observedGeneration`.
5. Sleeps until expiry (`RequeueAfter`), then deletes the Environment. The finalizer deletes the namespace and waits for it to disappear.

## API

| Field | Notes |
|---|---|
| `spec.ttl` | Go duration (`30m`, `1h30m`). Seconds, minutes or hours only. Expiry is `creationTimestamp + ttl`, so editing it extends or shortens the lifetime. |
| `spec.workloads[]` | 1–10 items, keyed by `name` (DNS label). `image`, `port` (1–65535), `replicas` (0–10, default 1). |
| `spec.labels` | Optional, max 20. Keys under `preview.example.dev/` are reserved and rejected. |
| `status.conditions` | `Ready`: every workload has all replicas updated and available. `Progressing`: still converging. `Degraded`: a user error such as an invalid TTL. |

Validation lives in the CRD (kubebuilder markers and CEL), so bad input is rejected by the API server rather than discovered at reconcile time. The Environment name is capped at 50 characters so `mayfly-<name>` fits in a 63-character namespace name.

## Design decisions

**Cluster-scoped Environment owns the Namespace through a real ownerReference.**
The ownership rule runs one way: a *namespaced* owner can't own a *cluster-scoped* object, but a *cluster-scoped* owner can own both. Making `Environment` cluster-scoped lets it own the Namespace and every workload inside it. That gives garbage collection as a safety net, `Owns()` watches that re-trigger reconciles, and ownership that is easy to assert in tests. The alternative (a namespaced Environment tracked only by labels) would need hand-written cleanup and could leak namespaces.
*Tradeoff:* a cluster-scoped CR can't be granted per-namespace with RBAC. That fits a platform-team tool, less so a self-service one. A `preview.example.dev/environment` label is also set on everything so resources can be found even if ownerReferences are stripped.

**The finalizer deletes the namespace explicitly and waits for it to be gone.**
Relying on GC would be asynchronous and hard to test, and releasing the finalizer while the namespace is still `Terminating` would report completion too early. The controller deletes the namespace, keeps the finalizer until a `Get` returns NotFound, and is woken by the namespace's delete event via `Owns()`. It does not poll. It never deletes a `mayfly-*` namespace it doesn't control, even on a name collision.

**TTL via `RequeueAfter`, not polling.** The only timer is "time until expiry". Readiness changes arrive through `Owns(Deployment)` watches. Expiry is derived from `creationTimestamp`, so it survives restarts with no stored state. The clock is injectable so the logic stays testable.

**Idempotent, conflict-tolerant reconcile.** Children use `CreateOrUpdate` and only mutate fields the controller owns. The Deployment selector is set on create only, because it is immutable. A Service's `ClusterIP` and server-defaulted fields are left alone, so there are no update loops. Namespace labels are merged, not replaced, to keep API-server-managed labels. Status is written with a merge patch, so there is no resourceVersion conflict against spec or finalizer updates. Conflicts elsewhere are retried by controller-runtime's backoff.

**User errors are not retried.** An invalid TTL sets `Degraded=True` and waits for a spec change, because retrying can't fix it. Spec fixes clear it.

## Layout

```
api/v1alpha1/                 CRD types and validation markers
internal/controller/
  environment_controller.go   Reconcile, finalizer, children, status
  ttl.go                      pure expiry logic
  workload.go                 pure workload -> Deployment/Service mapping
  *_test.go                   unit tests + envtest suite
test/e2e/                     kind end-to-end tests
config/                       CRD, RBAC, manager and sample manifests
```

## Development

Requires Go (see `go.mod`), Docker, and for e2e [kind](https://kind.sigs.k8s.io/) and `kubectl`. Run on Linux, macOS or WSL.

| Target | What it does |
|---|---|
| `make install` | Install the CRD into the cluster in your current kubeconfig. |
| `make run` | Run the controller locally against that cluster. |
| `make test` | Unit tests and envtest controller tests (downloads the envtest binaries). |
| `make e2e` | Create a kind cluster, build and load the image, run the e2e suite, tear down. |
| `make deploy IMG=...` | Deploy the operator into a cluster. |
| `make manifests generate` | Regenerate CRDs, RBAC and deepcopy code after changing types or markers. |

Quick try:

```sh
kind create cluster
make install
make run            # in one terminal
kubectl apply -f config/samples/preview_v1alpha1_environment.yaml   # in another
kubectl get environments,ns
```

## Testing

- **Unit tests** cover TTL parsing and expiry boundaries, and the workload-to-Deployment/Service mapping (selector/template consistency, default replicas, reserved-label precedence, readiness).
- **envtest** runs the real controller against a real kube-apiserver. envtest has no kubelet, deployment controller, garbage collector or namespace controller, so the suite compensates:
  - readiness is simulated by patching Deployment status
  - ownership is verified by asserting `ownerReferences`, not by expecting garbage collection
  - namespace termination is simulated by clearing the namespace's `kubernetes` finalizer through the `finalize` subresource
  - TTL expiry uses a real short TTL, so only `RequeueAfter` can wake the reconciler
  - CRD validation (patterns, CEL rules, defaults) is exercised against the real API server
- **e2e** deploys the built operator image into kind and checks the full lifecycle with real pods, namespace termination and TTL expiry.

## Scope and limitations (v1)

Intentionally out of scope: Ingress or external access, config and secrets injection, resource requests/limits, pull secrets, webhooks, multi-version CRDs, and quota or per-team limits. Removed labels are not pruned from the namespace once added.

## License

Apache License 2.0. See [LICENSE.md](./LICENSE.md).
