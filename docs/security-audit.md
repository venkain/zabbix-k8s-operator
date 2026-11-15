# Security audit — 2026-11-13 (post v0.4.0)

Scope: the operator itself (`cmd/`, `internal/`, `api/`), the RBAC it
ships, the manifests it deploys, and its supply chain. Method: manual
review of privilege surfaces (RBAC, secrets, network, workload security
contexts, the Zabbix API client, CRD validation), cross-checked against
code usage. Envtest does not enforce RBAC, so privilege findings were
verified against code paths, not test behavior.

## Summary

| # | Finding | Severity | Status |
|---|---------|----------|--------|
| 1 | Manager role holds kube-state-metrics' privilege superset (`nodes/proxy`, `services/proxy`, PV/namespace/daemonset/jobs/HPA reads) | Accepted (by design, documented) | — |
| 2 | Secrets access is cluster-wide (`get/list/watch/create/update/patch/delete`) | Accepted (feature-inherent) | — |
| 3 | Operator-deployed workloads ship no explicit securityContext | Medium | **Fixed** |
| 4 | Deployed workloads have no NetworkPolicy | Low | Recommended |
| 5 | Operator → frontend API runs plaintext HTTP (API token in transit) | Low / Info | Documented |
| 6 | Unused Secret `update`/`patch` verbs | Low | Recommended |

## Detail

### 1. Manager role superset — accepted, documented

`config/rbac/role.yaml` carries kube-state-metrics' discovery rule set:
`nodes/proxy` + `services/proxy` (`create`, `get`), plus read-only
`nodes`, `namespaces`, PVs, PVCs, endpoints, daemonsets, replicasets,
jobs, cronjobs and HPAs. None of these are called by the manager code.

This is deliberate: the manager creates **and binds** the runtime KSM
ClusterRole (`ClusterMetricsClusterRole`), and Kubernetes
privilege-escalation protection requires the binding creator to hold a
superset of the bound permissions. The marker comment in
`internal/controller/zabbix_controller.go` documents this. Only the KSM
pod and the monitor identity actually exercise the rules.

Future hardening options (not done): ship the KSM ClusterRole +
binding statically in the bundle/chart so a cluster admin bootstraps
the privileged ruleset and the operator identity never needs
`nodes/proxy`. Deferred — it moves failure from install time into the
operator's absence and complicates the disable path.

### 2. Cluster-wide secrets — accepted, feature-inherent

The manager reads user-provided Secrets (database credentials, Zabbix
API token) and creates its own (monitor token, ClickHouse auth). The
cluster-metrics placement override allows an arbitrary namespace, so
the read scope cannot be namespaced below "the resource's namespace +
any override". No Secret listing is performed by operator code
(list/watch exist for the informer cache only).

Recommended hygiene: keep `spec.clusterMetrics.namespace` values under
operator control in multi-tenant clusters; a future admission
validation could restrict override namespaces.

### 3. Workload securityContexts — fixed

Every pod the operator deploys (server, frontend, web service, KSM,
ClickHouse) previously ran with no explicit securityContext: no
seccompProfile, no `allowPrivilegeEscalation: false`, no capability
drops, no `runAsNonRoot`. Hardening applied where known-safe:

- **Pod level, all deployed workloads:** `seccompProfile:
  RuntimeDefault`.
- **KSM:** full restricted set — `runAsNonRoot: true`,
  `runAsUser: 65534`, `allowPrivilegeEscalation: false`,
  `capabilities.drop: [ALL]` (upstream-supported). The numeric uid is
  required: the `kube-state-metrics:v2.9.2` image declares a
  non-numeric USER (`nobody`), so kubelet cannot verify `runAsNonRoot`
  from the image alone and the container fails to start without an
  explicit uid (caught and verified by the e2e suite).
- Zabbix/ClickHouse images keep image-default users; per-container
  hardening is evaluated per component before adding (forcing
  `runAsNonRoot` on untested images risks breaking the stack; the e2e
  suite is the gate for any further tightening).

### 4. NetworkPolicy for deployed workloads — recommended

The manager ships a metrics-only NetworkPolicy for itself. Deployed
workloads have unrestricted network. Notably KSM is a read-only
cluster observer: a default-deny egress policy scoped to the KSM pod
(except DNS + the Zabbix server scrape path) would shrink its exposure.
Recommended follow-up.

### 5. Plaintext operator → frontend API — documented

`APIEndpoint` always targets the instance's own frontend Service
(`http://<frontend>.<ns>.svc:80/api_jsonrpc.php`) — not a
user-controlled URL, so there is no SSRF surface. The Zabbix API token
traverses in-cluster plaintext; inside the CNI fabric this is the
standard trade-off for in-cluster control planes. A TLS frontend
endpoint or mesh mTLS would close it; tracked as future work.

### 6. Unused Secret verbs — recommended

`update`/`patch` on Secrets are not exercised by operator code
(creation + reads + delete on disable). Candidate for verb pruning in a
future RBAC pass — regenerating `role.yaml` must be followed by the
full e2e suite, since envtest does not enforce RBAC.

## Verified-clean surfaces

- **Zabbix API client** (`internal/zabbix/api.go`): no
  `InsecureSkipVerify` anywhere; the token travels only in the
  `Authorization` header; JSON-RPC bodies are struct-encoded; response
  decoding is size-unbounded but only from the instance's own
  frontend Service.
- **Credentials:** the ClickHouse auth password is `crypto/rand`
  (16 bytes, hex-encoded), never logged, never rotated behind the
  provisioned user.
- **Metrics endpoint:** HTTPS by default with authn/authz
  (`metrics-secure: true`), e2e-verified both directions (unauthorized
  token rejected, `metrics-reader` accepted).
- **Manager pod:** nonroot, RuntimeDefault seccomp, read-only root
  filesystem, all capabilities dropped, privilege escalation disabled,
  distroless base image.
- **No admission webhooks** are shipped or enabled.
- **CRD validation:** name/prefix fields carry DNS-label patterns with
  length caps before they flow into object names, ClusterRole names or
  Zabbix API payloads; endpoint URLs are pattern-constrained; enums
  gate value-type and TLS-verify fields.
- **Token hygiene:** no token values in logs or events (only
  secret names); error messages reference the Secret, not its content.
