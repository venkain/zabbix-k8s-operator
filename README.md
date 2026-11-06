# Zabbix Kubernetes Operator

A Kubernetes operator that deploys and manages production-grade
[Zabbix](https://www.zabbix.com) installations — server with native high
availability, frontend, web service, and proxies — on a PostgreSQL database
managed by [CloudNativePG](https://cloudnative-pg.io) or provided externally.

**Status: pre-scaffold (design & validation phase).** No operator code exists
yet; see [Roadmap](#roadmap).

## Why

The [community Helm chart](https://github.com/zabbix-community/helm-zabbix)
installs Zabbix on Kubernetes but has structural limits an operator can fix:

| Limitation of the Helm chart | Operator answer |
|---|---|
| Bundled PostgreSQL is a single pod, **no persistence by default**, and cannot be combined with server HA | CloudNativePG clusters (HA, backups, PG upgrades) or external DB via Secret |
| Version upgrades are manual; the chart drops non-current Zabbix majors (8.0 chart no longer supports 7.x) | Version is a spec field; reconciliation drives the deployment |
| Helm upgrades hit Kubernetes immutability rules (StatefulSet `serviceName`) and require manual `--cascade=orphan` surgery | Controller owns resources for the whole lifecycle and handles immutable-field changes deliberately |
| Flat `ZBX_*` env-var configuration, no drift correction | Declarative CR spec, level-triggered reconciliation, status conditions |

Full analysis: [docs/research/helm-chart-analysis.md](docs/research/helm-chart-analysis.md).

## Scope (v1)

Decisions locked 2026-11-06 — see
[docs/requirements.md](docs/requirements.md) for details and rationale.

- **Components**: Zabbix Server (native HA) + frontend + web service + proxy
  group. Java Gateway, Agent DaemonSet, Browser Monitoring and MCP server are
  later versions.
- **Database**: CloudNativePG integration first; external database via
  existing Secret second.
- **Versions**: Zabbix 8.0 (primary) and 7.0 LTS (install & run). Cross-version
  upgrade orchestration (e.g. 7.0 → 8.0 DB migrations) is out of scope for v1.
- **Environments**: kind for development/tests; production-ready by design
  (HA, probes, resource management, security contexts, RBAC least privilege).
- **Distribution**: plain-YAML install bundle (kubebuilder `dist/install.yaml`),
  Helm chart for the operator itself later.

## Roadmap

- [x] **Phase 0 — research**: chart analysis, landscape, requirements (this repo's docs)
- [ ] **Phase 1 — chart validation**: deploy `zabbix-community/helm-zabbix` 8.0.0 on kind, verify every component, document what works and what breaks — the evidence base for superseding it
- [ ] **Phase 2 — scaffold**: kubebuilder project, CRD design (`Zabbix` + supporting types), reconcile loops for the core stack
- [ ] **Phase 3 — database**: CloudNativePG integration, external-DB mode
- [ ] **Phase 4 — production hardening**: HA topologies, probes, PDBs, metrics, e2e suite on kind
- [ ] **Phase 5 — publish**: GitHub release, install bundle, docs site

## Documentation

| Document | Contents |
|---|---|
| [docs/requirements.md](docs/requirements.md) | Scope, assumptions, decisions, open questions |
| [docs/research/helm-chart-analysis.md](docs/research/helm-chart-analysis.md) | The community chart: components, defaults, gaps |
| [docs/research/existing-operators.md](docs/research/existing-operators.md) | Prior art: what exists, what we differentiate on |

## Related work

- [zabbix-community/helm-zabbix](https://github.com/zabbix-community/helm-zabbix) — the chart this project studies (and aims to supersede for Kubernetes-native workflows)
- [sagh0900/zabbix-operator](https://github.com/sagh0900/zabbix-operator) — prior art: an existing CNPG-based Zabbix operator (independent implementation; see [existing-operators.md](docs/research/existing-operators.md) for the comparison)

## License

Apache License 2.0 — see [LICENSE](LICENSE).
