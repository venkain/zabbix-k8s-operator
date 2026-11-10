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


## Roadmap

- [x] **Phase 0 — requirements & research** (research material maintained separately)
- [x] **Phase 1 — scaffold**: kubebuilder project, CRD design, core reconcile loops
- [x] **Phase 2 — database**: CloudNativePG integration, external-DB mode
- [ ] **Phase 3 — production hardening**: HA topologies, probes, PDBs, metrics, e2e suite on kind
  - [x] HA topologies, probes, PDBs, metrics (exposure + status surfacing)
  - [ ] e2e suite on kind
- [ ] **Phase 4 — publish**: GitHub release, install bundle, docs site

## Documentation

| Document | Contents |
|---|---|
| [docs/requirements.md](docs/requirements.md) | Goals, assumptions, decisions, component matrix, open questions |

## License

Apache License 2.0 — see [LICENSE](LICENSE).
