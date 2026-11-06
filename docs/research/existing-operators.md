# Prior art: existing Zabbix Kubernetes operators

Surveyed: 2026-11-06 (GitHub search: "zabbix operator", by stars/last push).

## Landscape

| Project | Stars | Last push | State |
|---|---|---|---|
| `sagh0900/zabbix-operator` | 1 | 2026-10-31 | **Active, serious** — the only living implementation |
| `vpdevx/zabbix-operator` | 1 | 2025-02-17 | Stale |
| `Aluanan/Zabbix-Operator` | 1 | 2021-08-04 | Dead |
| `zabbix-community/helm-zabbix` (chart, not operator) | — | active | De-facto standard install method |

Conclusion: **the space has no dominant player**; the chart is the thing to
supersede. An operator's viability is proven (below), adoption is not.

## sagh0900/zabbix-operator (main prior art)

What it does (from its README):

- Two CRDs: `ZabbixDatabase` (a CloudNativePG cluster) and `ZabbixSystem`
  (the Zabbix stack: HA servers, frontend, web service, proxies, agents).
- Supports Zabbix 7.0 / 7.2 / 7.4 / 8.0, **including patch upgrades and
  cross-line upgrades** (7.0 → 7.4 → 8.0) with DB migration handling.
- envtest + e2e-on-kind suites, runbook for its own alerts, migration guide
  from Helm/plain manifests, single published image (`ghcr.io`).
- Apache-2.0.

How to relate to it:

- **Benchmark, don't copy.** Its existence validates feasibility (it took
  over a running 7.4.7/12 GB DB and upgraded to 8.0.0rc1). Our project
  cites it as prior art and differentiates on design, docs, and release
  engineering.
- Steal-worthy questions to answer in our design: the
  `ZabbixDatabase`/`ZabbixSystem` CRD split; how it sequences DB readiness
  → schema init → server start; how it handles immutable StatefulSet
  fields on upgrade; its alert runbook as an observability model.

## What we differentiate on (working list)

1. **Documentation-first**: requirements/decisions tracked in-repo with
   rationale (this repo's docs), plus a phase-by-phase public roadmap.
2. **Chart-validation evidence base**: we publish the hands-on validation
   report of the community chart that motivates the operator — nobody else
   has documented this comparison.
3. **CRD design driven by the official component matrix** (see
   [requirements.md](../requirements.md)) rather than env-var parity.
4. Release engineering from day one: conventional commits, atomic history,
   install bundle, later a Helm chart for the operator itself.
