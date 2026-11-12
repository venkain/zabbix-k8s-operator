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
- ~~OQ5: Cluster metrics into Zabbix~~ — **resolved 2026-11-10
  (design + lab verification)**: the official "Kubernetes Cluster by
  HTTP" template (bundled with Zabbix 8.0; the older per-component
  Kubernetes templates are deprecated) against kube-state-metrics +
  the Kubernetes API is the supported path for monitoring the cluster
  the operator runs in. This slice verified the integration end-to-end
  on the kind lab (kube-state-metrics 2.9.2 deployed; Zabbix host
  created through the API with the template linked and
  `{$KUBE.API.TOKEN}` set; live cluster metrics flowing — cluster
  CPU/memory utilization, pod/namespace/node/deployment counts, LLD
  node host). Operator-side automation of the provisioning (deploy
  KSM, create the host, link the template, set macros) is a candidate
  later slice behind a spec knob; it is deliberately not built now
  (OQ3 policy: no speculative features). Lab findings that any future
  implementation must honor:
  - the template discovers the KSM endpoint by regex `(.+)kube-state-metrics$`
    against Endpoints object names — a service named exactly
    `kube-state-metrics` does NOT match; it needs a name prefix (the
    official Helm chart prefixes it the same way);
  - the Endpoints port must be named `http`/`https`/`https-main`;
  - `{$HTTP.TLS.VERIFY}` defaults to `full` — the cluster CA must be
    trusted by the Zabbix server container (lab used `none`);
  - the template's token needs read access plus `services/proxy` (KSM
    endpoint discovery + scrape goes through the API service proxy)
    and `nodes/proxy` (kubelet `/metrics/resource` via the API node
    proxy).
  - **2026-11-11 update: the automation slice shipped** — the knob is
    real: `spec.clusterMetrics` on the Zabbix CR (optional, off by
    default; requires `spec.api`). The operator deploys a managed
    kube-state-metrics Deployment/Service (`<instance>-kube-state-metrics`,
    port `http`), a monitor ServiceAccount + long-lived token Secret
    (`<instance>-kube-monitor(-token)`, legacy `kubernetes.io/service-account-token`
    type filled asynchronously by the token controller), a namespace-
    embedded shared ClusterRole/Binding (`<ns>-<instance>-kube-state-metrics`,
    binds BOTH identities), and registers/realigns the Zabbix host by
    NAME (template resolved by name; ids differ per Zabbix build).
    Deletion removes host + cluster-scoped RBAC via the
    `cluster-metrics-cleanup` finalizer (host cleanup is skipped, with
    a log, when the API token Secret is gone). Lab-verified on the
    kind lab including take-over of the manually provisioned setup:
    `ClusterMetricsReady=True/ClusterHostRegistered`, 271 items on the
    host, 0 unsupported, cluster utilisation + namespace CPU all
    fresh. Engineering notes honored en route:
    - Kubernetes privilege-escalation protection: the manager must
      hold a superset of every rule the generated ClusterRole grants —
      the first deploy was rejected by the API server until the
      manager role gained the same read rules (and the generated role
      was trimmed to what KSM + the template actually need; no
      secrets/configmaps reads — KSM then logs `forbidden` warnings
      for its non-granted collectors, which is expected and harmless);
    - kubebuilder RBAC markers: multiple resources inside one marker
      are `;`-separated (`resources=nodes;namespaces`) — commas break
      controller-gen parsing;
    - the template's "Failed to get Nodes" error is the JSONPath
      preprocessing step's CUSTOM ERROR message (`error_handler 3`,
      stored in `error_handler_params`), fired when the KSM scrape
      contains no `kube_node_info` samples yet — a warm-up transient
      that self-clears; `kube.namespace.cpu_usage[*]` likewise needs a
      few minutes of history. Not operator bugs, documented in README;
    - script items (type 21) catch their own errors and return
      `{"error": ...}` as the value — item-level errors on dependents
      come from preprocessing custom handlers, not the script's throw.
      Script/intermediate items show `lastclock 0` by design
      (`history=0`), so freshness checks must watch the leaf items.
    - deferred: regenerate `dist/install.yaml` (CRD changed) with the
      NEXT release tag — regenerating now would embed a dev image tag
      in the committed bundle.
- OQ6: Zabbix 8.0 support — what changes for the operator? — **reviewed
  2026-11-12, plan below**. Source: the official
  [What's new and upgrade notes: 8.0](https://www.zabbix.com/documentation/8.0/en/manual/whatsnew)
  (8.0.0 released 2026-11-05; the next minor, 8.2, is in devel — planned
  Sept 2027 — so 8.0 LTS is the only target for the foreseeable future).
  The operator's `spec.version` already accepts `8.0` (pattern
  `^[0-9]+\.[0-9]+$`) and the official container tags exist, so 8.0
  deploys work today; the review is about deltas that need operator
  code, docs, or user guidance.

  **Breaking changes that hit operator users (docs/upgrade guidance):**
  - Minimum DB versions raised — PostgreSQL 13 → **15**, MySQL 8.0.30 →
    8.4, MariaDB 10.5 → 10.11, Elasticsearch 7 → **8** (server refuses
    to start on older ES; `AllowUnsupportedDBVersions` cannot override
    the ES check). CNPG users: pin a CNPG image ≥ PG 15 (current CNPG
    defaults are ≥16, fine). Elasticsearch <8 anywhere is a hard stop.
  - Minimum PHP 8.2 (frontend) — irrelevant for official images.
  - **History storage provider rework**: `HistoryStorageURL/Types/
    DateIndex` are deprecated in favor of a single `HistoryProvider`;
    frontend `$HISTORY` → `$HISTORY_PROVIDERS`; ClickHouse joins
    Elasticsearch as a history/telemetry backend. Cannot mix old+new
    params. Operator angle: if/when history-backend passthrough is
    added (server config or env), it must emit the NEW param names for
    8.0+ and the OLD ones for 7.x — a version-gated config builder.
  - **API access now disabled by default for new user roles** (all
    roles on fresh installs; only new roles on upgraded ones), and an
    empty API allow-list now denies everything. Directly affects
    `spec.clusterMetrics` token provisioning: a user who creates a
    fresh dedicated role for the monitor token on 8.0 gets
    `ClusterHostRegistrationFailed` unless API access is explicitly
    enabled for that role. Document in the clusterMetrics README
    section (and consider surfacing the API error text in the
    condition message).
  - HTTP-based templates and webhook media types now verify TLS by
    default (`{$HTTP.TLS.VERIFY}`, default `full`). The Kubernetes
    Cluster template users still need the CA trusted in the server
    container or `none` — OQ5 lab findings already documented this.
  - Deprecated Kubernetes templates officially replaced by
    "Kubernetes Cluster by HTTP" — operator story unchanged (we link
    the new template already).

  **New features mapped against current operator support:**
  - APM / OpenTelemetry (traces/metrics/logs via OTLP; proxy gets an
    APM collector with gRPC listener + TLS params; server gains
    `TelemetryProvider`, `StartTelemetryQueryPollers`; telemetry data
    lives in ClickHouse) — NOT supported by the operator yet. Plan:
    second wave, gated on demand. Needs (a) passthrough for the new
    server params (OQ3 policy: only when demonstrated), (b) proxy APM
    support: APM-enabled proxy image + Service exposing the OTLP
    listener port + TLS cert wiring, (c) optionally a managed
    ClickHouse if users want in-cluster telemetry storage (big scope —
    likely never managed; external endpoint passthrough instead).
  - CEP (complex event processing) — API-surface feature
    (`ceprule.*`); the operator manages infrastructure, not rules. No
    work.
  - Multiple PostgreSQL hosts in `DBHost` (comma list, connection
    failover) — CNPG already gives failover via the `-rw` Service; for
    `database.external`, a future nicety would be accepting a host
    list. Low priority.
  - Mobile app / bridge adapter (`BridgeAdapterURL`,
    `EnableMobileDevices`), MCP server, dark theme, dashboards,
    widgets, SAML cert import, vault AppRole, DNS caching (c-ares) —
    all inside official images or API/frontend; covered by existing
    `env` escape hatches or nothing to do.
  - New templates (AWS/Azure/GCP additions, MariaDB/Percona, OTLP
    templates, Podman, Hyper-V) — content-level; no operator work.

  **Operator support plan (ordered):**
  1. Docs (no code): README "Upgrading the stack to 8.0" notes — DB
     minimums (PG 15+), API-role default-off for clusterMetrics
     tokens, history-provider param migration pointer.
  2. Version-gated server config builder IF history-backend or
     telemetry passthrough is added (OQ3 gate; not speculative).
  3. APM slice (separate OQ when asked for): proxy APM image flag +
     OTLP Service + TLS; server `TelemetryProvider`/
     `StartTelemetryQueryPollers` passthrough; ClickHouse as external
     data source.
  4. `database.external` host-list support (nice-to-have).
  5. 8.2 tracking note: it is in devel until ~Sept 2027; revisit the
     8.2 what's-new page (NetFlow, topology, log management, AI tools
     per early research) when it nears release.

## Decision log

- 2026-11-12: OQ6 reviewed against the official 8.0 what's-new/upgrade
  notes: no code changes required for 8.0 deploys today (version
  pattern + official tags already cover it); recorded the breaking
  changes that hit operator users (PG 15+ minimum, API-role
  default-off affecting clusterMetrics tokens, history-provider param
  rework) and a demand-gated plan for APM/OTLP + history-backend
  passthrough. Docs-only upgrade notes approved as the first slice.

- 2026-11-06: D1–D5 accepted; scope matrix drafted. Phase 0 research
  (deployment-tooling analysis and hands-on validation evidence) completed
  in a separate research repository.
- 2026-11-07: Reference policy recorded (official versioned Zabbix docs
  are the sole normative reference). OQ4 added (active-server routing).
- 2026-11-10: OQ1–OQ4 closed (see entries above). OQ1/OQ4 were decided
  by the shipped slice 2/3 implementations; OQ2/OQ3 decided explicitly
  (Service-type knob; curated surface + env escape hatch).
- 2026-11-10: OQ5 closed by design + lab verification — cluster
  metrics into Zabbix go through the official "Kubernetes Cluster by
  HTTP" template + kube-state-metrics; operator automation of the
  provisioning recorded as a candidate later slice, not built now.
- 2026-11-11: OQ5 automation slice built and lab-verified:
  `spec.clusterMetrics` (optional knob) automates kube-state-metrics +
  monitor identity + RBAC + Zabbix host registration, with a deletion
  finalizer for the server-side host and cluster-scoped RBAC. Full
  details in the OQ5 entry above. Deferred to the next release:
  regenerating `dist/install.yaml` (the CRD changed).
- 2026-11-11: OQ5 disable path added (e2e-driven find). Kubernetes
  garbage collection never fires when only the feature flag is
  removed — the objects' owner (the Zabbix resource) stays alive — so
  disabling `spec.clusterMetrics` would have orphaned the KSM
  Deployment/Service/SAs and the cluster-scoped ClusterRole forever.
  The operator now cleans up on disable itself
  (`disableClusterMetrics`): best-effort Zabbix host delete (the host
  name is remembered in the new `status.clusterMetricsHostName` since
  the spec field is gone; failure leaves an unmanaged host instead of
  wedging reconciliation), delete of all namespaced objects +
  ClusterRole/Binding, stale `ClusterMetricsReady` condition dropped,
  finalizer released. The deletion path (`finalizeClusterMetrics`)
  keeps its blocking semantics: an API failure there keeps the
  finalizer and requeues, because at delete time nothing can come
  back for the resource — silent leaks are worse than retries.
- 2026-11-11: OQ5 placement overrides (`spec.clusterMetrics.namespace`
  + `namePrefix`). Defaults stay the resource's own namespace and
  name. Placement is recorded on the status at enablement
  (`clusterMetricsNamespace`/`clusterMetricsNamePrefix`) and pinned
  until disable: mid-flight override changes are ignored (disable +
  re-enable to move) — otherwise a namespace change would orphan the
  old objects exactly like the disable bug did. Cross-namespace
  objects carry no owner references (k8s rejects cross-namespace
  owners): their cleanup rides entirely on the operator-driven
  disable path and the deletion finalizer. ClusterRole/Binding names
  embed the placement namespace, so two instances picking the same
  namespace do not fight over one role.
