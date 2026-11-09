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

## Roadmap

- [x] **Phase 0 — requirements & research** (research material maintained separately)
- [x] **Phase 1 — scaffold**: kubebuilder project, CRD design, core reconcile loops
- [x] **Phase 2 — database**: CloudNativePG integration, external-DB mode
- [ ] **Phase 3 — production hardening**: HA topologies, probes, PDBs, metrics, e2e suite on kind
- [ ] **Phase 4 — publish**: GitHub release, install bundle, docs site

## Documentation

| Document | Contents |
|---|---|
| [docs/requirements.md](docs/requirements.md) | Goals, assumptions, decisions, component matrix, open questions |

## License

Apache License 2.0 — see [LICENSE](LICENSE).
