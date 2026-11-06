# Requirements

Status: **accepted 2026-11-06** (decisions made with the project owner).
This document is the source of truth for v1 scope. Revisit at each phase
boundary; record changes in the [changelog of decisions](#decision-log).

## Goals

1. Validate that the community Helm chart
   (`zabbix-community/helm-zabbix`) works as advertised, and establish —
   with evidence — that a Kubernetes operator can supersede it for
   Kubernetes-native workflows.
2. Build and publish a production-ready Zabbix operator on GitHub.
3. Target all official Zabbix **8.0** functionality for the in-scope
   components; support **7.0 LTS** as an install-and-run target.

## Non-goals (v1)

- Cross-version upgrade orchestration (7.0 → 8.0 DB schema migrations).
- Managing Java Gateway, Agent DaemonSet mode, Browser Monitoring
  (WebDriver), MCP server, or Bridge adapter.
- MySQL/MariaDB backends (Zabbix supports them; PostgreSQL/TimescaleDB only
  here — same decision the community chart made).
- Multi-cluster management, backup orchestration beyond what CNPG already
  provides.

## Assumptions

- A1: The community chart is the de-facto "official" Kubernetes install
  method: Zabbix maintains no first-party chart (checked 2026-11-06; the
  chart README references a barebones `git.zabbix.com` chart for
  integration only). Validating the community chart therefore validates
  the de-facto standard.
- A2: Zabbix images are published per version/tag
  (`zabbix/zabbix-*:ubuntu-8.0.0`, `alpine-*` variants) and are the
  building blocks the operator composes — same images the chart uses.
- A3: Zabbix Server native HA (6.0+) is the production availability story;
  it requires all server replicas to share one PostgreSQL database.
- A4: PostgreSQL 15–18 (optionally TimescaleDB 2.20–2.29) per
  [Zabbix 8.0 requirements](https://www.zabbix.com/documentation/8.0/en/manual/installation/requirements).

## Decisions

| # | Decision | Choice | Rationale |
|---|---|---|---|
| D1 | Positioning vs existing operators | **Build our own**; cite `sagh0900/zabbix-operator` as prior art, benchmark against it and the chart | Learning + portfolio + full design control; the space has no dominant player |
| D2 | Database strategy | **CNPG integration first, external DB (existing Secret) second** | Chart's bundled PG is single-pod/unpersisted and incompatible with server HA; CNPG gives HA, backups, PG upgrades; external Secret keeps chart parity for BYO-DB users |
| D3 | v1 components | **Server (HA) + frontend + web service + proxy group** | The core monitoring story; everything else is additive later |
| D4 | Version support | **8.0 + 7.0 LTS install/run; upgrades later** | Cross-version DB migrations are the riskiest feature; defer until the core loop is proven |

## Component feature matrix (v1 target, Zabbix 8.0)

| Component | Official 8.0 | Chart 8.0.0 | Operator v1 |
|---|---|---|---|
| Server, native HA (N replicas, heartbeat failover) | ✅ | ✅ (+ sidecar auto-clean hack) | ✅ (designed node management) |
| Frontend (nginx) | ✅ | ✅ | ✅ |
| Web service (scheduled reports) | ✅ | ✅ | ✅ |
| Proxy (StatefulSet, multi-replica, SQLite3) | ✅ | ✅ | ✅ (proxy group abstraction) |
| Agent 2 (sidecar in server/proxy pods) | ✅ | ✅ | ✅ (sidecar only) |
| PostgreSQL | required | ✅ single-pod, no persistence default | ✅ via CNPG |
| External DB via Secret | — | ✅ | ✅ (D2, phase 3) |
| Java Gateway | ✅ | ✅ | later |
| Agent 2 DaemonSet (node monitoring) | ✅ | ✅ | later |
| Browser Monitoring (Selenium WebDriver) | ✅ new | ✅ | later |
| MCP server | ✅ new | ❌ | later |
| Bridge adapter | ✅ new | ❌ | later |
| 7.0 LTS | ✅ (until 2029) | ❌ (8.0 chart dropped 7.x) | ✅ install/run (D4) |

## Open questions

- OQ1: CRD shape — one `Zabbix` CR owning the whole stack, plus how proxy
  groups are expressed (sub-spec vs separate CR). Decide in Phase 2 after
  chart validation; `sagh0900`'s `ZabbixDatabase`/`ZabbixSystem` split is a
  data point.
- OQ2: Frontend exposure — Ingress/LoadBalancer/NodePort handled by the
  operator, or left to users (chart parity)?
- OQ3: How much of the `ZBX_*` surface to expose in the CR spec vs hidden
  behind sane production defaults (the chart exposes ~everything; an
  operator should curate).

## Decision log

- 2026-11-06: D1–D4 accepted; scope matrix drafted; Phase 1 (chart
  validation on kind) approved as next step.
