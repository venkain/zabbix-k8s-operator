# Design: AI agent integration (Zabbix MCP server)

Status: implemented (see `spec.agentIntegration.mcp`; this document
records the verified facts and the decisions taken).

## What the MCP server is

`zabbix/zabbix-mcp-server` (new in Zabbix 8.0) exposes the Model
Context Protocol over Streamable HTTP at `/mcp`: AI agents can read
hosts/triggers/problems/item history and act on incidents (acknowledge,
suppress, close, maintenance periods). Facts verified against the
official documentation and the image's env_vars contract:

- **Token passthrough, no stored credentials.** The server keeps no
  credentials: every MCP request must carry the caller's Zabbix API
  token as `Authorization: Bearer <token>`. Authorization and audit
  trail follow the token's user. The operator therefore manages NO new
  Secret for this component — there is nothing to provision or rotate.
- **Stateless.** No session affinity; a single-replica Deployment is
  sufficient and safe behind any Service.
- **Image facts:** numeric `USER 1997` (full restricted container
  context with `RunAsUser: 1997`), listens on `8443` by default
  (`ZBX_LISTENPORT`, endpoint path `/mcp`), tags follow the family
  convention (`ubuntu-8.0-latest`, pinned via the instance's
  `spec.version`).
- **Configuration is fully env-driven on the official image:**
  `ZBX_FRONTEND_URL` (required), `ZBX_ALLOWEDIP` (default
  `127.0.0.1,::1`), `ZBX_TOOL_PREFIX`, and the indexed tool rules
  `ZBX_ALLOWTOOL_<N>` / `ZBX_DENYTOOL_<N>` (+ regex variants). No
  ConfigMap needed.

## CRD surface

Optional `spec.agentIntegration.mcp` block (presence = enabled,
matching every other optional feature):

```yaml
spec:
  agentIntegration:
    mcp:
      image:            # ImageSpec override (repo/tag/pullPolicy)
      resources:        # container resources
      allowedIPs: []    # AllowedIP; unset keeps the image default
                        # (localhost only — useless in-cluster; set this)
      allowTools: []    # wildcard rules; see takeover semantics below
      denyTools: []     # wildcard rules; see takeover semantics below
      toolPrefix: ""    # ZBX_TOOL_PREFIX
      service:          # optional exposure, exactly like frontend:
        type:           # ClusterIP (default) / NodePort / LoadBalancer
        annotations: {}
```

The MCP server does not use the operator's API token; it needs only the
frontend URL, so it is allowed without `spec.api`. Version gate: Zabbix
>= 8.0 (`SupportsAgentIntegration`) — enabling on 7.0 deploys nothing,
deletes anything a previous (supported) version deployed, and reports
`AgentIntegrationReady=False` with reason `MCPUnsupportedVersion`.

## Decisions (and why)

- **`allowedIPs` default = image default (localhost only).** The
  operator cannot know MCP client addresses; forcing an explicit list
  documents intent. The CRD comment steers users: set the agent pods'
  CIDR, avoid `0.0.0.0/0`.
- **Tool rules REPLACE the image default policy (image contract).**
  Setting any indexed rule env var discards the image's default deny
  list (`item_get`, `lld_get`, `interface_get`, `macro_get`,
  `history_get`). The operator therefore renders the rules in
  evaluation order: user allows → user denies → default denies minus
  tools the user's rules decide. First match wins, so an explicit allow
  beats the default deny of the same tool. Regex rule variants are
  deferred (wildcards cover the realistic cases; regex adds escaping
  risk through the CRD).
- **Probes are TCP on 8443.** The transport answers 401 without a
  token and 403 for sources outside AllowedIP — HTTP probes would need
  a token.
- **No NetworkPolicy in this pass.** The MCP Deployment is namespaced
  and the default exposure is ClusterIP; a NetworkPolicy scoped to DNS
  + the frontend Service follows with audit follow-up #4, uniformly for
  all components.

## Objects the operator converges

| Object | Name | Notes |
| --- | --- | --- |
| Deployment | `<instance>-mcp-server` | 1 replica; restricted container context (`RunAsUser: 1997`, seccomp, drop ALL, no privilege escalation); TCP probes on 8443 |
| Service | `<instance>-mcp-server` | ClusterIP default; port name `mcp` on 8443 |

**Wiring:** `ZBX_FRONTEND_URL=http://<instance>-frontend.<ns>.svc:80/api_jsonrpc.php`
— the same always-own-frontend pattern as the API client and
`WebServiceURL` (documented plaintext finding #5 applies; the frontend
Service is instance-internal).

**Status:** `AgentIntegrationReady` condition (Deployment-ready).
Teardown mirrors the managed-ClickHouse removal path (name-based
deterministic delete on disable/unsupported; owner references handle
instance deletion — no cluster-scoped objects involved).

## Security posture

- AuthN/authZ: entirely delegated to the Zabbix API token the client
  presents. The operator's own token is never exposed to this
  component.
- Default exposure ClusterIP: agent access requires in-cluster
  connectivity; wide exposure is an explicit user opt-in (NodePort/LB),
  same stance as the frontend.
- `allowedIPs` maps to `AllowedIP` (the image default denies
  unspecified clients with 403) — the CRD requires users to think about
  it by leaving it unset until they do.
- Tool deny defaults ship with the image; the CR only ever ADDs
  restrictions unless `allowTools` explicitly un-denies a tool (and the
  takeover semantics re-render the remaining defaults).

## e2e coverage

1. Enable → Deployment/Service roll out, condition true, frontend-URL
   env wired; `tools/list` over the in-cluster endpoint with a dummy
   token → expect 401 (`allowedIPs: ["0.0.0.0/0"]` in the test CR so
   the request reaches authn; parked-state assertion, matching the
   clusterMetrics dummy-token pattern; real-token e2e stays with the
   backlog item).
2. Disable → objects gone, condition dropped, stack stays Ready.

## Non-goals

- No client-side integration (agents are the user's concern).
- No per-user token management by the operator.
- 7.0 LTS: no MCP server exists there — feature is 8.0+ only.
