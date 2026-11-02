# Host MCP upstreams behind Envoy

Status: design, being implemented. It gives agents the tools of an
existing MCP server (Grafana's, GitHub's, any other) without putting the
server's credential, or a route to its backend, in the container. The host
runs the server with a credential of its own, Envoy proxies it, and Envoy
holds each agent's tool allowlist. Grafana's MCP server is the first
instance; GitHub's follows once it can hold a configured credential in its
HTTP mode (see "GitHub").

A new upstream needs only configuration: an image digest, its flags, the
path of its credential file, its egress, and each agent's allowlist. It
never needs new redoma code. redoma's code names no upstream and no tool;
what it adds is generic and written once.

## Why

An agent that works on a repository needs more than its workspace: it
reads dashboards and logs, opens pull requests, answers review comments,
reads the checks. The only way to do that from a redoma container today is
to give it what a person would use: a token in its environment, a CLI, and
the service's hosts in its egress allowlist. That undoes what redoma does
for the model credential, which never enters the container:

- **The container holds the credential.** Anything the turn runs can read
  it and use it for anything its scopes allow, for as long as it is valid.
- **Policy is the service's alone.** A token that can open a pull request
  can, with the wrong scopes, merge, approve or delete branches. Nothing on
  the host can say "this agent, these tools".
- **Nothing on the host sees what was done.** The egress proxy sees a
  CONNECT to the service's host, not which call was made.

Writing redoma-native tools for each service would not scale, and each
would be code to audit. Existing MCP servers already implement the tools.
What is missing is a way to run them on the host, limit them per agent,
and keep their credentials out of the container.

## The pattern

```
agent container (own netns)                   host
  claude --mcp-config: http://127.0.0.1:8081/mcp/<upstream>
     |
     v
  127.0.0.1:8081 agent_<name>_mcp  -- host Envoy --------------------------+
     http_filters, per agent:                                              |
       header_mutation (remove authorization and the upstream's            |
                        override headers the agent sent)                   |
       mcp (parse JSON-RPC: method, params.name, configured arguments)     |
       rbac (this agent's allowlist; anything else is a 403)               |
       router (/mcp/<upstream> to the upstream's cluster)                  |
                                                                           v
                                     cluster mcp_<upstream>
                                       source address in the upstream's netns
                                                                           |
upstream container (own netns, --network none)                             |
  127.0.0.1:<port> the MCP server, HTTP mode, its credential  <------------+
    read from /run/secrets/<upstream>/token (re-read on change)
  HTTPS_PROXY=127.0.0.1:3128 -> Envoy egress listener -> the upstream's backend hosts
```

- **The agent's side.** A fourth per-agent listener, `agent_<name>_mcp`
  on 127.0.0.1:8081 in the agent's namespace, serves every upstream under
  `/mcp/<upstream>` (MCP's Streamable HTTP, HTTP/1.1 or HTTP/2). claude
  reaches it through an `http` entry of the shim's `--mcp-config`, one per
  upstream the agent has. As with the model and egress listeners, the
  listener the connection arrives on is the agent's identity: only that
  agent's namespace reaches it. The agent speaks plain HTTP to its own
  loopback; no SVID is presented on this hop, and none needs to be.
- **The upstream's side.** Each upstream server runs in a rootful Podman
  container of its own, `--network none`, in its own network namespace
  like an agent, and in a user namespace of its own (`--userns=auto`).
  Envoy binds an egress listener in that namespace, holding the upstream's
  own host allowlist. Envoy reaches the server's port on that namespace's
  loopback through a cluster whose source address is in that namespace.
  Nothing else can reach the server: it has no network but Envoy's, so it
  authenticates no caller and needs no SVID.
- **The credential is the upstream's own.** The server holds it for its
  whole life, read from a file the host keeps current (see "The
  credential"). Envoy never sees it, injects it or routes by it.
- **Between them**, Envoy parses each MCP request, removes every request
  header that could change the server's identity, target or tool set, and
  refuses any method or tool not on the agent's allowlist. The container
  never holds a credential.

### What redoma adds, once

- `redoma xds` publishes, per agent with any upstream: the MCP listener,
  its RBAC policy, and its routes. Per upstream, it publishes the egress
  listener in the upstream's namespace and the cluster to its server. All
  of it is built from the configuration below, re-read at every scan like
  the egress allowlists. The host bootstrap (`examples/envoy.yaml`) does
  not change: everything comes over LDS and CDS.
- The OCI hook registers an upstream container (annotation
  `redoma/upstream=<name>`) much as it registers an agent. At createRuntime
  it mounts the namespace at `/run/netns/redoma-mcp-<container ID>` and
  applies the same process and mount checks. The user and network namespace
  checks differ, because an upstream runs with `--userns=auto` (see
  "Users"): the hook registers it with `redoma xds`, under its upstream
  name, only from its poststart stage, once the namespace has no interface
  but `lo`. That needs a new hook definition in `ocihook/hooks.d`, a
  `poststart` stage matching the annotation `redoma/upstream`, beside the
  existing createRuntime and poststop ones. An upstream name is never an
  agent name: a container carrying both annotations is refused, and the two
  namespaces of names are kept apart in `agent.Merge`.
- The shim gains `--mcp-upstream=<name>` (repeatable): an `http` entry
  named `<name>` in its `--mcp-config`, so the tools appear to claude as
  `mcp__<name>__*`, and that pattern in `--allowedTools`. This only decides
  what claude is offered; it grants nothing.
- Two systemd template units in the package:
  - `redoma-mcp@.service` runs one upstream container from its
    configuration file (image by digest, arguments, environment, the
    credential's directory), so starting an upstream is
    `systemctl enable --now redoma-mcp@<name>`. One instance of each
    upstream serves every agent that has it.
  - `redoma-mcp-token@.path` and `redoma-mcp-token@.service` copy the
    upstream's credential from where the host's minter writes it into
    `/run/redoma-mcp/<upstream>/token`, atomically, whenever it changes
    and before each start of the upstream.

## What was verified

Against the Envoy API that redoma pins (`github.com/envoyproxy/go-control-plane/envoy`
v1.40.0, the API of Envoy v1.40.0) and Envoy's sources at v1.40.0 and
v1.40.2; against `grafana/mcp-grafana` v2.2.0 (commit `cb8e115`); and
against `github/github-mcp-server` v2.0.3 (commit
`b98d115bbb080e9555466a96656ec4df605aea21`). Paths are in those trees. The
host must run an Envoy that has these extensions; v1.40.0 or later. The
MCP filter is a core extension (`source/extensions/extensions_build_config.bzl:209`
at v1.40.2), so Envoy's standard and distroless builds carry it.

### The MCP filter (`envoy.filters.http.mcp`)

- **Status: alpha, `security_posture: unknown`**
  (`source/extensions/extensions_metadata.yaml:682-686`). Its proto file is
  `work_in_progress` (`api/envoy/extensions/filters/http/mcp/v3/mcp.proto:17`),
  and its documentation says it "is actively under development"
  (`docs/root/configuration/http/http_filters/mcp_filter.rst:11-13`).
  The agent is an untrusted downstream, and this filter has not been
  hardened against one.
- **What it extracts.** It writes the request's attributes to dynamic
  metadata under the namespace `envoy.filters.http.mcp`
  (`source/extensions/filters/common/mcp/constants.h:16`): `method`, and
  for `tools/call` the path `params.name`. Further paths, such as
  `params.arguments.datasourceUid`, can be added per method with
  `parser_config` extraction rules (`mcp.proto:177-200`). Arrays are
  skipped by the parser
  (`source/extensions/filters/http/mcp/mcp_json_parser.cc:354-356`), so no
  rule can see inside a list argument.
- **Modes that matter.**
  - `traffic_mode: REJECT_NO_MCP` refuses a request that is not MCP, and
    one whose body exceeds `max_request_body_size`, with a 400. The default,
    `PASS_THROUGH`, lets both through, the oversized one with only a marker
    in the metadata (`mcp.proto:27-43`, `:91-101`). redoma uses
    `REJECT_NO_MCP`.
  - `attribute_source: BODY` (the default) parses the body and ignores the
    `Mcp-Method` and `Mcp-Name` headers. `HEADERS` trusts the client's
    headers and must never be used here (`mcp.proto:62-73`).
  - `reject_duplicate_keys: true` refuses a body with a repeated key in
    `REJECT_NO_MCP` mode, so that no parser downstream can read a different
    value than Envoy did (`mcp.proto:133-138`).
- **A parser differential, to check per upstream.** Envoy reads
  `params.name` and every extracted argument with exact case, and
  `reject_duplicate_keys` does not treat `name` and `Name` as duplicates.
  A server that decodes case-insensitively could read a different value
  than Envoy checked. For github-mcp-server v2.0.3 the dispatch is done by
  the MCP Go SDK (v1.8.0), which decodes with case matching on
  (`internal/json/json.go:30-33` in go-sdk,
  `DontMatchCaseInsensitiveStructFields`), so a `"Name"` key next to
  `"name"` makes the call fail; it cannot make the server call a tool other
  than the one Envoy checked. Arguments are a different matter: some of
  that server's tools decode them with `mapstructure.WeakDecode`, which
  matches keys case-insensitively.
  **mcp-grafana v2.2.0 is such a server for every tool:** it decodes a
  tool's arguments with `encoding/json` (`tools.go:177-191`), which matches
  keys case-insensitively, the last one winning. A call with
  `{"uid": "allowed", "UID": "other"}` passes a constraint on `uid` and
  reaches `other`; the integration test shows it, and fails if a release
  changes it. So, for Grafana, an argument constraint is no boundary, and a
  narrower data scope is Grafana's to enforce (see "Its token").
- **The rule that follows.** An argument constraint is a boundary only if
  the server decodes that argument case-sensitively and refuses or ignores
  a duplicate of it in another case. That is a property of each tool's
  code, not of the protocol, so an upstream's design must prove it for
  every argument its allowlists constrain, tool by tool, citing the code
  (GitHub's included, see "GitHub"), and the upgrade checklist proves it
  again at every release (see "Upgrading an upstream"). Until it is
  proven, a constraint on that argument is advisory, and the allowlist's
  real limit is its tool names. Each upstream's parser is part of its
  trust, which is one reason the server's own limits stay the first layer
  (see "Layered limits").
- **Risk, stated plainly.** A bug in this filter could do one of two things:
  - **Pass a call RBAC should have refused.** The server's own ceiling and
    the backend's permissions for the upstream's credential still hold.
  - **Crash the Envoy that serves every agent's model, egress and relay
    listeners.** That is the larger risk, because the Envoy is shared.

  **Decision:** use the filter, with the layers below so that no single
  bug grants a dangerous tool.
  - **No fallback.** redoma has no second parser of MCP requests, in
    `ext_proc` or anywhere else. A bug in the filter is fixed in Envoy,
    upstream, and hosts take the Envoy release that fixes it; it is not
    worked around in redoma.
  - **One Envoy.** The MCP listeners run on the same host Envoy as the
    model, egress and relay listeners, not a second one. The accepted risk:
    a crash in this alpha filter takes down every listener of every agent,
    model and relay included, until Envoy restarts. A second Envoy would
    contain that, at the cost of a second process, bootstrap and xDS client
    to run and keep in step; that cost is not taken.

### RBAC on the tool name, per agent

- `envoy.filters.http.rbac` is stable and `robust_to_untrusted_downstream`
  (`extensions_metadata.yaml:759-763`). A permission can match dynamic
  metadata of a given filter with `sourced_metadata`
  (`api/envoy/config/rbac/v3/rbac.proto:212-233`, `Permission.sourced_metadata
  = 14` at `:321`). Envoy's own example allows `tools/call` only for a
  named tool, matching `method` and `params.name` under
  `envoy.filters.http.mcp`
  (`docs/root/configuration/http/http_filters/_include/mcp-filter.yaml:234-256`),
  with the MCP filter placed before RBAC (`mcp_filter.rst:57-77`).
- **Keyed on the caller's identity:** the RBAC filter sits in the agent's
  own listener, `agent_<name>_mcp`, which only that agent's namespace can
  reach. So the policy is the agent's, its principal is `any`, and no
  certificate or header is involved. This is the same identity the model
  and egress listeners rely on, and it holds in Delegated and Broker mode
  alike.
- **The policy is an allowlist.** One policy, `ALLOW`, whose permissions
  are, for each upstream route of the agent:
  - the lifecycle methods (`initialize`, `notifications/initialized`,
    `ping`, `tools/list`);
  - `tools/call` with `params.name` among the agent's tools, each AND-ed
    with its argument constraints, if any (below).

  Anything that matches no permission is a 403: another method
  (`resources/*`, `prompts/*`, `sampling/*`), another tool, an unparsed
  body. The filter's `REJECT_NO_MCP` mode has already refused anything that
  is not MCP.
- **The path, exactly.** Each permission also matches the request's
  `:path` header exactly, `/mcp/<upstream>`. `:path` includes the query
  string, so a request with any query is a 403: MCP's Streamable HTTP
  needs none, and a query would be a second channel, besides the headers,
  through which the agent could configure a server.
- **A body only on POST.** GET (the session's event stream) and DELETE
  (the session's end) are allowed on the method, because the MCP filter
  passes them before reading any body. So RBAC allows them only without a
  body: no `content-length` other than 0 and no `transfer-encoding`. An
  HTTP/2 request can still send DATA without either header, so that a
  server ignores a GET or DELETE body is a step of the upgrade checklist.
- **Argument constraints.** An extraction rule makes an argument visible to
  RBAC, so a tool can be allowed only with, say, `owner` = `example-org`
  and `repo` = `example-repo`, or a `branch` that starts with
  `agent/example/`. Each holds only where the upstream decodes that
  argument case-exactly ("The rule that follows", above). They are
  exact or prefix string matches on scalar arguments, and an argument a
  constraint names must be present: a call without it is refused. A list
  argument cannot be constrained (the parser skips arrays), so a tool whose
  target sits in a list (the paths in GitHub's `push_files`, for example)
  is constrained only by the upstream and the backend.
- **Constraint sets.** An agent's allowlist is one or more sets, each a
  list of tools with the constraints that apply to them. A call is allowed
  when any set allows it; RBAC expresses the sets as OR-ed permissions. So
  one file can allow, for example, reads on the agent's own repository and
  on another, and writes on its own only.
- **What RBAC cannot check** is anything that needs the backend's state.
  Envoy sees the arguments, not what they point at. A constraint on
  `owner` and `repo` keeps a comment in the allowed repositories. It cannot
  tell whether a pull request number is one of the agent's own. So, within
  an allowed repository, an agent may comment on or reply in any pull
  request or issue. "Its own pull requests only" is not expressible in this
  design: it would need a lookup at the backend, which nothing on the path
  does.

### Request headers the agent may not send

An upstream that takes its configuration from the request is configured by
the agent unless Envoy removes those headers. The configured credential is
the clearest case: a server that prefers a request's token over its own
runs as whoever's token the agent sends. So the agent's listener removes,
from every request to every upstream:

- `authorization`, always, whatever the upstream's configuration says;
- the upstream's `strip_header` names (exact) and `strip_header_prefix`
  families (any case), which the upstream's `.conf` lists: for Grafana
  `x-grafana-`, for GitHub `x-mcp-`.

**How.** A route's `request_headers_to_remove` takes exact names only
(`api/envoy/config/route/v3/route_components.proto:374-376`), so it cannot
remove a family of headers. The agent's listener therefore runs
`envoy.filters.http.header_mutation` before the MCP filter, with one
`remove_on_match` request mutation per prefix (a `key_matcher` string
matcher, prefix, `ignore_case`) and per name
(`api/envoy/config/common/mutation_rules/v3/mutation_rules.proto:93-111`,
`api/envoy/extensions/filters/http/header_mutation/v3/header_mutation.proto:22-24`).
The filter is alpha, `security_posture: unknown`
(`extensions_metadata.yaml:835-841`), and sees only headers. One listener
serves every upstream of the agent, so each upstream's families are
removed from requests to every upstream: a header one upstream treats as
configuration is no business of the agent's at any other. A per-agent
header an upstream wants (`agent_header.<Name>`) is added after the
removal, by the route: the router applies a route's header additions after
the listener's filters have run.

Kept, because the protocol needs them: `Mcp-Session-Id`,
`MCP-Protocol-Version`, `Mcp-Method`, `Mcp-Name` and `Mcp-Param-*`. With
protocol version `2026-07-28` the MCP filter requires `Mcp-Method` and
`Mcp-Name` and refuses a request whose headers disagree with its body
(`source/extensions/filters/http/mcp/mcp_filter.cc:422-425`, `:494-540`,
`:798-828`). `Mcp-Param-*` headers carry tool arguments from that version
on; Envoy does not read them, it checks the body. An upstream that routed
or authorized on them rather than the body would let a client on an older
protocol send a header that disagrees with what RBAC checked; checking that
is a step of the upgrade checklist. Pinning `MCP-Protocol-Version` in RBAC
would close that gap generically, at the cost of refusing every client that
does not speak that version yet, `initialize` included; it is not pinned.

### Reaching a server in another namespace

`SocketAddress.network_namespace_filepath` makes Envoy create the socket in
that namespace (`api/envoy/config/core/v3/address.proto:100-110`). For a
cluster, it applies through the source address of the cluster's bind
config (`:184-197`). So the cluster `mcp_<upstream>` connects to the
upstream's `port` on 127.0.0.1 from a source address in
`/run/netns/redoma-mcp-<id>`, and reaches the server's loopback. The route
rewrites `/mcp/<upstream>` to the upstream's own `path` and sets the
`Host` header to `127.0.0.1:<port>`, so a server that validates its Host
(Grafana's does, against loopback variants of its address) sees its own.

## The credential

**The rule:** an upstream's credential reaches its server only as a file,
which the server re-reads when it changes.

- The upstream's `.conf` names the file's host path, `token_file`, under
  `/run/redoma-mcp/<upstream>/`. The unit mounts that **directory**
  read-only at `/run/secrets/<upstream>/` in the container, so a rename
  over the file is visible inside, and refuses to start when the file is
  missing.
- The server is told the in-container path in its `arg` or `env` lines
  (Grafana: `GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE=/run/secrets/grafana/token`).
  Those lines may name the path, never a value, and both kinds are
  checked. A name says it holds a secret when it contains `token`, `key`,
  `secret`, `password` or `credential`, in any case:
  - an `env` with such a name must end in `_FILE` or `_PATH`;
  - an `arg` that is a flag (`-x`, `--x`, `--x=value`) with such a name
    must end in `-file`, `-path`, `_file` or `_path`, given with its
    value or without (a value in the next `arg` line is the same value).

  Either refusal makes the file invalid. The check is by name, so a flag
  that takes a secret under another name is not caught, and stays a step
  of the upgrade checklist.
- **What an upstream must provide.** A way to hold its credential from a
  file, re-read on change, in its shared HTTP mode. Grafana's server does
  (below). One that does not needs an upstream change first; a sidecar
  that sets a header from the file would work too, but it is per-request
  injection by another name and is a last resort.
- **Delivery.** The host's own minter (a timer that mints or copies a
  short-lived token; redoma does not ship one) writes the token to a path
  of its choosing, replacing it by rename. `redoma-mcp-token@<upstream>.service`
  copies that file to `/run/redoma-mcp/<upstream>/token.tmp` (directory
  root's, 0700) and renames it to `token`. The root copy keeps
  `/run/redoma-mcp` out of every user session's reach. It runs:
  - **at every start of the upstream:** `redoma-mcp@<upstream>.service`
    wants it and starts after it, so the first copy after a boot (`/run`
    is a tmpfs, and the token is gone) is made before the server starts;
  - **at every change:** `redoma-mcp-token@<upstream>.path` watches the
    minter's path (`PathChanged=`, which fires only on a change, never for
    a file that already exists when the path unit starts). Both the path
    and the source come from a drop-in the host writes, so redoma names no
    path under anyone's home.

  When the minter has not written yet, the copy fails, the upstream's
  start fails on the missing token and is retried, and the minter's first
  write makes the copy that lets it start.
- **The copier trusts its source no further than the minter.** The source
  is a path the minter chooses, possibly under a user's home, and the
  copier is root, so it refuses to be pointed at any other file. It walks
  the path from `/` one component at a time without following a symlink
  (`openat` with `O_NOFOLLOW`), and every directory on the way must be
  owned by root or the minter's uid and writable by no one else. The file
  itself, opened with `O_NOFOLLOW` and `O_NONBLOCK` (a FIFO cannot hang
  it), must be a regular file owned by the minter's uid, which is root
  unless the drop-in names another (`REDOMA_MCP_TOKEN_OWNER`), writable by
  no one else, and at most 64 KiB. A minter that swaps the file for a
  symlink, or a directory on its path for one, gets a refusal, not a copy
  of a file root can read.
- **Rotation** is the minter's: write a new token before the old one
  expires. The server reads the new file at its next request (Grafana) or
  on change; no restart. A missing or expired token is the backend's 401,
  returned to the agent as the tool's error. There is no fallback to another
  credential.
- **One credential per upstream instance.** Every agent that uses an
  upstream shares its credential, so the backend enforces only the union of
  what those agents may do, and each agent's own limit is Envoy's RBAC. An
  upstream that needs a second identity for one tool (GitHub's pull request
  authorship, below) runs a second instance with its own credential and
  ceiling, and Envoy routes that tool to it (`route_for`).

**What a credential in the server costs.** The server now holds a live
token for its whole life. A bug in it (it parses backend content, which
may be written by anyone) or a compromised image yields that token. The
container's hardening is what keeps the token from leaking anywhere but its
backend: `--network none` with an egress listener that allows only the
backend's hosts, `--userns=auto`, all capabilities dropped,
`no-new-privileges`, a read-only root, and the image pinned by digest.

**Who validates the backend's TLS.** The server alone. The egress listener
is a CONNECT tunnel to the allowed hosts and does not terminate TLS, so the
backend's certificate is checked by the server, against its image's
roots. A server that skipped that check would send its credential to
whatever answered on the tunnelled host. So an `arg` or `env` whose name
says it turns verification off (it contains `insecure`, `skip-verify` or
`skip_verify`, or is `NODE_TLS_REJECT_UNAUTHORIZED`, in any case) makes the
upstream's `.conf` invalid, and how a release verifies its backend, and
which flags or variables change its roots, is a step of the upgrade
checklist.

Exfiltration is then possible only through the backend itself (written
into a comment, a dashboard, an annotation), which is why each upstream's
credential has the least the backend allows, and its write tools are off
unless needed.

## Grafana

`grafana/mcp-grafana` v2.2.0 (`cb8e115`), the first upstream.

### Its credential

- **Transports:** `-t stdio|sse|streamable-http`
  (`cmd/mcp-grafana/main.go:1416-1422`) and `--address` (default
  `localhost:8000`, `:1425`). Streamable HTTP runs the MCP SDK's handler
  stateless (`main.go:1383-1387`), so one instance serves any number of
  agents.
- **A configured credential from a file.** `GRAFANA_URL`, plus
  `GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE`, which is read fresh on every call
  "so that rotated tokens ... are picked up without restarting the server"
  (`mcpgrafana.go:82-92`). That is the contract "The credential" needs.
- **But a request can override it.** In HTTP mode
  (`extractKeyGrafanaInfoFromReq`, `mcpgrafana.go:1093-1125`):
  - the token comes from the request's `X-Grafana-Service-Account-Token`
    or the deprecated `X-Grafana-API-Key` (`apiKeyFromHeaders`,
    `:215-224`); only when the request carries neither does the configured
    token apply (`:1105-1108`);
  - basic auth comes from the request's `Authorization: Basic ...` when
    present, else the environment (`:1100-1115`);
  - the org comes from `X-Grafana-Org-Id`, else the environment
    (`:1117-1122`);
  - a request-selected Grafana, `X-Grafana-URL`, needs
    `--allow-grafana-url-override`, off by default (`main.go:269`);
  - headers named in `GRAFANA_FORWARD_HEADERS` are forwarded to Grafana;
    it stays unset.

  So the agent's listener strips `authorization` and every `x-grafana-*`
  header. With those gone, every request runs as the configured token
  against `GRAFANA_URL`.
- **Caller authentication** (`--server-auth-token`, `main.go:737-766`) is
  not needed: the server binds loopback in a namespace only Envoy reaches.
  It logs a warning for a loopback address without it (`:762-763`), which
  is expected.

### Its ceiling and arguments

```
-t streamable-http --address=127.0.0.1:8000 --endpoint-path=/mcp
--disable-write
--enabled-tools=search,datasource,prometheus,loki,dashboard,alerting,navigation,tempo,pyroscope,annotations
--disable-api --disable-admin --disable-incident --disable-oncall --disable-sql
--disable-assistant --disable-rendering --disable-snapshot --disable-plugin
--disable-provisioning
--loki-guardrail-mode=enforce --prometheus-guardrail-mode=enforce
--usage-stats=disabled
```

- `--disable-write` (`main.go:241`) removes every create, update and delete
  tool, the raw-SQL query tools and the assistant.
- `--enabled-tools` (`main.go:222`) takes **categories**, not tool names,
  so this ceiling is coarser than GitHub's `--tools`. Per-tool precision
  is Envoy's RBAC and the tool check's.
- `--disable-api` matters: `grafana_api_request` is a raw API passthrough
  that would bypass every tool-level limit.
- The `--disable-*` flags each remove a category whether or not
  `--enabled-tools` names it, so the ceiling holds even if a release
  enables a category by default; they are all passed, here and in the
  example below.
- The guardrail modes refuse queries that are too expensive, with advice
  to narrow them, rather than letting an agent run an unbounded scan.
- `--usage-stats=disabled`: the server reports anonymous usage statistics
  to a Grafana endpoint by default. The egress listener would refuse it
  (that host is not the upstream's backend), but the server would keep
  trying; the flag outranks the environment's settings.

### Its tools

- **Read, the usual allowlist:** `search_dashboards`, `search_folders`;
  `list_datasources`, `get_datasource`, `check_datasources_health`;
  `query_prometheus`, `query_prometheus_histogram`,
  `list_prometheus_metric_names`, `list_prometheus_metric_metadata`,
  `list_prometheus_label_names`, `list_prometheus_label_values`;
  `query_loki_logs`, `query_loki_stats`, `query_loki_patterns`,
  `list_loki_label_names`, `list_loki_label_values`;
  `get_dashboard_by_uid`, `get_dashboard_summary`,
  `get_dashboard_property`, `get_dashboard_panel_queries`;
  `alerting_rules_read`, `alerting_silences_read`; `get_annotations`,
  `get_annotation_tags`; the Tempo and Pyroscope read tools;
  `generate_deeplink`.
- **Read, but opt-in:** `run_panel_query` (arbitrary panel queries),
  `alerting_manage_routing` (contact points can carry webhook URLs and
  secrets).
- **Write: never allowed.** Removed by `--disable-write` and on no
  allowlist: dashboards, folders, datasources, incidents, annotations,
  alerting writes, snapshots, plugins, the assistant, SQL, and
  `grafana_api_request`. `generate_deeplink` with `shorten=true` creates a
  short URL, a write; it stays read-only by not passing
  `--enable-write-tools=generate_deeplink`.
- **Constraints do not confine it.** The query tools take a scalar
  `datasourceUid`, but the server decodes arguments case-insensitively (see
  "A parser differential"), so a set constrained to some datasources is
  advisory: a case-variant key reaches any datasource the token can read.
  Allowlists for Grafana name tools, which RBAC does hold; which data an
  agent reads is the token's.

### Its token

A dedicated Grafana service account with the **Viewer** role, its token
with an expiry. Viewer is the backend's layer: even a write tool that were
registered and allowed would be refused by Grafana. What Viewer still
exposes is every datasource's data, logs included, and logs can carry
secrets, which then enter the model's context. Narrow the data on
Grafana's side, where it holds: the service account's datasource and
folder permissions (where the Grafana edition has them), Loki's enforced
matchers (`--loki-enforced-matchers`, which needs `--disable-api`), or,
for agents that must see different data, a second upstream instance with
its own service account, each agent's allowlist naming only its
instance.

### Its configuration

```
# /etc/redoma/mcp/grafana.conf
image = docker.io/grafana/mcp-grafana@sha256:<the v2.2.0 index digest>
port = 8000
path = /mcp
tool_check = refuse
arg = -t
arg = streamable-http
arg = --address=127.0.0.1:8000
arg = --endpoint-path=/mcp
arg = --disable-write
arg = --enabled-tools=search,datasource,prometheus,loki,dashboard,alerting,navigation,tempo,pyroscope,annotations
arg = --disable-api
arg = --disable-admin
arg = --disable-incident
arg = --disable-oncall
arg = --disable-sql
arg = --disable-assistant
arg = --disable-rendering
arg = --disable-snapshot
arg = --disable-plugin
arg = --disable-provisioning
arg = --loki-guardrail-mode=enforce
arg = --prometheus-guardrail-mode=enforce
arg = --usage-stats=disabled
env = GRAFANA_URL=https://grafana.example.org
env = GRAFANA_SERVICE_ACCOUNT_TOKEN_FILE=/run/secrets/grafana/token
token_file = /run/redoma-mcp/grafana/token
egress = grafana.example.org
strip_header_prefix = x-grafana-
max_request_bytes = 1048576
```

```
# /etc/redoma/mcp/grafana/example.conf
# Tools only: for this server a datasourceUid constraint is advisory (see
# "Constraints do not confine it").
tool_unconstrained = query_prometheus
tool_unconstrained = list_prometheus_metric_names
tool_unconstrained = list_prometheus_label_values
tool_unconstrained = query_loki_logs
tool_unconstrained = list_loki_label_values
tool_unconstrained = search_dashboards
tool_unconstrained = get_dashboard_summary
tool_unconstrained = list_datasources
```

## GitHub

`github/github-mcp-server` v2.0.3. Its HTTP mode **cannot** hold a
configured credential yet: it takes the token from each request's
`Authorization` header and nothing else (`pkg/http/middleware/token.go:13-47`,
wired for every request at `pkg/http/handler.go:142`; `httpCmd` reads no
token, `cmd/github-mcp-server/main.go:170-226`), and the project says so
(`docs/github-app-auth.md:8-9`). Its `stdio` mode does take one, but a
stdio process serves one session and cannot be the shared instance. So
GitHub becomes an upstream once its HTTP mode gains a token file, re-read
on change (an upstream change, `--token-file`), and the client's
`Authorization` is then ignored or refused. Everything below is ready for
that, and needs no redoma change when it lands.

### What it reads from a request

- **Request headers can change the tool set, and the flags bound them only
  when one is set.** HTTP mode reads `X-MCP-Toolsets`, `X-MCP-Tools`,
  `X-MCP-Readonly`, `X-MCP-Exclude-Tools`, `X-MCP-Lockdown`,
  `X-MCP-Insiders` and `X-MCP-Features` from each request
  (`pkg/http/middleware/request_config.go:16-64`). With any of
  `--toolsets`, `--tools`, `--exclude-tools` or `--read-only` set, the
  flags are a ceiling and the headers can only narrow it
  (`pkg/http/handler.go:302-351`, `hasStaticConfig` at `:376-381`); with
  none set, a client's `X-MCP-Toolsets: all` reaches every tool. So its
  `.conf` requires a static `--tools` list, strips `x-mcp-*`, and may set
  `X-MCP-Tools` per agent so that `tools/list` shows each agent only its
  own tools (`agent_header.X-MCP-Tools = {tools}`).
- **`Mcp-Param-*`.** The server annotates the `owner` and `repo` arguments
  of its tools (`HeaderParams`, `pkg/inventory/server_tool.go:276-289`);
  from protocol `2026-07-28` the SDK's server refuses a `tools/call` whose
  headers differ from the body (`mcp/streamable.go:1616-1631`,
  `mcp/streamable_headers.go:359-363`, `:402-442` in go-sdk v1.8.0). On an
  earlier protocol nothing checks them, and at v2.0.3 nothing acts on them
  either: dispatch and arguments come from the body RBAC checked.
- **Image:** `ghcr.io/github/github-mcp-server:v2.0.3`, an OCI index,
  `sha256:250bc6ce70788207608ef74cef4397230ff2e8481218da74d6093e4bd9101132`.
  Its HTTP client is Go's default transport, which honours `HTTPS_PROXY`.

### Pull request authorship: a second instance

A pull request opened through the shared instance is authored by its
credential's identity, and so is its squash commit. A host that wants pull
requests opened by a person runs a second instance, `github-pr`: the same
image, `--tools=create_pull_request` only, and the person's token in its
own file. The shared upstream's `.conf` routes that one tool there:

```
route_for.create_pull_request = github-pr
```

Envoy routes to `mcp_github-pr` only a request whose extracted metadata is
`method` = `tools/call` and `params.name` = `create_pull_request`, both
exact. A route's `dynamic_metadata` matchers must all match
(`api/envoy/config/route/v3/route_components.proto:778-782`), after the MCP
filter has run with `clear_route_cache` (`mcp.proto:86-89`). Every other
request, `tools/list` included, goes to the shared instance. This is a
server-side ceiling, not only routing: even a misrouted request cannot
reach an approval tool with the person's token, because that instance never
registers one. A `route_for` target must be an upstream whose own `.conf`
exists; its tool check must list the tool.

**`route_for` and sessions.** A `tools/call` routed to the second instance
carries the `Mcp-Session-Id` the first instance gave the client. A
stateless server (Grafana's, `main.go:1383-1387`, and GitHub's,
`pkg/http/handler.go:261`, run their SDK's handler stateless) ignores it; a server that keeps sessions does not know it, and
refuses the call. So `route_for` is for servers whose HTTP mode is
stateless, which the upgrade checklist confirms for both instances.

### The person's token, and what GitHub does not stop

- **Its scope:** a fine-grained token on the agents' repositories only,
  with Pull requests read and write, Contents read and Metadata read.
  Nothing else. Contents read means it cannot push, change files or merge
  (a merge writes contents).
- **GitHub is no backstop against an approval in that person's name.**
  Pull requests write allows commenting on, editing and closing pull
  requests, and approving or requesting changes on pull requests other
  people opened; GitHub refuses an approval only of the person's own. What
  stops an approval on this route is the instance's `--tools` ceiling
  (`create_pull_request` alone), the route's exact match, and RBAC, each
  enough on its own.
- **The shared instance's credential and a merge.** A credential with
  contents and pull requests write can merge a pull request that is
  approved and green: the default branch's ruleset refuses a merge only
  while a review or a required check is missing. So for such a pull
  request GitHub is no backstop, and layers 1 and 2 alone stop the merge:
  `merge_pull_request` is not registered, and RBAC does not allow it.
- **An approval on the shared instance** is stopped the same way: Pull
  requests write allows submitting a review, so GitHub would not refuse
  it, and the review tools are neither registered nor allowed.

### Its argument constraints, proven per tool

The example below constrains `owner`, `repo`, `head` and `base`. Each is a
boundary only where the tool reads it case-exactly ("The rule that
follows"). At v2.0.3:

- `create_pull_request` reads `owner` and `repo` with `RequiredParam` and
  `head` and `base` with `OptionalParam` (`pkg/github/pullrequests.go:769-804`),
  both an exact lookup in the arguments' map (`pkg/github/params.go:128-140`,
  `:200-211`). A `"Repo"` key next to `"repo"` is another key, never read.
- The tools that decode their arguments with `mapstructure.WeakDecode`,
  case-insensitively (`pull_request_review_write`, the Copilot tools, the
  discussion and notification tools), are never allowed, so no constraint
  rests on them.
- Every other tool the example allows is proven the same way, with its
  code cited here, before GitHub is enabled (Rollout, step 3), and again
  at every release.

### Never allowed, for GitHub

Tool names at v2.0.3; an operator writing `--tools` leaves every one out:

- merging, auto-merge, updating a branch from its base
  (`merge_pull_request`, `update_pull_request_branch`);
- reviews: writing, submitting, deleting or commenting in one, and
  resolving its threads (`pull_request_review_write`,
  `create_pull_request_review`, `submit_pending_pull_request_review`,
  `delete_pending_pull_request_review`, `add_comment_to_pending_review`,
  `add_pull_request_review_comment`, `resolve_review_thread`,
  `unresolve_review_thread`). Replying in an existing thread,
  `add_reply_to_pull_request_comment`, is a comment, not a review, and may
  be allowed;
- statuses and check runs;
- anything under Actions;
- repository administration, creating, deleting or forking repositories
  (`create_repository`, `delete_repository`, `fork_repository`,
  `create_repository_ruleset`);
- deleting files or branches (`delete_file`);
- Copilot assignments and reviews (`assign_copilot_to_issue`,
  `assign_copilot_to_issue_with_intent`, `request_copilot_review`);
- `search_*`: free-text queries no constraint can narrow.

### Its configuration

```
# /etc/redoma/mcp/github.conf
image = ghcr.io/github/github-mcp-server@sha256:<a release with --token-file>
port = 8082
path = /
tool_check = refuse
arg = http
arg = --listen-host=127.0.0.1
arg = --port=8082
arg = --lockdown-mode
arg = --tools=pull_request_read,list_pull_requests,update_pull_request,add_issue_comment,add_reply_to_pull_request_comment,get_file_contents,list_commits
arg = --token-file=/run/secrets/github/token
token_file = /run/redoma-mcp/github/token
egress = api.github.com
strip_header_prefix = x-mcp-
agent_header.X-MCP-Tools = {tools}
route_for.create_pull_request = github-pr
max_request_bytes = 1048576
```

```
# /etc/redoma/mcp/github/example.conf
[set]
tool = pull_request_read
tool = list_pull_requests
tool = get_file_contents
tool = list_commits
tool = add_issue_comment
tool = add_reply_to_pull_request_comment
tool = create_pull_request
constrain = * arguments.owner = example-org
constrain = * arguments.repo = example-repo
constrain = create_pull_request arguments.head ^= agent/example/
constrain = create_pull_request arguments.base = main
[set]
tool = pull_request_read
tool = get_file_contents
constrain = * arguments.owner = example-org
constrain = * arguments.repo = example-consumer
```

## Layered limits

No single layer is trusted to stop a dangerous call:

1. **The server's own flags, first.** Its static ceiling: GitHub's
   `--tools` list (tool names), Grafana's `--enabled-tools` and
   `--disable-*` with `--disable-write` (categories). A tool outside it is
   not registered at all, whatever the request says. One instance serves
   every agent, so this ceiling is the host's, not an agent's.
2. **Envoy's RBAC, per agent.** The allowlist, with argument constraints.
   It is the only per-agent layer, and the one that does not depend on the
   upstream's code.
3. **The backend, for the upstream's credential.** Grafana's Viewer role
   refuses every write. GitHub's token permissions (no `workflows`, no
   administration), the repositories it is installed on, and the
   repositories' rulesets.

What each upstream's never-allowed tools rely on is stated with its
example above. Where layer 3 cannot refuse something (a GitHub token that
can write pull requests can also approve one), layers 1 and 2 are what stop
it, and each is enough on its own.

### Untrusted text

What an agent reads through an upstream (issues, comments, code, log
lines, dashboard titles) may be written by anyone. Filters such as GitHub's
`--lockdown-mode` reduce what reaches the model; neither they nor this
design make such text safe. They grant or refuse nothing.

## Configuration

Files under `/etc/redoma/mcp/`, all root's and writable by no one else,
like `agents.txt`, and strict: an unknown key, a key given twice (list keys
excepted) or a value that does not parse invalidates the file. `#` starts
a comment only at the start of a line: a value with a `#` after a space or
tab, which reads like a trailing comment, invalidates the file rather than
passing the comment on as part of the value. What an
invalid file does depends on which it is, and none widens access ("An error
never widens access"; see "Who enables what").

### `/etc/redoma/mcp/<upstream>.conf`, the upstream

`<upstream>` follows the agent name rule (lower-case letters, digits and
`-`), and is never an agent's name.

| Key | Meaning |
|---|---|
| `image` | the image, by digest (`<repository>@sha256:<64 hex>`); a tag is refused |
| `port` | the server's port on 127.0.0.1 in its namespace; never one of redoma's own listener ports (3128, 7070, 8080, 8081, 9911) |
| `path` | the server's MCP path; Envoy rewrites `/mcp/<upstream>` to it (default `/`) |
| `arg` (list) | the server's arguments, in order; a secret-named flag must end in `-file`, `-path`, `_file` or `_path`, and none may turn TLS verification off |
| `env` (list) | `NAME=value`; a secret-named variable must end in `_FILE` or `_PATH`, and none may turn TLS verification off |
| `token_file` | the credential's host path, under `/run/redoma-mcp/<upstream>/`; its directory is mounted read-only at `/run/secrets/<upstream>/` |
| `egress` (list) | the hosts on 443 its egress listener allows, in the egress allowlists' syntax (suffix wildcards included) |
| `tool_check` | `refuse` (default), `warn` or `off` |
| `strip_header_prefix` (list) | request header families removed from every agent's requests, any case |
| `strip_header` (list) | exact request header names removed; `authorization` always is |
| `agent_header.<Name>` | a header the route adds per agent; `{tools}` is the agent's tools, comma-separated |
| `route_for.<tool>` | send `tools/call` of that tool to another upstream (an instance with its own credential) |
| `max_request_bytes` | the MCP filter's body limit (default 1 MiB, at most 10 MiB); an agent's listener takes the largest of its upstreams', since a per-route override would replace the filter's whole configuration |

### `/etc/redoma/mcp/<upstream>/<agent>.conf`, an agent's allowlist

And `_default.conf`, every agent's, whose sets come first:

- `tool = <name>`: allowed under the set's constraints;
- `tool_unconstrained = <name>`: allowed with none (a tool may not be
  listed both ways in one set);
- `constrain = <tool or *> <argument path> = <value>`, or `^=` for a
  prefix; `*` means every `tool` of the set, and the path is under
  `arguments.`;
- `[set]` starts a new set; lines before the first `[set]` form a set of
  their own.

A call is allowed when any set allows it. An argument a constraint names
must be present. Every `tool` line must be reached by at least one
`constrain` line of its set (by name, or by a `*` that follows it); a
`tool` line that none reaches, such as one after the set's `constrain = *`
lines, invalidates the file rather than allowing the tool with any
arguments. A tool allowed without constraints is always written
`tool_unconstrained`, an explicit choice.

### Who enables what

- An upstream exists when its `.conf` exists and parses, and its container
  runs and is registered. **An upstream that fails** publishes no egress
  listener and no route for any agent (so `/mcp/<upstream>` is a 404
  everywhere), and the error is logged at every scan. The one exception
  is the tool check's own listener and cluster, which stay published so
  that the check can run again; no agent reaches either.
- **The tool check.** When an upstream registers, and when `redoma xds`
  starts, it asks the upstream for its tools (`initialize`, then
  `tools/list`, following its cursor) and compares them with every tool
  name its configuration names: the `.conf`'s `route_for` tools,
  `_default.conf`'s and each agent file's `tool` and `tool_unconstrained`
  lines. A name the server does not list is a mismatch. A tool the server
  lists and no file names is reported too (logged, never an error): after
  an upgrade it shows what a category ceiling such as Grafana's registered
  without anyone choosing it. The answer is kept for the container's life.
  The upstream's `tool_check` decides what a mismatch does:
  - `refuse` (the default): the file that names the tool is invalid, with
    the effect any invalid file of its kind has: in the `.conf` it fails
    the upstream, in `_default.conf` every agent's list for the upstream,
    in an agent's file that agent's list alone. Under `refuse`, an upstream
    that cannot be asked fails, and is asked again at the next scan;
  - `warn`: the mismatch is logged, and everything is published as if
    there were none (the call fails at the server instead);
  - `off`: no check, and no request is made.

  The question goes through Envoy, not to the server directly: per
  upstream, `redoma xds` publishes a check listener on a unix socket in
  `/run/redoma/xds`, routed to the upstream's cluster and allowing only
  the lifecycle methods and `tools/list`. It sends no header but the
  protocol's, so the server lists its whole static ceiling.
- An agent has an upstream when its own file under that upstream exists.
  Without it, `/mcp/<upstream>` is a 404 for that agent.
- The route stays published for as long as the agent is, between turns
  too. Envoy cannot tell a turn's call from another process's in the
  container, so outside a turn the agent can still call its allowed tools.
- An agent file that exists but does not parse still publishes the agent's
  route, with an RBAC policy that allows nothing: every call is a 403, and
  the error is logged, so the agent's failure says "denied", not "no such
  server". An invalid `_default.conf` does that to every agent with a file.
- `_default.conf` and any other underscore name can never be an agent's
  (`allowlist.ValidName` refuses the underscore).
- The host switch is the presence of `/etc/redoma/mcp/`; nothing in it,
  nothing published.

`redoma xds` turns these into Envoy configuration only, and never reads a
credential: it contacts an upstream only for the tool check, through
Envoy.

### The runtime layout

| Path | Owner, mode | Holds |
|---|---|---|
| `/run/redoma-mcp/<upstream>/` | root, 0700 | the upstream's credential, `token`, copied by `redoma-mcp-token@<upstream>.service` |
| `/run/netns/redoma-mcp-<container ID>` | the hook | each upstream container's namespace, as for agents |
| `/run/redoma/xds/mcp-check-<upstream>.sock` | root (Envoy binds it), 0666, in `redoma-sds`'s 0750 directory | the tool check's listener, which only `redoma xds` and root reach |

`/run/redoma-mcp` is outside `/run/redoma` because the hook refuses every
mount from `/run/redoma`, for agents and upstreams alike. An upstream's
container gets exactly one host directory, its credential's, read-only, and
no workspace and no host socket.

**The check socket's access is its directory's.** Envoy binds the
listener's socket (a `Pipe` address, which has only a `path` and a `mode`,
`api/envoy/config/core/v3/address.proto:23-34`). It runs as root, so the
socket is root's; after the bind it applies `mode` with a `chmod` and
changes no owner (`source/common/network/socket_impl.cc:73-79`). The
listener's mode is `0666`, and what limits it is `/run/redoma/xds`, `0750`
`redoma-sds`: only uid 961, members of `redoma-sds` (the host Envoy) and
root reach a path in it, the same principals that reach `xds.sock`.

### Users

Each upstream container runs rootful with Podman's `--userns=auto`: a user
namespace of its own, mapped to a range of unprivileged host uids and gids
that no other `--userns=auto` container holds, allocated from the
subordinate ids of the user `containers` (podman-run(1) of Podman 4.9,
`--userns=auto`). So:

- **Install** needs a `containers` entry in both `/etc/subuid` and
  `/etc/subgid`, with a range that holds no host uid or gid in use: not
  root's, not redoma's services' (961, 963, 964), not any agent's.
- **The credential across the user namespace.** The token is a host-root
  0600 file. The unit mounts the directory with `:idmap`, so the file reads
  as the container root's, and runs the server as that root, `--user=0:0`,
  whatever user its image names (mcp-grafana's image sets `USER` to uid
  1000, which could not read the token). Inside `--userns=auto` that root
  is an unprivileged host uid, with no capability. If a host's kernel or
  Podman cannot idmap that
  mount, the fallback is a fixed `--userns` mapping for upstreams and a
  copier that `chown`s to the mapped uid.
- **The hook** refuses an agent whose uid, gid or any `additionalGids`
  entry falls in the `containers` ranges, so no agent shares an id with an
  upstream's process. For an upstream it requires the opposite: a `user`
  namespace entry whose mappings lie wholly inside those ranges and map no
  host id 0.
- **`--network none` is proven differently.** For an agent the hook reads
  "a network namespace with no path and no user namespace" as proof of
  `--network none`; with a user namespace, Podman creates the network
  namespace without a path for any network mode and attaches the network
  after the runtime made it, so that proof does not hold. For an upstream,
  the hook mounts the namespace at createRuntime as for an agent, but
  registers it with `redoma xds` only from its poststart hook, once Podman
  has configured the network, and only when the namespace has no interface
  but `lo`. An upstream not registered gets no cluster, no egress listener
  and no requests (open question 1).

## Upgrading an upstream

An upgrade is a digest change of `image` in the upstream's `.conf`, and a
restart of its unit. The tool check then compares the new release's tools
with the allowlists, so a tool renamed or removed is caught at start, and a
tool newly registered is reported. What a tool list cannot show stays a
written checklist, gone through before the digest moves:

- the release's flags, and whether its static ceiling still bounds every
  request header;
- whether it still reads its credential from the file, re-reads it, and
  never prefers a request's credential once the stripped headers are gone;
- the request headers it reads, and any new family the `.conf` must strip
  (a new prefixed header is covered by its prefix, a new unprefixed one is
  not), and whether it now routes or authorizes on a header such as
  `Mcp-Param-*` rather than the body;
- the parser its dispatch and each allowed tool's argument decoding use
  (case, duplicate keys) against Envoy's, for every argument a constraint
  names ("The rule that follows");
- that it ignores a body on GET and DELETE, and any query string;
- that its HTTP mode is still stateless, where `route_for` sends a tool
  to another instance;
- that it verifies its backend's TLS certificate, against which roots,
  and which flags or variables turn that off or change the roots;
- whether a flag takes a secret under a name the `arg` check does not
  catch;
- what a renamed or new tool does, before any allowlist names it.

## Pushing

GitHub's server cannot push an agent's local commits; its write paths are
the contents and Git Data APIs (`push_files`, `create_or_update_file`,
`delete_file`, `create_branch`). Publishing through them needs no redoma
code, but each call is one commit authored by the upstream's credential,
made from file contents the model writes out as arguments, and the paths
of `push_files` sit in a list Envoy cannot constrain. Publishing the
agent's own commits would need a host push, `redoma git-push`, as another
upstream in the same pattern: it reads the agent's workspace as data, never
running git there, checks every commit (author, sign-off, protected paths,
sizes), and pushes fast-forward only to a branch under the agent's prefix.
It is built only when hosts need the agents' own commits published.

## Network

- **Every tool an agent uses goes through an MCP upstream.** An upstream's
  backend hosts are in the upstream's own egress listener, scoped to its
  namespace, never in an agent's egress allowlist.
- **Fetching repositories** is a read-only git upstream too, once there is
  one (open question 2). Until then, an agent that needs a clone or fetch
  keeps its git hosts in its egress allowlist.

## Audit

Envoy's access log on the MCP listeners is the audit record, one JSON line
per request: the listener (the agent); the request's method, path and
request id; the cluster it reached (which upstream instance, under
`route_for`); the MCP filter's whole metadata (the MCP `method`,
`params.name` and every extracted argument); the response code and its
details (`rbac_access_denied_matched_policy[none]` when RBAC refused it,
which tells that 403 from one the server sent); flags, duration and
bytes. Bodies are not logged. The backend's own audit log names the
upstream's credential for each call.

## Rollout

1. The generic pieces: the configuration parsers; the MCP listener, RBAC,
   routes, clusters, egress listeners and the tool check in `redoma xds`;
   the hook's upstream registration; `redoma-mcp@.service` and
   `redoma-mcp-token@.{path,service}`; the shim's `--mcp-upstream`.
2. Grafana as the first upstream: read tools, for one agent.
3. GitHub, once its HTTP mode holds a token file and every argument its
   allowlists constrain is proven case-exact ("Its argument constraints,
   proven per tool"): read tools first, then writes, then `github-pr`.
4. Publishing, and a read-only git upstream (open question 2).

**Migrating an agent from an in-container credential:**
1. Write its allowlist file.
2. Start its shim with `--mcp-upstream=<name>`.
3. Tell it, in its instructions, to use the tools.
4. Remove the credential from its environment and the service's hosts from
   `egress/<name>.txt`.
5. Restart it.

## Tests

Under `bazel test //...`, with no network:

- the configuration parsers, every rule and refusal;
- the generated Envoy resources: per agent, the filter order
  (`header_mutation`, MCP, RBAC, router), the MCP filter's modes, the RBAC
  policy (an allowlist, OR-ed sets, no tool or method outside the files),
  the routes and the stripped headers; per upstream, the egress listener
  and the cluster's source namespace;
- the hook's registration of upstreams and its refusals;
- the tool check against a fake upstream, under each `tool_check` value;
- an integration test (`xds/internal/mcpe2e`) that runs an agent's MCP
  listener in Envoy v1.40.2 in front of Grafana's MCP server v2.2.0, both
  the release binaries pinned by sha256 in `MODULE.bazel`, with a fake
  Grafana behind it. It checks that the configured token is the one Grafana
  sees whatever the agent sends, and a rotated token without a restart;
  that `x-grafana-*` and `authorization` from the agent never reach the
  server; that a tool outside the agent's allowlist, or outside a
  constraint, or a method outside the lifecycle, is refused before the
  server; that a body that is not MCP, or has a duplicate key, is refused;
  that no write tool is registered under `--disable-write`; and that a
  case-variant argument key still reaches past a constraint (the known
  limit above). It runs on the host's loopback, without namespaces.

## Open questions

1. **Proving `--network none` for an upstream.** To be checked on Podman
   4.9 with `--userns=auto`: that poststart runs after the network is
   configured, that a poststart failure leaves the container unregistered,
   and that nothing attaches a network later.
2. **How a clone or fetch is served.** A clone is a pack written into the
   agent's workspace, which a tool's result does not do on its own. Is it
   an existing MCP server's tools, one added as an upstream, or reads at a
   ref only, with no local clone at all?

### Decided

- **The upstream holds its own credential,** from a file it re-reads.
  Envoy never injects one, and no credential is chosen per request.
- **One Envoy** for MCP, model, egress and relay listeners; a crash of
  the alpha MCP filter takes every listener down, an accepted risk.
- **No `ext_proc` fallback.** A bug in the MCP filter is fixed in Envoy.
- **CI secrets on same-repository branches** are a deployment's hardening
  choice, out of redoma's scope. The threat: a pull request from a branch
  of the same repository runs the base repository's workflows, with its
  secrets, on the branch's code, and refusing changes to workflow files
  does not stop changes to the scripts and tests those workflows run.
  Publishing to a fork instead makes CI treat it as a fork's, without
  secrets.
- **Routes are published whenever the agent is,** not only during a turn.
- **Uids come from `--userns=auto`**, with the credential directory
  idmapped into it.
- **One upstream instance per credential,** shared by every agent that has
  it; the backend enforces the union of their access. A second identity is
  a second instance, routed by tool (`route_for`).
- **A per-agent credential is not needed:** with one instance per
  credential it would mean an instance per agent.
- **No direct backend egress for agents:** every tool goes through an MCP
  upstream.
- **Upstream images are checked at start** against the allowlists,
  `tool_check = refuse | warn | off`, `refuse` by default; tools no file
  names are reported; behaviour changes stay a checklist step.
- **Grafana first,** read-only, needing no upstream change; GitHub when its
  HTTP mode holds a token file.
