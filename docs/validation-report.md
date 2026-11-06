# Validation Report — zabbix-community/helm-zabbix

Hands-on validation of chart `8.0.0` (Zabbix `ubuntu-8.0.0` images) on the
local kind cluster `operator-lab` (Kubernetes 1.37, Helm 4.3.1, kind
v0.33.0). Purpose: evidence base for the operator's rationale — see
[research/helm-chart-analysis.md](research/helm-chart-analysis.md) for the
checklist this executes and [requirements.md](../requirements.md) for scope.

Method: each scenario installs/upgrades the chart exactly as a real user
would, then verifies (a) Kubernetes-level health (pods, PVCs, services) and
(b) the user-visible contract — Zabbix API login and frontend reachability —
via `POST /api_jsonrpc.php` through a port-forward.

---

## Scenario 1 — defaults, exactly as advertised

Command: `helm upgrade --install zabbix zabbix-community/zabbix
--dependency-update --create-namespace --version 8.0.0 -n monitoring`

Deployed topology (per values defaults):

| Component | Form | Notes |
|---|---|---|
| zabbix-zabbix-server | Deployment, **2/2 containers** | server + agent2 sidecar |
| zabbix-postgresql | StatefulSet, 1 replica | **no PVC** (persistence off) |
| zabbix-zabbix-web | Deployment, 1 replica | frontend |
| zabbix-zabbix-webservice | Deployment, 1 replica | reports |
| zabbixProxy / JavaGateway / BrowserMonitoring | not deployed | `enabled: false` defaults |

Results:

- All pods `Ready` in **66 s** from install (DB init + schema load included).
- `kubectl -n monitoring wait --for=condition=ready pod --all` — clean pass.
- API: `user.login` with default `Admin/zabbix` → **success** (session token
  returned). Frontend: HTTP **200**.

**Finding 1.1 (confirmed by design): the default install stores the entire
Zabbix database in a single unpersisted pod.** `kubectl get pvc -n monitoring`
is empty; deleting `zabbix-postgresql-0` (or its node dying) loses all hosts,
history, and configuration. Verified structurally (no PVC objects exist);
the destructive proof is deferred to Scenario 2's reschedule test on the
persisted variant, where the PVC must survive the same event.

Implication for the operator: the out-of-the-box path is a **demo
install**. Production users must already know to (a) enable persistence,
then (b) discover that HA is impossible with the bundled DB — two
non-obvious steps the chart does not guide. The operator's default path
should be production-shaped instead (CNPG-backed).

---

## Scenario 2 — persistence ON: pending

## Scenario 3 — server HA + external DB Secret: pending

## Scenario 4 — proxy group, per-pod DNS: pending

## Scenario 5 — upgrade breakage (across chart versions / image tags): pending
