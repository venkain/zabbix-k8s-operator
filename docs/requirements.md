# Requirements

Status: **accepted 2026-11-06** (decisions made with the project owner).
This document is the source of truth for v1 scope. Revisit at each phase
boundary; record changes in the [decision log](#decision-log).

This repository intentionally does not reference other Zabbix deployment
tooling. Research and validation material that informed these decisions is
maintained separately.

## Reference policy

The normative reference for all Zabbix behavior, configuration, defaults,
and requirements is the **official Zabbix documentation for the targeted
version**:

- Zabbix 8.0 (primary): <https://www.zabbix.com/documentation/8.0/en/manual>
- Zabbix 7.0 LTS (run target): <https://www.zabbix.com/documentation/7.0/en/manual>

Component environment-variable specifics follow the official container
documentation under the same versioned manual
(`.../manual/installation/containers`). Community charts, forum threads,
and third-party tutorials are never normative and are not cited here.

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

- ~~OQ1: CRD shape~~ — **resolved 2026-11-09**: one `Zabbix` CR owns the
  whole stack; proxy groups are a separate `ZabbixProxyGroup` CR that
  references the server resource (`spec.serverRef`) — the group has its
  own lifecycle (member scaling, server-side group maintenance,
  deletion finalizer) and need not require a `Zabbix` instance to exist.
- ~~OQ2: Frontend exposure~~ — **resolved 2026-11-10**: the operator
  exposes a Service-type knob (`frontend.service.type`, ClusterIP
  default; NodePort/LoadBalancer opt-in, plus Service annotations).
  Ingress routing on top stays user-managed — the operator owns the
  Service, not the traffic stack above it.
- ~~OQ3: `ZBX_*` surface~~ — **resolved 2026-11-10**: the CR exposes a
  curated production surface (replicas, images, resources, HA wiring,
  proxy-group settings) behind sane defaults derived from the official
  component documentation; `frontend.env` is the escape hatch for
  frontend-only PHP settings. Deeper config passthrough is added only
  when a real need is demonstrated, never speculatively.
- ~~OQ4: Active-server routing~~ — **resolved 2026-11-08**: a
  controller-managed `<instance>-server-active` Service whose selector
  tracks the HA role label, maintained per pod from the official
  `hanode.get` registry through the API. The frontend and proxy group
  members are wired to it.

## Decision log

- 2026-11-06: D1–D5 accepted; scope matrix drafted. Phase 0 research
  (deployment-tooling analysis and hands-on validation evidence) completed
  in a separate research repository.
- 2026-11-07: Reference policy recorded (official versioned Zabbix docs
  are the sole normative reference). OQ4 added (active-server routing).
- 2026-11-10: OQ1–OQ4 closed (see entries above). OQ1/OQ4 were decided
  by the shipped slice 2/3 implementations; OQ2/OQ3 decided explicitly
  (Service-type knob; curated surface + env escape hatch).
