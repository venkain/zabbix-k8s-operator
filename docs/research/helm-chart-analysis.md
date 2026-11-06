# Analysis: zabbix-community/helm-zabbix

Analyzed: 2026-11-06, chart `zabbix` v8.0.0 (`ubuntu-8.0.0` images default),
README + values reference. Purpose: establish what the chart actually does
and where an operator adds structural value. This feeds Phase 1 (hands-on
validation on kind), which will confirm or correct these findings.

## What the chart deploys

Components (all toggleable via values):

- **Zabbix Server** — Deployment; replicas > 1 ⇒ native Zabbix HA
  (heartbeat in DB, automatic failover). An `haNodesAutoClean` sidecar/job
  prunes stale HA node registrations.
- **Zabbix Agent 2** — default: sidecar in server and proxy pods; optional
  DaemonSet with `hostNetwork`.
- **Zabbix Web (frontend)** — nginx-based `zabbix/zabbix-web-nginx-*` image.
- **Zabbix Web Service** — for scheduled PDF reports.
- **Zabbix Proxy** — StatefulSet, SQLite3 backend, multi-replica, headless
  Service for per-pod DNS.
- **PostgreSQL** — bundled single-instance StatefulSet
  (`postgres:18` default; TimescaleDB image optional), **persistence OFF by
  default**, no HA. Or: external DB via `postgresAccess.existingSecretName`
  (README explicitly names CNPG/PGO as secret sources).
- **Java Gateway**, **Browser Monitoring** (Selenium WebDriver),
  **Ingress**, RBAC, ServiceAccount, helm test jobs — optional extras.

## Key defaults (values.yaml)

| Value | Default | Implication |
|---|---|---|
| `postgresql.enabled` | `true` | Single-pod DB, **no persistence** — data loss on pod reschedule unless persistence is enabled explicitly |
| `postgresql.persistence.enabled` | `false` | See above |
| `zabbixServer.zabbixServerHA` | `false` | HA off unless replicas raised / flag set; **cannot combine internal PG with HA** |
| `zabbixImageTag` | `ubuntu-8.0.0` | Only 8.0 supported/tested by chart 8.0.0 |
| `postgresAccess.*` | flat values or existing Secret | Central DB-credential Secret shared by all components |

## Structural gaps an operator can close

1. **No real database story.** Single pod, ephemeral by default, and
   explicitly incompatible with server HA (`postgresql.enabled=true` is
   "not usable in combination with `zabbixServer.zabbixServerHA=true`").
   Production users must bring their own DB with zero scaffolding. → CNPG
   integration (D2).
2. **Upgrade path is manual and lossy.** Chart 8.0.0 dropped 7.x support
   entirely; its own README documents StatefulSet immutability breakage on
   upgrade requiring manual `kubectl delete statefulset --cascade=orphan`
   surgery. DB schema migrations are unmanaged ("back up your database").
   → Reconciliation with deliberate handling of immutable fields (D4 defers
   cross-version migrations; lifecycle management still helps).
3. **Configuration is env-var soup.** ~70 `ZBX_*` variables across ~900
   lines of values; no validation, no drift correction, no status. →
   Curated CR spec + conditions (OQ3).
4. **No level-triggered repair.** Helm is imperative at run time: drift
   (deleted ConfigMap, scaled pod) persists until the next
   `helm upgrade`. → Controller watches owned resources.
5. **HA node hygiene is a sidecar hack.** `haNodesAutoClean` runs in-pod to
   delete stale HA registrations from the DB. → Operator-managed node set.

## What the chart gets right (parity obligations)

- Component toggle matrix incl. optional extras (later versions for us).
- Central DB-credential Secret consumed by every component — our external-DB
  mode should consume the same shape of Secret (`host/port/user/password/
  dbname` keys).
- Headless Service per StatefulSet with stable pod DNS (fixed in chart
  8.0.0) — we already know this pattern from the curriculum.
- Helm test jobs (connection checks to server/web) — operator e2e should
  test the same user-visible contract.

## Validation checklist for Phase 1 (kind)

To be executed and recorded in `docs/validation-report.md`:

- [ ] Install chart 8.0.0 on kind with defaults; record every pod's fate.
- [ ] Repeat with persistence ON; verify PVC + survival of pod reschedule.
- [ ] Enable `zabbixServerHA` with external Secret; verify HA failover
      (kill active node, watch standby take over).
- [ ] Deploy proxy (multi-replica); verify per-pod DNS + registration.
- [ ] Attempt `helm upgrade` across chart versions; document breakage.
- [ ] Attempt Zabbix version change via `zabbixImageTag`; document result
      (expect: works within a major, untested across majors).
- [ ] Login to frontend; create a host; confirm the stack actually works
      end-to-end, not just "pods are Running".
