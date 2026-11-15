# Design: AI agent integration (Zabbix MCP server)

Status: proposal — not yet implemented. This is the third confirmed
post-v0.4.0 TODO (after the security audit and the streamlining pass).

## What the MCP server is

`zabbix/zabbix-mcp-server` (new in Zabbix 8.0) exposes the Model
Context Protocol over Streamable HTTP at `/mcp`: AI agents can read
hosts/triggers/problems/item history and act on incidents (acknowledge,
suppress, close, maintenance periods). Facts that shape the operator
design:

- **Token passthrough, no stored credentials.** The server keeps no
  credentials: every MCP request must carry the caller's Zabbix API
  token as `Authorization: Bearer <token>`. Authorization and audit
  trail follow the token's user. The operator therefore manages NO new
  Secret for this component — unlike the web-service/API wiring, there
  is nothing to provision or rotate.
- **Stateless.** No session affinity; a single-replica Deployment is
  sufficient and safe behind any Service.
- **Image facts:** numeric `USER 1997` (safe for the full restricted
  container context — no `runAsUser` guessing needed, but pin it
  explicitly anyway), listens on `8443` by default, small (~9 MB
  compressed alpine). Tags follow the family convention:
  `ubuntu-8.0-latest`, pinned via the instance's `spec.version`.
- **Configuration is env/config-file driven:** the container entrypoint
  maps `ZBX_FRONTEND_URL` (must point at `/api_jsonrpc.php`) and
  `ZBX_ALLOWEDIP`; the rest (tool allow/deny rules, `ToolPrefix`,
  `MaxResponseSize`, `Timeout`, TLS, debug) lives in
  `/etc/zabbix/zabbix_mcp_server.conf` with deny-by-default for the
  sensitive tools.

## CRD surface

Optional `spec.agentIntegration.mcp` block (follows the web-service
pattern):

```yaml
spec:
  agentIntegration:
    mcp:
      enabled: true
      # all fields below optional
      image:            # ImageSpec override (repo/tag/pullPolicy)
      resources:        # container resources
      allowedIPs: []    # AllowedIP; default: deny-external
      denyTools:  []    # DenyTool rules on top of the image defaults
      allowTools: []    # AllowTool overrides
      toolPrefix: ""    # optional ToolPrefix
      service:          # optional exposure, exactly like frontend:
        type:           # ClusterIP (default) / NodePort / LoadBalancer
        annotations: {}
```

Validation (programmatic webhook or CRD markers): `enabled` implies
`spec.api` must be set? — **no**: the MCP server does not use the
operator's API token; it needs only the frontend URL. It should be
allowed without `spec.api`. Version gate: Zabbix >= 8.0
(`SupportsTelemetry`-style helper, e.g. `SupportsAgentIntegration`).

## Objects the operator converges

| Object | Name | Notes |
| --- | --- | --- |
| Deployment | `<instance>-mcp-server` | 1 replica; restricted container context (`USER 1997`, seccomp, drop ALL, no privilege escalation); probes: the transport answers `401` without a token — use a TCP probe on 8443 |
| Service | `<instance>-mcp-server` | ClusterIP default; port name `mcp` on 8443 |
| NetworkPolicy | reuse manager's policy pattern | default-deny egress except DNS + the frontend Service (same exposure shape as finding #4's fix) |

**Wiring:** `ZBX_FRONTEND_URL=http://<instance>-frontend.<ns>.svc:80/api_jsonrpc.php`
— the same always-own-frontend pattern as the API client and
`WebServiceURL` (documented plaintext finding #5 applies; the frontend
Service is instance-internal).

**Status:** `AgentIntegrationReady` condition (Deployment-ready +
frontend Service resolvable). Teardown mirrors the web-service
disable/cleanup path (owner references handle the namespaced objects;
no cluster-scoped objects involved — simpler than clusterMetrics).

## Security posture

- AuthN/authZ: entirely delegated to the Zabbix API token the client
  presents. The operator's own token is never exposed to this
  component.
- Default exposure ClusterIP: agent access requires in-cluster
  connectivity; wide exposure is an explicit user opt-in (NodePort/LB),
  same stance as the frontend.
- `allowedIPs` maps to `AllowedIP` (default config denies unspecified
  clients with 403) — recommend users set it whenever exposure leaves
  the cluster.
- Tool deny-list defaults ship with the image (sensitive tools denied);
  the CR only ever ADDs restrictions by default — explicit
  `allowTools` widens them deliberately.

## e2e coverage plan

1. Enable → Deployment/Service roll out, condition true, tools/list
   over the in-cluster endpoint with a dummy token → expect 401
   (parked-state assertion, matching the clusterMetrics dummy-token
   pattern; real-token e2e stays with the backlog item).
2. Disable → objects gone, condition dropped, stack stays Ready.

## Non-goals

- No client-side integration (agents are the user's concern).
- No per-user token management by the operator.
- 7.0 LTS: no MCP server exists there — feature is 8.0+ only.
