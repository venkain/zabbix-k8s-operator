# Requirements

Status: **accepted 2026-11-06** (decisions made with the project owner).
This document is the source of truth for v1 scope. Revisit at each phase
boundary; record changes in the [decision log](#decision-log).

This repository intentionally does not reference other Zabbix deployment
tooling. Research and validation material that informed these decisions is
maintained separately.

## Goals

1. A production-ready Kubernetes operator for Zabbix: declarative custom
   resources, level-triggered reconciliation, and a default path that is
   production-shaped (HA-capable, persistent, observable) rather than a
   demo topology.
2. Support all official Zabbix **8.0** functionality for the in-scope
   components.
3. Support Zabbix **7.0 LTS** as an install-and-run target.

## Non-goals (v1)

- Cross-version upgrade orchestration (7.0 → 8.0 DB schema migrations).
- Managing Java Gateway, Agent DaemonSet mode, Browser Monitoring
  (WebDriver), MCP server, or Bridge adapter.
- MySQL/MariaDB backends (PostgreSQL/TimescaleDB only).
- Multi-cluster management, backup orchestration beyond what CNPG already
  provides.

## Assumptions

- A1: Zabbix publishes versioned container images for every component
  (`zabbix/zabbix-*`, `ubuntu-*`/`alpine-*` tags) and those images are the
  building blocks the operator composes.
- A2: Zabbix Server native HA (6.0+) is the production availability story;
  all server replicas share one PostgreSQL database.
- A3: PostgreSQL 15–18 (optionally TimescaleDB 2.20–2.29) per
  [Zabbix 8.0 requirements](https://www.zabbix.com/documentation/8.0/en/manual/installation/requirements).

## Decisions

| # | Decision | Choice | Rationale |
|---|---|---|---|
| D1 | Implementation | Independent implementation from first principles | Full design ownership; the operator pattern is applied directly rather than derived from existing tooling |
| D2 | Database strategy | **CloudNativePG integration first; external DB (existing Secret) second** | A bundled single-instance database cannot provide HA or durable persistence by default; CNPG gives HA, backups, and PostgreSQL upgrades; external Secret support covers BYO-DB users |
| D3 | v1 components | **Server (HA) + frontend + web service + proxy group** | The core monitoring story; everything else is additive later |
| D4 | Version support | **8.0 + 7.0 LTS install/run; upgrades later** | Cross-version DB migrations are the riskiest feature; defer until the core reconcile loop is proven |
| D5 | References policy | This repository references no other Zabbix deployment tooling | Keeps the project self-contained; research material lives outside |

## Component feature matrix (v1 target, Zabbix 8.0)

| Component | Official 8.0 | Operator v1 |
|---|---|---|
| Server, native HA (N replicas, heartbeat failover) | ✅ | ✅ |
| Frontend (nginx) | ✅ | ✅ |
| Web service (scheduled reports) | ✅ | ✅ |
| Proxy (StatefulSet, multi-replica, SQLite3) | ✅ | ✅ (proxy group abstraction) |
| Agent 2 (sidecar in server/proxy pods) | ✅ | ✅ (sidecar only) |
| PostgreSQL | required | ✅ via CNPG (D2) |
| External DB via Secret | — | ✅ (D2, second step) |
| Java Gateway | ✅ | later |
| Agent 2 DaemonSet (node monitoring) | ✅ | later |
| Browser Monitoring (Selenium WebDriver) | ✅ new | later |
| MCP server | ✅ new | later |
| Bridge adapter | ✅ new | later |
| 7.0 LTS | ✅ (until 2029) | ✅ install/run (D4) |

## Open questions

- OQ1: CRD shape — one `Zabbix` CR owning the whole stack, plus how proxy
  groups are expressed (sub-spec vs separate CR). Decide during Phase 1.
- OQ2: Frontend exposure — Ingress/LoadBalancer/NodePort handled by the
  operator, or left to users?
- OQ3: How much of the `ZBX_*` configuration surface to expose in the CR
  spec vs hidden behind sane production defaults.

## Decision log

- 2026-11-06: D1–D5 accepted; scope matrix drafted. Phase 0 research
  (deployment-tooling analysis and hands-on validation evidence) completed
  in a separate research repository.
