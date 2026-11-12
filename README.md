# Zabbix Kubernetes Operator

A Kubernetes operator that deploys and manages production-grade
[Zabbix](https://www.zabbix.com) installations — server with native high
availability, frontend, web service, and proxies — on a PostgreSQL database
managed by [CloudNativePG](https://cloudnative-pg.io) or provided externally.

**Status: development.** The v1alpha1 API and the core reconcile loops are
implemented and lab-validated on kind (see
[Roadmap](#roadmap)); no release has been published yet.

## What it does

Declarative, level-triggered management of a complete Zabbix deployment:

- **Zabbix Server** with native HA (N replicas, heartbeat failover,
  operator-managed node lifecycle)
- **Frontend** and **Web service** (scheduled reports)
- **Proxy group** (multi-replica StatefulSet with per-pod DNS)
- **Database**: CloudNativePG cluster (HA, backups, version upgrades) or an
  external database supplied via a Kubernetes Secret
- Status conditions, events, and drift repair for everything it owns

Design follows the Kubernetes operator pattern throughout: desired state is
declared in custom resources, the controller converges actual state toward
it, and nothing depends on imperative re-invocation.

## Scope (v1)

Decisions are tracked with rationale in
[docs/requirements.md](docs/requirements.md).

- **Components**: Server (HA) + frontend + web service + proxy group.
  Java Gateway, Agent DaemonSet mode, Browser Monitoring and MCP server are
  later versions.
- **Database**: CloudNativePG integration first; external database via
  existing Secret second.
- **Versions**: Zabbix 8.0 (primary) and 7.0 LTS (install & run target).
  Cross-version upgrade orchestration is out of scope for v1.
- **Environments**: kind for development/tests; production-ready by design
  (HA, probes, resource management, security contexts, RBAC least privilege).
- **Distribution**: plain-YAML install bundle, Helm chart for the operator
  itself later.

## Observability

**Operator metrics.** The manager exposes the standard
controller-runtime metric families (`controller_runtime_*`,
`workqueue_*`, `go_*`, process metrics) on `:8443/metrics`, reached
through the `zabbix-k8s-operator-controller-manager-metrics-service`
Service. The endpoint is HTTPS with self-signed serving certs and is
protected by controller-runtime's authentication/authorization filters:
every request is authenticated with a `TokenReview` and authorized with
a `SubjectAccessReview`, so scrapers must present a service account
token whose identity holds the bundled `zabbix-k8s-operator-metrics-reader`
ClusterRole (grants `get` on the `/metrics` non-resource URL). Probe it
locally with:

```sh
TOKEN=$(kubectl -n zabbix-k8s-operator-system create token <scraper-sa>)
kubectl -n zabbix-k8s-operator-system port-forward \
  svc/zabbix-k8s-operator-controller-manager-metrics-service 8443:8443
curl -sk -H "Authorization: Bearer $TOKEN" https://127.0.0.1:8443/metrics
```

For Prometheus, bind `metrics-reader` to your scraper's service account
(or install the optional `../prometheus` ServiceMonitor component from
the kustomize bundle) and the same path serves as a scrape target.

**Zabbix-side status.** `ZabbixProxyGroup.status` surfaces what the
Zabbix server itself reports: `groupState` (the official proxy group
state: unknown/offline/recovering/online/degrading) and `onlineReplicas`
— the count of this instance's member proxies whose last contact with
the server (`proxy.lastaccess`) is inside the group's failover window,
the official per-member online criterion. `Zabbix.status` reports
server HA state and component readiness through conditions.

**Cluster metrics into Zabbix.** Monitoring of the Kubernetes cluster
itself uses Zabbix's bundled "Kubernetes Cluster by HTTP" template
(8.0; the older per-component Kubernetes templates are deprecated).
The operator automates the whole path behind an optional spec knob —
set `spec.clusterMetrics` and it deploys a managed kube-state-metrics
endpoint, creates a scoped monitor service account plus a long-lived
token Secret, provisions the cluster-scoped read RBAC, and registers
the Zabbix host with the template linked and `{$KUBE.API.TOKEN}` +
`{$HTTP.TLS.VERIFY}` set — verified live in the kind lab (cluster
CPU/memory utilization, pod/node/namespace/deployment metrics, and
LLD node hosts flowing; deletion of the resource removes the host and
RBAC through the `cluster-metrics-cleanup` finalizer):

```yaml
spec:
  api:                       # required: the host is managed via the API
    tokenSecretRef:
      name: zbx-api-token
      key: token
  clusterMetrics:
    clusterName: kind-operator-lab   # the Zabbix host name
    tlsVerify: none                  # full needs the cluster CA in the server
    # namespace: observability       # default: the Zabbix resource's namespace
    # namePrefix: my-cluster         # default: the Zabbix resource's name
    # kubeStateMetrics:              # optional image/resources override
    #   image:
    #     tag: v2.13.0
```

When `kubeStateMetrics` is omitted the template discovers an existing
kube-state-metrics endpoint; in both cases the naming constraints from
the template's discovery regex hold: the Endpoints name must match
`<prefix>kube-state-metrics` (a service named exactly
`kube-state-metrics` is not matched) and the port must be named
`http`/`https`/`https-main`. The derived objects land in the
resource's own namespace under the resource's name as prefix
(`<name>-kube-state-metrics`, `<name>-kube-monitor`,
`<name>-kube-monitor-token`); `namespace` and `namePrefix` override
the placement — objects placed outside the resource's namespace carry
no owner references (Kubernetes rejects cross-namespace owners), so
their lifecycle rides on the disable path and the deletion finalizer
instead of garbage collection. Placement is recorded on the status at
enablement and pinned until the feature is disabled: changing the
overrides mid-flight is ignored (disable, then re-enable to move).
Removing `spec.clusterMetrics` entirely disables the feature: the
operator deletes the derived objects and the cluster-scoped RBAC
itself (garbage collection cannot fire when only the knob is removed)
and releases the `cluster-metrics-cleanup` finalizer. The managed
role is deliberately minimal —
no Secret/configmap reads — so KSM's log may show `forbidden`
warnings for collectors outside the grant; the template's metrics do
not need them. `tlsVerify: full` requires the cluster CA to be trusted
by the Zabbix server container. The host name is the ownership key:
the operator adopts (and realigns) an existing host with the same
name; on resource deletion the host and the cluster-scoped RBAC are
removed. Right after first enablement the utilisation items can sit
in `unsupported` state for a few minutes — the template's
"Failed to get Nodes" error fires while kube-state-metrics' informers
warm up and clears itself; `kube.namespace.cpu_usage[*]` needs a few
minutes of history for its `last()` expressions.

## Testing

**Unit/integration:** `make test` — envtest-backed controller tests
plus builder unit tests (`go test ./...`).

**End-to-end:** `make test-e2e` — Ginkgo suite
([test/e2e](test/e2e)) that runs against a *fresh dedicated kind
cluster* (`zabbix-k8s-operator-test-e2e`, deleted and recreated each
run; never your dev cluster). It builds and loads the manager image,
deploys CRDs + operator, installs CloudNativePG, bootstraps a
database cluster, declares a `Zabbix` instance, and asserts the real
outcomes: the whole stack rolls out with `DatabaseReady`/`Ready`
conditions and accurate `observedGeneration`, every owned workload
carries a `maxUnavailable=1` disruption budget that admits
disruptions, the server wires the database through the CNPG app
secret, and the metrics endpoint enforces its authz contract
(unauthorized token → 403, `metrics-reader`-bound token → 200).
A failed run leaves the cluster for debugging; passing runs clean up
automatically. Duration ≈ 3 minutes.

## Installation

Prerequisite: [CloudNativePG](https://cloudnative-pg.io/) manages the
database the operator wires in (it is intentionally not managed by the
operator):

```sh
kubectl apply --server-side -f \
  https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v1.30.1/cnpg-1.30.1.yaml
```

Install the operator (CRDs, RBAC, manager) from the committed bundle:

```sh
kubectl apply -f \
  https://raw.githubusercontent.com/venkain/zabbix-k8s-operator/v0.1.0/dist/install.yaml
```

Or from the Helm chart (`dist/chart`; the image defaults to the
scaffold placeholder — set `manager.image.repository`/`tag` to the
published image):

```sh
helm install zabbix-k8s-operator dist/chart \
  --namespace zabbix-k8s-operator-system --create-namespace \
  --set manager.image.repository=<registry>/zabbix-k8s-operator \
  --set manager.image.tag=<tag>
```

Then bootstrap the database and declare the stack — full examples in
[config/samples](config/samples):

```sh
kubectl create ns monitoring
kubectl -n monitoring apply -f - <<'EOF'
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: zabbix-db
spec:
  instances: 1
  bootstrap:
    initdb:
      database: zabbix
      owner: zabbix
  storage:
    size: 20Gi
EOF
kubectl -n monitoring apply -f \
  https://raw.githubusercontent.com/venkain/zabbix-k8s-operator/v0.1.0/config/samples/zabbix_v1alpha1_zabbix.yaml
kubectl -n monitoring get zabbix --watch
```

For development and iteration, deploy from source with
`make deploy IMG=<registry>/<project>:<tag>` instead.




## Roadmap

- [x] **Phase 0 — requirements & research** (research material maintained separately)
- [x] **Phase 1 — scaffold**: kubebuilder project, CRD design, core reconcile loops
- [x] **Phase 2 — database**: CloudNativePG integration, external-DB mode
- [x] **Phase 3 — production hardening**: HA topologies, probes, PDBs, metrics, e2e suite on kind
- [x] **Phase 4 — publish**: GitHub release, install bundle, docs
  - [x] Install bundle (dist/install.yaml) + v0.1.0 release
  - [x] Docs: README-first — this repo renders on GitHub, so the
    README + [docs/requirements.md](docs/requirements.md) are the
    documentation; no separate docs-site build to maintain

## Documentation

| Document | Contents |
|---|---|
| [docs/requirements.md](docs/requirements.md) | Goals, assumptions, decisions, component matrix, open questions |

## License

Apache License 2.0 — see [LICENSE](LICENSE).
