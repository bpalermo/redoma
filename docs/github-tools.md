# Upstream MCP servers behind Envoy, and GitHub as the first

Status: proposal, nothing implemented. It proposes a pattern for giving
agents the tools of an existing MCP server (GitHub's, Grafana's, any
other) without putting the server's credential, or a route to its
backend, in the container. The host runs the server, Envoy proxies it,
and Envoy holds both the per-agent tool allowlist and the credential.
GitHub's official MCP server is the first instance.

A new server should need only a block of configuration: an image digest,
its flags, a credential, its egress, and each agent's allowlist. It should
never need new redoma code. What redoma adds is generic and written once.

## Why

An agent that works on a repository needs to publish its work: open a pull
request, answer review comments, read the checks. Today the only way to do
that from a redoma container is to give it what a person would use: a
token in its environment, `gh`, and the GitHub hosts in its egress
allowlist. That undoes what redoma does for the model credential, which
never enters the container (Envoy injects it on two paths only):

- **The container holds the credential.** Anything the turn runs can read
  it and use it for anything its scopes allow, for as long as it is valid.
- **Policy is GitHub's alone.** A token that can open a pull request can,
  with the wrong scopes, merge, approve, change workflows or delete
  branches. Nothing on the host can say "this agent, these tools".
- **Nothing on the host sees what was done.** The egress proxy sees a
  CONNECT to `api.github.com`, not which pull request was opened.

The same holds for every other service an agent might use through MCP.
Writing redoma-native tools for each would not scale, and each would be
code to audit. Existing MCP servers already implement the tools. What is
missing is a way to run them on the host, limit them per agent, and keep
their credentials out of the container.

## The pattern

```
agent container (own netns)                   host
  claude --mcp-config: http://127.0.0.1:8081/mcp/<upstream>
     |
     v
  127.0.0.1:8081 agent_<name>_mcp  -- host Envoy --------------------------+
     http_filters, per agent:                                              |
       header_mutation (remove the x-mcp-* headers the agent sent)         |
       mcp (parse JSON-RPC: method, params.name, configured arguments)     |
       rbac (this agent's allowlist; anything else is a 403)               |
       router (route on the tool name: cluster per credential)             |
                                                                           v
                                     cluster mcp_<upstream>[_<credential>]
                                       upstream filter: credential_injector
                                       (the upstream's token, from an SDS file)
                                       source address in the upstream's netns
                                                                           |
upstream container (own netns, --network none)                             |
  127.0.0.1:<port> the MCP server, HTTP mode  <----------------------------+
  HTTPS_PROXY=127.0.0.1:3128 -> Envoy egress listener -> the upstream's backend hosts
```

- **The agent's side.** A fourth per-agent listener, `agent_<name>_mcp`
  on 127.0.0.1:8081 in the agent's namespace, serves every upstream under
  `/mcp/<upstream>` (MCP's Streamable HTTP, HTTP/1.1 or HTTP/2). claude
  reaches it through an `http` entry of the shim's `--mcp-config`, one per
  upstream the agent has. As with the model and egress listeners, the
  listener the connection arrives on is the agent's identity. The agent
  speaks plain HTTP to its own loopback; no SVID is presented on this hop,
  and none needs to be.
- **The upstream's side.** Each upstream server runs in a rootful Podman
  container of its own, `--network none`, in its own network namespace
  like an agent, and in a user namespace of its own (`--userns=auto`).
  Envoy binds an egress listener in that namespace, holding
  the upstream's own allowlist (GitHub's server needs `api.github.com`).
  Envoy reaches the server's port on that namespace's loopback through a
  cluster whose source address is in that namespace. Nothing else can
  reach the server: it has no network but Envoy's, so it authenticates no
  one and needs no SVID.
- **Between them**, Envoy parses each MCP request, refuses any method or
  tool not on the agent's allowlist, picks the credential by tool, and
  injects it. The container never holds a credential, and the upstream
  server holds one only for the duration of a request.

### What redoma adds, once

- `redoma xds` publishes, per agent with any upstream: the MCP listener,
  its RBAC policy, and its routes. Per upstream, it publishes the egress
  listener in the upstream's namespace and the clusters with their upstream
  credential injectors. All of it is built from the configuration below,
  re-read at every scan like the egress allowlists. The host bootstrap
  (`examples/envoy.yaml`) does not change: everything comes over LDS and
  CDS.
- The OCI hook registers an upstream container (annotation
  `redoma/upstream=<name>`) much as it registers an agent. At createRuntime
  it mounts the namespace at `/run/netns/redoma-mcp-<container ID>` and
  applies the same process and mount checks. The user and network namespace checks differ, because an
  upstream runs with `--userns=auto` (see "Users"): the hook registers it
  with `redoma xds`, under its upstream name, only from its poststart
  stage, once the namespace has no interface but `lo`. That needs a new
  hook definition in `ocihook/hooks.d`, a `poststart` stage matching the
  annotation `redoma/upstream`, beside the existing createRuntime and
  poststop ones. An upstream name is never an agent name: a container
  carrying both annotations is refused, and the two namespaces of names
  are kept apart in `agent.Merge`.
- The shim gains `--mcp-upstream=<name>` (repeatable): an `http` entry in
  its `--mcp-config` and `mcp__<name>__*` in `--allowedTools`. This only
  decides what claude is offered; it grants nothing.
- A systemd template unit, `redoma-mcp@.service`, runs one upstream
  container from its configuration file (image by digest, arguments), with
  `--userns=auto`, so starting a new upstream is `systemctl enable --now
  redoma-mcp@<name>`. One instance of each upstream serves every agent
  that has it; there is no per-agent instance.

## What was verified

Against the Envoy API that redoma pins (`github.com/envoyproxy/go-control-plane/envoy`
v1.40.0, the API of Envoy v1.40.0), and against `github/github-mcp-server`
v2.0.3 (commit `b98d115bbb080e9555466a96656ec4df605aea21`). Paths are in
those trees. The host must run an Envoy that has these extensions; v1.40.0
or later.

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
  `params.arguments.owner`, can be added per method with `parser_config`
  extraction rules (`mcp.proto:177-200`). Arrays are skipped by the parser
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
  - `clear_route_cache: true` lets a route match on the extracted metadata
    (`mcp.proto:86-89`), which is how a tool picks its credential below.
- **A parser differential, checked.** Envoy reads `params.name` with exact
  case. The server's own pre-parse decodes into a Go struct with
  `encoding/json`, which matches field names without regard to case
  (`pkg/http/middleware/mcp_parse.go:16-32` in the server). That pre-parse
  only narrows which tools are registered for the request, though. The
  dispatch is done by the MCP Go SDK (v1.8.0), which decodes with case
  matching on (`internal/json/json.go:30-33` in go-sdk,
  `DontMatchCaseInsensitiveStructFields`). So a `"Name"` key next to
  `"name"` makes the call fail; it cannot make the server call a tool other
  than the one Envoy checked. This holds for this server and SDK version
  only. Each upstream's parser is part of its trust, which is one reason
  the server's own limits stay the first layer (see "Layered limits").
- **Risk, stated plainly.** A bug in this filter could do one of two things:
  - **Pass a call RBAC should have refused.** The server's own ceiling and
    GitHub's permissions still hold.
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
  and egress listeners rely on.
- **The policy is an allowlist.** One policy, `ALLOW`, whose permissions
  are, for each upstream route of the agent:
  - the lifecycle methods (`initialize`, `notifications/initialized`,
    `ping`, `tools/list`);
  - `tools/call` with `params.name` among the agent's tools, each with its
    argument constraints if any (below).

  Anything that matches no permission is a 403: another method
  (`resources/*`, `prompts/*`, `sampling/*`), another tool, an unparsed
  body. The filter's `REJECT_NO_MCP` mode has already refused anything that
  is not MCP.
- **Argument constraints.** An extraction rule makes an argument visible to
  RBAC, so a tool can be allowed only with, say, `owner` = `example-org`
  and `repo` = `example-repo`, or a `branch` that starts with
  `agent/example/`. They are exact or prefix string matches on scalar
  arguments. A list argument cannot be constrained (the parser skips
  arrays), so a tool whose target sits in a list (the paths in
  `push_files`, for example) is constrained only by the upstream and the
  backend.
- **What RBAC cannot check** is anything that needs the backend's state.
  Envoy sees the arguments, not what they point at. A constraint on
  `owner` and `repo` keeps a comment in the allowed repositories. It cannot
  tell whether a pull request number is one of the agent's own, or whether
  a review comment id belongs to the pull request named. So, within an
  allowed repository, an agent may comment on or reply in any pull request
  or issue. "Its own pull requests only" is not expressible in this design:
  it would need a lookup at the backend, which nothing on the path does.
  Narrowing by credential does not help either (see "One credential per
  upstream").

### The credential (`credential_injector`)

- `envoy.filters.http.credential_injector` with the generic credential
  (`envoy.http.injected_credentials.generic`) reads an SDS generic secret
  and sets it as a header: `Authorization` by default, with a
  `header_value_prefix` such as `Bearer `
  (`api/envoy/extensions/http/injected_credentials/generic/v3/generic.proto:24-41`).
  With `overwrite: true` it replaces whatever the client sent
  (`api/envoy/extensions/filters/http/credential_injector/v3/credential_injector.proto:72-74`).
  redoma's model listeners already use exactly this, from the SDS file
  `/etc/redoma/secrets/anthropic-key.yaml` (`xds/internal/lds/lds.go:167-175`).
- Both are alpha, `security_posture: unknown`
  (`extensions_metadata.yaml:487-493`, `:1295-1299`). They see only the
  request's headers and the secret, not the body, and redoma already runs
  them on the model listeners. The credential injector is also supported
  as an **upstream** filter (`status_upstream: alpha`, `:493`). That puts
  the credential on the cluster, not on the agent's listener: every route
  to `mcp_github` gets the App token, and the one route to `mcp_github_pr`,
  which matches `create_pull_request` alone, gets the person's token (see
  "The person's credential").
- **Rotation.** An SDS secret from a file (`PathConfigSource`) is reloaded
  when the file is moved into place: Envoy watches the path for moves, or a
  `watched_directory` for moves into it
  (`api/envoy/config/core/v3/config_source.proto:159-196`). A one-hour App
  installation token rotates by its minter writing the new SDS file
  aside and renaming it over the old one, well before expiry (at half its
  life). Envoy picks it up without a restart, and in-flight requests keep
  the token they were sent with. redoma never reads the file. An expired
  or missing token is GitHub's 401, returned to the agent as the tool's
  error; there is no fallback to another credential.
- **One credential per upstream.** Each credential an upstream's `.conf`
  names (`default`, and `pr` for GitHub) is one per redoma install, shared
  by every agent that uses the upstream. The trade-off: GitHub enforces
  only the union of those agents' repositories, and each agent's own
  repositories are held by Envoy's RBAC alone.

### Reaching a server in another namespace

`SocketAddress.network_namespace_filepath` makes Envoy create the socket in
that namespace (`api/envoy/config/core/v3/address.proto:100-110`). For a
cluster, it applies through the source address of the cluster's bind
config (`:184-197`). So the cluster `mcp_github` connects to
127.0.0.1:8082 from a source address in `/run/netns/redoma-mcp-<id>`, and
reaches the server's loopback.

### GitHub's MCP server in HTTP mode

- **Per-request token.** `github-mcp-server http` takes the token from each
  request's `Authorization` header (`pkg/http/middleware/token.go:13-47`)
  and needs none at start. A request without one gets a 401 challenge.
  Its HTTP mode is stateless: a new server per request
  (`pkg/http/handler.go:260-261`). So any request can go to any instance,
  under any credential.
- **Flags** (`cmd/github-mcp-server/main.go:243-255`, and `:271-277` for
  `http`'s own):
  - `--toolsets`, `--tools`, `--exclude-tools`, `--read-only`,
    `--lockdown-mode`;
  - `--port` (8082 by default) and `--listen-host`.

  Toolsets at this version: `context actions code_quality code_security
  copilot copilot_issue_intents dependabot discussions gists git governance
  issues labels notifications orgs projects pull_requests repos
  secret_protection security_advisories stargazers`.
- **Request headers can change the tool set, and the flags bound them
  only when one is set.** HTTP mode reads `X-MCP-Toolsets`, `X-MCP-Tools`,
  `X-MCP-Readonly`, `X-MCP-Exclude-Tools`, `X-MCP-Lockdown`,
  `X-MCP-Insiders` and `X-MCP-Features` from each request
  (`pkg/http/middleware/request_config.go:16-64`).
  - With any of `--toolsets`, `--tools`, `--exclude-tools` or `--read-only`
    set, the flags are a ceiling and the headers can only narrow it
    (`pkg/http/handler.go:302-351`, `hasStaticConfig` at `:376-381`).
  - With none set, a client's `X-MCP-Toolsets: all` reaches every tool.

  So redoma requires a static `--tools` list for this server, strips every
  `X-MCP-*` header the agent sends, and may set `X-MCP-Tools` itself per
  agent (below).
- **Which headers are stripped, and how.** A route's
  `request_headers_to_remove` takes exact names only
  (`api/envoy/config/route/v3/route_components.proto:374-376`), so it cannot
  remove a family of headers. The agent's listener therefore runs
  `envoy.filters.http.header_mutation` before the router, with one
  `remove_on_match` request mutation per prefix: a `key_matcher` string
  matcher, prefix, `ignore_case`
  (`api/envoy/config/common/mutation_rules/v3/mutation_rules.proto:93-111`,
  `api/envoy/extensions/filters/http/header_mutation/v3/header_mutation.proto:22-24`).
  That filter is alpha, `security_posture: unknown`
  (`extensions_metadata.yaml:835-841`), like the credential injector, and
  sees only headers. Removed: `x-mcp-*` only, the server's per-request
  configuration above, which only the operator may set. The per-agent
  `X-MCP-Tools` is added after the removal, by the route (the router
  applies a route's header additions after the listener's filters have
  run).

  Kept, because the protocol needs them:
  - `Mcp-Session-Id` and `MCP-Protocol-Version`.
  - `Mcp-Param-*`. The server annotates the `owner` and `repo` arguments of
    its tools (`HeaderParams`, `pkg/inventory/server_tool.go:276-289`). From
    protocol version `2026-07-28` (`minVersionForStandardHeaders`,
    `mcp/streamable_headers.go:30` in go-sdk v1.8.0), a client sends them as
    `Mcp-Param-owner` and `Mcp-Param-repo` headers (the SDK's own client
    does, `:198-260`). The SDK's server refuses, with a 400, a `tools/call`
    that lacks one of them or whose value differs from the body's argument
    (`mcp/streamable.go:1616-1631`, `mcp/streamable_headers.go:359-363` and
    `:402-442`).
    Envoy's MCP filter does not read these headers; it checks the body.
    - **On protocol `2026-07-28`**, the SDK checks that the headers agree
      with the body, so a header cannot carry a value other than the one
      RBAC allowed. Stripping them would break every repository tool on
      that version.
    - **On an earlier version**, which the client is free to pick, nothing
      checks them: the SDK skips the check below `2026-07-28`
      (`mcp/streamable_headers.go:359-363`), and Envoy never reads them.
      They are still harmless at v2.0.3, because nothing acts on them
      either. Dispatch and its arguments come from the body, the same body
      RBAC checked. The server reads `Mcp-Param-*` only to list them as
      allowed CORS headers (`pkg/http/middleware/cors.go:18-30`, `:66`).

    That safety belongs to this server version, not to the pattern. A
    release that routed or authorized on `Mcp-Param-*` would let a client on
    an older protocol send a header that disagrees with the body. Checking
    that is a step of the upgrade checklist (see "Upgrading an upstream").
    - Pinning `MCP-Protocol-Version` to `2026-07-28` in RBAC would close
      the gap generically, but at a cost. Every client that does not speak
      that version yet would be refused, including the agent's Claude Code
      if it negotiates an earlier one, and so would `initialize`, which
      carries no such header. The doc does not pin it. Pinning becomes an
      option once the agents' client speaks `2026-07-28`.
  - `Mcp-Method` and `Mcp-Name`. With protocol version `2026-07-28` the MCP
    filter requires them and refuses a request whose headers disagree with
    its body (`source/extensions/filters/http/mcp/mcp_filter.cc:422-425`,
    `:494-540`, `:798-828`). With an earlier version and `BODY`, the filter
    ignores them, and the server reads only `Mcp-Method`, to refuse one
    method (`pkg/http/handler.go:249`).
- **Image:** `ghcr.io/github/github-mcp-server:v2.0.3`, an OCI index,
  `sha256:250bc6ce70788207608ef74cef4397230ff2e8481218da74d6093e4bd9101132`
  (linux/amd64
  `sha256:cecac9a377c81f5ebfd20a9aabc7b9ff6a3a92393af6aa4b75875fe884e037b9`,
  linux/arm64
  `sha256:be672d50a0de2c6f9c4b81527828ad1b81fb4108b33f98b5876c2abc8d4ca488`).
  Its config runs as user 0 with entrypoint `/server/github-mcp-server`.
  Under `--userns=auto` (see "Users") that root is an unprivileged host uid
  of its own; the unit still gives it a non-root `--user` inside its
  namespace. Its HTTP client is Go's default transport, which honours
  `HTTPS_PROXY`.

## Layered limits

No single layer is trusted to stop a dangerous call. What each layer stops
on its own:

- **A merge.** Layers 1 and 2 each stop it on both routes:
  `merge_pull_request` is not registered, and RBAC does not allow it.
  - At GitHub, the person's route is safe because its token has Contents
    read only.
  - On the App's route, GitHub refuses a merge only while the pull request
    lacks an approving review or a required check. The App's contents and
    pull requests write would let it merge a pull request that is approved
    and green. So, for an approved and green pull request, GitHub is no
    backstop there, and layers 1 and 2 are what stop the merge.
- **An approval.** Layers 1 and 2 each stop it on both routes. At GitHub,
  neither credential's permissions do: Pull requests write allows
  submitting a review, so GitHub is no backstop against an approval on
  either route. On the person's route the approval would also carry a
  person's name (see "The person's credential" below).

1. **The server's own flags, first.** GitHub's server is started with an
   explicit `--tools` list, the union of what any agent on the host may
   call, and `--lockdown-mode` (see "Untrusted text"). Toolsets are not
   used: they are coarse. `repos` holds `delete_repository`,
   `create_repository` and `fork_repository` next to `get_file_contents`;
   `pull_requests` holds `merge_pull_request`, `update_pull_request_branch`
   and `pull_request_review_write` next to `create_pull_request`. A tool
   that is not in the static universe is not registered at all, whatever
   the request says. Envoy also sets `X-MCP-Tools` per agent to that
   agent's list, so `tools/list` shows each agent only its own tools.
   One instance of the server serves every agent (it is stateless), so
   this ceiling is the host's, not an agent's: an agent's own limit is
   layer 2.
2. **Envoy's RBAC, per agent.** The allowlist above, with argument
   constraints. It is the only per-agent layer, and the one that does not
   depend on the upstream's code.
3. **GitHub, as the backstop.**
   - The App's permissions: contents, pull requests and issues write;
     checks, statuses and metadata read. Never `workflows`, administration,
     secrets or environments. GitHub refuses a change to
     `.github/workflows` from a token without `workflows`, through git or
     the API.
   - Repository rulesets that let the App create and update only branches
     under the agents' prefixes, and block deletion and force-push.
   - A ruleset on the default branch requiring an approving review and the
     required checks, with neither the App nor the person's token in its
     bypass list. It stops a merge at GitHub until a pull request is
     approved and green, and no longer (see above).
   - The App installed on the agents' repositories only.

### The person's credential

`create_pull_request` is sent with a person's token, not the App's: the
pull request's author is whoever opens it. Its squash commit on the
default branch is then authored by that person, so the published history
names someone who answers for it, with the agent's commits as its contents.

- **Its scope:** a fine-grained token on the agents' repositories only,
  with Pull requests read and write, Contents read and Metadata read.
  Nothing else.
- **What GitHub stops on it:**
  - Contents read means it cannot push, change files or merge (a merge
    writes contents). The default branch's ruleset stops a merge too.
  - It **can** do anything Pull requests write allows as that person:
    comment, edit or close pull requests, and approve or request changes
    on pull requests other people opened. GitHub refuses an approval only
    of the person's own pull requests.

  So on this route GitHub is no backstop against an approval in that
  person's name.
- **So the route is narrowed by its match, not only by RBAC.** Envoy routes
  to `mcp_github_pr` only a request whose extracted metadata is `method` =
  `tools/call` and `params.name` = `create_pull_request`, both exact. A
  route's `dynamic_metadata` matchers must all match
  (`api/envoy/config/route/v3/route_components.proto:778-782`), after the
  MCP filter has run with `clear_route_cache`. Every other request,
  `tools/list` included, goes to `mcp_github` under the App's token.
  - In the configuration, a credential other than `default` is given to
    tools one tool per line (`credential_for.<tool>`), never a pattern. The
    upstream's own `.conf` lists the only tools that may be sent with that
    credential (`credential_allows.<credential>`): for GitHub's `pr`,
    `create_pull_request` alone. **The credential rule**, which makes the
    upstream's `.conf` invalid when any part fails:
    - every credential other than `default` that the file defines
      (`credential.<name>`) has a `credential_allows.<name>`, whether or not
      a `credential_for` line uses it;
    - every `credential_for` line names a credential the file defines, and
      a tool in that credential's `credential_allows`;
    - every `credential_allows` names a credential the file defines.

    An allow list, not a deny list. A deny list would have to name every
    review, comment and merge tool, and at v2.0.3 there are many (those
    under "Never allowed at any layer", plus comment tools such as
    `update_issue_comment`). It would also fail open on the next release's
    new tool. The check is generic: redoma knows no upstream's tools, and
    the list lives with the upstream's configuration.
  - RBAC and the server's `--tools` ceiling still apply on top. If a
    request names `pull_request_review_write`, it never reaches the person's
    token: it is not routed there, the server has not registered it, and
    RBAC has not allowed it. Each of these is enough on its own.

Never allowed at any layer, for GitHub (tool names at v2.0.3):

- merging, auto-merge, updating a branch from its base
  (`merge_pull_request`, `update_pull_request_branch`);
- reviews: writing, submitting, deleting or commenting in one, and
  resolving its threads (`pull_request_review_write`,
  `create_pull_request_review`, `submit_pending_pull_request_review`,
  `delete_pending_pull_request_review`, `add_comment_to_pending_review`,
  `add_pull_request_review_comment`, `resolve_review_thread`,
  `unresolve_review_thread`). Replying in an existing thread,
  `add_reply_to_pull_request_comment`, is a comment, not a review, and
  may be allowed;
- statuses and check runs;
- anything under Actions;
- repository administration, deleting or creating repositories, forks
  (`create_repository`, `delete_repository`, `fork_repository`,
  `create_repository_ruleset`);
- deleting files or branches (`delete_file`);
- Copilot assignments and reviews (`assign_copilot_to_issue`,
  `assign_copilot_to_issue_with_intent`, `request_copilot_review`).

### Untrusted text

Agents may read public repositories, other owners' included, and what they
read (issues, comments, code) is written by anyone. GitHub's server runs
with `--lockdown-mode`, which filters content from authors without push
access on public repositories. GitHub calls it best-effort, not an
authorization boundary, and so does this design: it reduces what reaches
the model, and grants or refuses nothing. An operator who wants an agent to
read only some repositories narrows its allowlist with `owner` and `repo`
constraints (`constrain = * arguments.owner = ...`); nothing refuses public
repositories by default.

### CI on same-repository branches

A pull request from a branch of the same repository runs the base
repository's workflows with its secrets and a token that may write, and
with the branch's own code: build scripts and tests run as CI. Refusing
`.github/workflows` changes (the App has no `workflows` permission) does
not stop changes to what those workflows execute, so an agent's branch can
reach the CI secrets. Publishing to a fork and opening the pull request
across repositories would make CI treat it as a fork's, without secrets.
That is a hardening choice for each deployment (how its repositories and
CI are set up), not something redoma requires or provides.

## Configuration

Two files per upstream, plus one per agent per upstream, in the style of
the egress allowlists. All of them are root's and writable by no one else,
like `agents.txt`, and strict: an unknown key, a key given twice (list keys
excepted) or a value that does not parse invalidates the file. What an
invalid file does depends on which it is, and none widens access ("An error
never widens access"; see "Who enables what").

`/etc/redoma/mcp/<upstream>.conf`, the upstream:

```
# /etc/redoma/mcp/github.conf
image = ghcr.io/github/github-mcp-server@sha256:250bc6ce70788207608ef74cef4397230ff2e8481218da74d6093e4bd9101132
port = 8082
# What a tool named in an allowlist but missing from the server's tools/list
# does: refuse (the default), warn or off. See "Who enables what".
tool_check = refuse
# The server's arguments, one per line, in order.
arg = http
arg = --listen-host=127.0.0.1
arg = --port=8082
arg = --lockdown-mode
arg = --tools=get_me,pull_request_read,list_pull_requests,create_pull_request,update_pull_request,add_issue_comment,add_reply_to_pull_request_comment,create_branch,push_files,get_file_contents,list_commits
# Hosts its egress listener allows (port 443).
egress = api.github.com
egress = raw.githubusercontent.com
# Credentials: an SDS file each, the header and prefix they are sent with.
credential.default = /run/redoma/mcp-credentials/github-app.yaml
credential.pr = /run/redoma/mcp-credentials/github-pr.yaml
credential_header = Authorization
credential_prefix = Bearer
# Which tools use a credential other than default, one tool per line.
credential_for.create_pull_request = pr
# The only tools a credential other than default may carry (a list key, one
# tool per line). See "The credential rule" for what makes the file invalid.
credential_allows.pr = create_pull_request
# Request header name prefixes removed from the agent's requests (any case).
strip_header_prefix = x-mcp-
# Optional: a header set per agent, {tools} being the agent's list.
agent_header.X-MCP-Tools = {tools}
max_request_bytes = 1048576
```

`/etc/redoma/mcp/<upstream>/<agent>.conf`, an agent's allowlist (and
`_default.conf`, every agent's, read first):

```
# /etc/redoma/mcp/github/example.conf
# Tools that take no repository and so no constraint (the * lines below
# do not apply to them; a tool may not be listed both ways).
tool_unconstrained = get_me
tool = pull_request_read
tool = list_pull_requests
tool = create_pull_request
tool = add_issue_comment
tool = add_reply_to_pull_request_comment
tool = create_branch
tool = push_files
# Constraints: <tool or *> <argument path> = <value>, or ^= for a prefix.
# * means every tool listed with tool =; an argument a constraint names must
# be present, so a tool without it is never allowed under that constraint.
constrain = * arguments.owner = example-org
constrain = * arguments.repo = example-repo
constrain = create_branch arguments.branch ^= agent/example/
constrain = push_files arguments.branch ^= agent/example/
constrain = create_pull_request arguments.head ^= agent/example/
```

**Who enables what.**
- An upstream exists when its `.conf` exists and parses, and its container
  runs and is registered. **An upstream that fails** publishes no egress
  listener and no route for any agent (so `/mcp/<upstream>` is a 404
  everywhere), and the error is logged at every scan. The one exception
  is the tool check's own listener and the `default` cluster it routes to
  (below), which stay published so that the check can run again; no agent
  reaches either. An upstream fails when its `.conf` does not parse, when
  the file breaks "The credential rule" (under "The person's credential"),
  or when the tool check refuses it.
- **The tool check.** When an upstream's container registers, and when
  `redoma xds` starts, it asks the upstream for its tools (`initialize`,
  then `tools/list`, following its cursor) and compares them with every
  tool name its configuration names: the `.conf`'s `credential_for` and
  `credential_allows`, `_default.conf`'s `tool` and `tool_unconstrained`
  lines, and each agent file's. A name the server does not list is a
  mismatch. The answer is kept for the container's life, so an allowlist
  edited later is compared at the next scan without asking again. The
  upstream's `tool_check` decides what a mismatch does:
  - `refuse` (the default): the file that names the tool is invalid, with
    the effect any invalid file of its kind has (below): a mismatch in the
    `.conf` fails the upstream, one in `_default.conf` fails every agent's
    list for the upstream, and one in an agent's file fails that agent's
    list alone. The error names each missing tool and the file that names
    it, and is logged at every scan;
  - `warn`: the mismatch is logged, and everything is published as if
    there were none (the call fails at the server instead);
  - `off`: no check, and no request is made.

  The effect follows the file, as a parse error does, so that one agent's
  stale allowlist costs that agent its route's tools and no other agent
  anything, while a `.conf` or `_default.conf` that names a tool the
  server does not have is wrong for every agent it serves. Under `refuse`,
  an upstream that cannot be asked (it does not answer, or answers with an
  error) fails, since no file can be checked, and is asked again at the
  next scan; under `warn` that is logged and the upstream published.

  The question goes through Envoy, not to the server directly: per
  upstream, `redoma xds` publishes a check listener on a unix socket in
  `/run/redoma/xds` (see "The runtime layout"), routed to the upstream's
  `default` cluster (so Envoy injects the credential, which `redoma xds`
  never reads) and allowing only the lifecycle methods and `tools/list`.
  The request carries no `X-MCP-*` header, so the server lists its whole
  static universe. The check is generic: it compares names, and redoma's
  code names no upstream's tools.
- An agent has an upstream when its own file under that upstream exists.
  `redoma xds` then publishes the agent's route to it. Without the file,
  there is no route, and `/mcp/<upstream>` is a 404 for that agent.
- The route stays published for as long as the agent is, between turns
  too. The shim offers the tools to claude only during a turn, but Envoy
  cannot tell a turn's call from another process's in the container, so
  outside a turn the agent can still call its allowed tools. Publishing
  the route only while the relay reports an open task would close that,
  at a cost in coupling (xds following the relay's tasks) not worth paying
  for tools the agent is allowed anyway.
- An agent file that exists but does not parse still publishes the agent's
  route, with an RBAC policy that allows nothing: every call is a 403, and
  the error is logged. The route stays so the agent's failure says
  "denied", not "no such server".
- An invalid `_default.conf` fails every agent's list for that upstream,
  as a bad `egress/_default.txt` fails every agent's egress list: each
  agent with a file gets the deny-all route, and agents without one still
  have no route.
- `_default.conf` and any other underscore name can never be an agent's
  (`allowlist.ValidName` refuses the underscore).
- The host switch is the presence of `/etc/redoma/mcp/`; nothing in it,
  nothing published.

`redoma xds` turns these into Envoy configuration only: the MCP filter
with `REJECT_NO_MCP`, `BODY`, `reject_duplicate_keys`,
`max_request_body_size` and the extraction rules the constraints need;
the RBAC policy; routes that match the extracted tool name (with
`clear_route_cache`) to the credential's cluster; the `header_mutation`
filter's prefix removals and the route's per-agent headers; and the
clusters, each with its upstream credential injector and source namespace.
It never reads a credential, and contacts an upstream only for the tool
check, through Envoy.

### The runtime layout

Following the rc.4 rule that every runtime directory is under
`/run/redoma`:

| Path | Owner, mode | Holds |
|---|---|---|
| `/run/redoma/mcp-credentials` | root, 0700 | the upstreams' SDS files, written by their minters, read by Envoy (root) alone |
| `/run/netns/redoma-mcp-<container ID>` | the hook | each upstream container's namespace, as for agents |
| `/run/redoma/xds/mcp-check-<upstream>.sock` | root (Envoy binds it), 0666, in `redoma-sds`'s 0750 directory | the tool check's listener, which only `redoma xds` and root reach |

**The check socket's access is its directory's.** Envoy binds the
listener's socket (a `Pipe` address, which has only a `path` and a `mode`,
`api/envoy/config/core/v3/address.proto:23-34` at v1.40.0). It runs as
root, so the socket is root's; after the bind it applies `mode` with a
`chmod` and changes no owner (`source/common/network/socket_impl.cc:73-79`
at v1.40.0). A mode of `0600` or `0660` would therefore keep `redoma xds`
(uid 961, not in root's group) out. The listener's mode is `0666`, and
what limits it is `/run/redoma/xds`, which `redoma-xds.service` makes
`0750` `redoma-sds` (`RuntimeDirectory=redoma/xds`,
`RuntimeDirectoryMode=0750`): only uid 961, members of group `redoma-sds`
(the host Envoy, a supplementary member, see README.md "Envoy") and root
can reach a path in it. These are the same principals that already reach
`xds.sock` in that directory, so the check socket adds none. No agent or
upstream container can mount it (the hook refuses all of `/run/redoma`).

No agent or upstream container may mount any of `/run/redoma`; the hook
already refuses it. An upstream's container gets no credential file, no
workspace and no host socket: its arguments, its egress listener and the
requests Envoy forwards are all it has.

**Users.** Each upstream container runs rootful with Podman's
`--userns=auto`: at its start Podman gives it a user namespace of its own,
mapped to a range of unprivileged host uids and gids that no other
`--userns=auto` container holds. There are no fixed uids, nothing is
added to `/etc/passwd` or `/etc/group`, and the upstream's `.conf` has no
`user` key. Podman 4.9 supports this in rootful mode, where it allocates
the ranges from the subordinate ids of the user `containers` (podman-run(1)
of Podman 4.9.3, `--userns=mode`, value `auto`: "The `--userns=auto` flag
requires that the user name `containers` be specified in the /etc/subuid
and /etc/subgid files, with an unused range of subordinate user IDs that
Podman containers are allowed to allocate"). So:

- **Install** needs a `containers` entry in both `/etc/subuid` and
  `/etc/subgid`, with a range that holds no host uid or gid in use: not
  root's, not redoma's services' (961, 963, 964), not any agent's.
- **The hook** refuses an agent whose uid, gid or any `additionalGids`
  entry falls in those ranges, so no agent shares an id with an upstream's
  process. For an upstream it requires the opposite of an agent: a `user`
  namespace entry, whose uid and gid mappings lie wholly inside the
  `containers` ranges and map no host id 0.
- **`--network none` is proven differently.** For an agent the hook reads
  "a network namespace with no path and no user namespace" as proof of
  `--network none`; with a user namespace, Podman creates the network
  namespace without a path for any network mode and attaches the network
  after the runtime made it, so that proof does not hold (see AGENTS.md,
  "The OCI hook"). For an upstream, the hook mounts the namespace at
  createRuntime as for an agent, but registers it with `redoma xds` only
  from its poststart hook, once Podman has configured the network, and
  only when the namespace has no interface but `lo`. An upstream not
  registered gets no cluster, no egress listener and no requests, so it
  never sees a credential, whatever network it has (open question 1).

### Upgrading an upstream

An upgrade is a digest change of `image` in the upstream's `.conf`, and a
restart of its unit. The tool check (see "Who enables what") then compares
the new release's tools with the allowlists, so a tool renamed or removed
is caught at start without redoma knowing any tool. What a tool list cannot
show stays a written checklist, gone through before the digest moves:

- the release's flags, and whether `--tools` is still a ceiling over every
  request header;
- the request headers it reads (`X-MCP-*` and any new family the agent's
  listener must strip), and whether it now routes or authorizes on a
  header such as `Mcp-Param-*` rather than the body;
- the parser its dispatch uses (case, duplicate keys) against Envoy's;
- what a renamed or new tool does, before any allowlist names it.

## Pushing

GitHub's server cannot push an agent's local commits. Its write paths are
the contents and Git Data APIs:
- `push_files` commits a list of files, sent as tool arguments, onto a
  branch;
- `create_or_update_file` and `delete_file` change one file each;
- `create_branch` makes a branch.

There are two ways to publish an agent's work.

**Through the API tools** (`create_branch`, then `push_files`):

- For:
  - No redoma code. Nothing on the host reads the agent's workspace.
  - Commits made through the API are created by GitHub for the App and can
    carry GitHub's verified signature.
  - The `workflows` permission backstop applies to them as it does to a
    push.
- Against:
  - The agent's history is not what is published. Each call is one new
    commit, authored as the App, made from file contents the model has to
    write out in full as arguments. That is slow and costly in tokens for
    large changes, and unusable for binary files.
  - No deletions or renames within `push_files`.
  - The target paths are in a list, so Envoy cannot constrain them. Only
    the App's permissions and rulesets can protect paths.
  - The agent's local commits carry no meaning (sign-off, authorship).

**Through a host push, as a second upstream.** A small redoma MCP server,
`redoma git-push`, offers one tool, `push(repo, branch)`, served through the
same pattern: its own container and user namespace, Envoy's RBAC, the App
token injected by Envoy. It reads the agent's workspace (bind-mounted read-only
into its container, per agent) as data, never running git there:
- `openat2` beneath the workspace, no symlinks;
- no configuration read at all; alternates and shallow repositories
  refused;
- git's caches (the commit graph, multi-pack indexes, bitmaps) ignored,
  never trusted.

It then works on a mirror of its own:
- it fetches only the default branch, the allowed bases and the agent's
  prefix;
- it checks every commit from the merge base with the bases up to the tip:
  author, sign-off, protected paths against each parent, sizes, and
  `allow_merges`;
- it pushes fast-forward only to a branch under the agent's prefix.

- For: the agent's own commits are published as made, with per-commit
  checks of paths and authorship that GitHub alone cannot express.
- Against: redoma code that parses hostile input (the agent's object
  store), and read access to every workspace it serves.

**Recommendation:** start with the API tools. They need nothing but
configuration, and the GitHub-side backstops cover what Envoy cannot
check. Build `redoma git-push` only when hosts need the agents' own
commits published (signed-off history, large or binary changes). It then
plugs into the same pattern as any other upstream, with no special case.

## Network

- **Every tool an agent uses goes through an MCP upstream.** Agents get no
  direct git or GitHub egress: no GitHub host (`github.com`,
  `api.github.com`, `*.githubusercontent.com`) is in any agent's egress
  allowlist, `_default.txt` or its own. A container with no GitHub host
  has no direct path to GitHub, whatever it holds. The upstreams' own
  egress listeners have those hosts instead, each scoped to its upstream's
  namespace.
- **Fetching repositories** (to rebase, to read a branch, to read another
  repository) is a read-only git upstream too, never git over the agent's
  egress: either an existing MCP server whose tools are read-only git
  (clone or fetch, read files, trees and logs at a ref), or one added
  later as an upstream, in the same pattern. Until there is one, reads
  are GitHub's server's read tools (`get_file_contents`, `list_commits`,
  `pull_request_read`), which read through the API and need no git. How a
  clone or fetch reaches the agent's workspace through MCP is open
  (open question 2); until it is answered, an agent that needs one keeps
  its GitHub egress (see "Rollout").
- **Dependencies hosted on GitHub** (release archives that redirect to
  `objects.githubusercontent.com` and similar) are not fetched from
  GitHub: they come through mirrors that are not GitHub (a Go module
  proxy, a Bazel download mirror) in the agent's egress allowlist.

## Audit

Envoy's access log on the MCP listeners is the audit record. One line per
request, with:

- the listener (the agent);
- the upstream and the cluster (so the credential role, never the token);
- from the MCP filter's metadata, `%DYNAMIC_METADATA(envoy.filters.http.mcp:method)%`
  and `params.name`, plus any extracted arguments;
- the RBAC result (`%DYNAMIC_METADATA(envoy.filters.http.rbac:...)%`, and
  the response flag for a denial);
- the status, duration and bytes;
- a request id, which the upstream can log too.

Bodies are not logged, and a digest of the body is not available to the
access log without another filter. GitHub's own audit log names the App or
the person for each write.

## Rollout

1. The generic pieces: the hook's upstream registration (its poststart
   stage and definition), the MCP listener, RBAC, routes, clusters and the
   tool check in `redoma xds`, the shim's `--mcp-upstream`, and
   `redoma-mcp@.service`. Plus unit tests against fake upstreams.
2. GitHub's server as the first upstream: read tools only for a first host.
3. Writes (`create_pull_request`, comments, replies), with the second
   credential.
4. Publishing through `create_branch` and `push_files`. Then, if needed,
   `redoma git-push`.
5. A read-only git upstream, once open question 2 (how a clone or fetch
   is served) has an answer.
6. GitHub hosts removed from the agents' egress allowlists, only after
   step 5: until then an agent that needs a clone or fetch keeps them.

**Identity modes.** Nothing here depends on how `redoma xds` gets the
agents' SVIDs: the MCP listener's identity is its namespace in both
Delegated and Broker mode, and no upstream needs an SVID.

**Migrating an agent from an in-container `gh`:**
1. Write its allowlist file.
2. Start its shim with `--mcp-upstream=github`.
3. Tell it, in its instructions, to use the tools.
4. Remove the token from its environment and the GitHub hosts from
   `egress/<name>.txt`.
5. Restart it.

Agents move one at a time.

**Tests**, under `bazel test //...`, with no network and no Envoy:

- the configuration parsers, every rule and refusal;
- the generated Envoy resources:
  - per agent: the filter order (MCP, RBAC, router), the MCP filter's
    modes, the RBAC policy (an allowlist; no tool or method outside the
    files), the routes, the stripped headers;
  - per upstream: the egress listener, the clusters' credential injectors
    and source namespaces.

  Every resource is checked through deterministic packing (`lds.mustAny`).
- the hook's registration of upstreams, and its refusal of an upstream
  name used as an agent's, of an agent whose ids fall in the `containers`
  ranges, and of an upstream without a `--userns=auto` user namespace;
- the hook's poststart stage for upstreams: it registers a namespace with
  only `lo`, and refuses (leaves unregistered, and unmounts) one with any
  other interface, read from a fake namespace listing; and the poststart
  definition in `ocihook/hooks.d`, held to its annotation and stage like
  the existing definitions;
- the tool check against a fake upstream, under each `tool_check` value:
  a missing tool, an upstream that does not answer, a paged `tools/list`.

An integration test, on a host with Envoy, runs a fake MCP server as an
upstream. It checks the following:
- that an allowed call reaches it with the injected header;
- that a disallowed tool, a disallowed method, a non-MCP body, an oversized
  body, a duplicate key and an `X-MCP-Toolsets` header from the agent are
  each refused or stripped before it;
- that a rotated SDS file is used without a restart.

An e2e test against a dedicated test repository, with a test App, runs from
a workflow started by hand.

## Open questions

1. **Proving `--network none` for an upstream.** The design registers an
   upstream from its poststart hook, once its namespace shows only `lo`
   (see "Users"). To be checked on Podman 4.9 with `--userns=auto`: that
   poststart runs after the network is configured, that a poststart
   failure leaves the container unregistered, and that nothing attaches
   a network later.
2. **How a clone or fetch is served.** A read-only git upstream answers
   tool calls, and a clone is a pack written into the agent's workspace,
   which a tool's result does not do on its own. Is it an existing MCP
   server's tools, one added as an upstream, or reads at a ref only (files,
   trees, logs), with no local clone at all?

### Decided

- **One Envoy** for MCP, model, egress and relay listeners; a crash of
  the alpha MCP filter takes every listener down, an accepted risk (see
  "The MCP filter").
- **No `ext_proc` fallback.** A bug in the MCP filter is fixed in Envoy,
  not worked around in redoma.
- **CI secrets on same-repository branches** are a deployment's hardening
  choice, out of redoma's scope (see "CI on same-repository branches").
- **Routes are published whenever the agent is,** not only during a turn:
  following the relay's tasks is not worth its complexity.
- **Uids come from `--userns=auto`**, a host range per container from the
  `containers` subordinate ids; no fixed uid (see "Users").
- **One upstream instance** serves every agent that has it.
- **Agents may read public repositories,** other owners' included.
  Lockdown stays on as a best-effort filter; the per-agent allowlist's
  `owner` and `repo` constraints narrow reads (see "Untrusted text").
- **No direct git or GitHub egress for agents:** every tool, git reads
  included, goes through an MCP upstream (see "Network").
- **Upstream images are checked at start** against the allowlists,
  `tool_check = refuse | warn | off` per upstream, `refuse` by default;
  behaviour changes stay a checklist step (see "Upgrading an upstream").
- **One credential per upstream per install,** shared by its agents;
  GitHub enforces the union of their repositories.
