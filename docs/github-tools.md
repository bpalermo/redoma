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
  container of its own, `--network none`, in its own network namespace,
  like an agent. Envoy binds an egress listener in that namespace, holding
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
  `redoma/upstream=<name>`) as it registers an agent: it mounts the
  namespace at `/run/netns/redoma-mcp-<container ID>`, applies the same
  process, mount and namespace checks, and registers it with `redoma xds`
  under its upstream name. An upstream name is never an agent name: a
  container carrying both annotations is refused, and the two namespaces
  of names are kept apart in `agent.Merge`.
- The shim gains `--mcp-upstream=<name>` (repeatable): an `http` entry in
  its `--mcp-config` and `mcp__<name>__*` in `--allowedTools`. This only
  decides what claude is offered; it grants nothing.
- A systemd template unit, `redoma-mcp@.service`, runs one upstream
  container from its configuration file (image by digest, arguments,
  user), so starting a new upstream is `systemctl enable --now
  redoma-mcp@<name>`.

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
  bug grants a dangerous tool. Keep a fallback ready: an `ext_proc`
  service that redoma owns (`envoy.filters.http.ext_proc` is stable and
  `robust_to_untrusted_downstream_and_upstream`,
  `extensions_metadata.yaml:526-532`), which parses the JSON-RPC body in Go
  and returns the same allow or deny. It is generic code, written once. Lua
  is not the fallback: it is stable and robust
  (`extensions_metadata.yaml:672-678`), but Envoy's Lua has no JSON
  parser, and a hand-written one in Lua would be weaker than the C++ one it
  replaces. Running the MCP listeners in a second Envoy, so that a crash
  spares the model and relay listeners, is an open question.

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
  or issue. A host that needs "its own pull requests only" needs the
  `ext_proc` fallback, which can look the target up, or a repository the
  agent's App can write nothing else in.

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
  to `mcp_github` gets the App token, and every route to `mcp_github_pr`
  gets the person's token.
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
- **Image:** `ghcr.io/github/github-mcp-server:v2.0.3`, an OCI index,
  `sha256:250bc6ce70788207608ef74cef4397230ff2e8481218da74d6093e4bd9101132`
  (linux/amd64
  `sha256:cecac9a377c81f5ebfd20a9aabc7b9ff6a3a92393af6aa4b75875fe884e037b9`,
  linux/arm64
  `sha256:be672d50a0de2c6f9c4b81527828ad1b81fb4108b33f98b5876c2abc8d4ca488`).
  Its config runs as user 0 with entrypoint `/server/github-mcp-server`, so
  the unit gives it `--user`. Its HTTP client is Go's default transport,
  which honours `HTTPS_PROXY`.

## Layered limits

No single layer is trusted to stop a dangerous call. Each of the three
below would stop a merge on its own.

1. **The server's own flags, first.** GitHub's server is started with an
   explicit `--tools` list, the union of what any agent on the host may
   call, and `--lockdown-mode` for public repositories. Toolsets are not
   used: they are coarse. `repos` holds `delete_repository`,
   `create_repository` and `fork_repository` next to `get_file_contents`;
   `pull_requests` holds `merge_pull_request`, `update_pull_request_branch`
   and `pull_request_review_write` next to `create_pull_request`. A tool
   that is not in the static universe is not registered at all, whatever
   the request says. Envoy also sets `X-MCP-Tools` per agent to that
   agent's list, so `tools/list` shows each agent only its own tools.
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
   - The App installed on the agents' repositories only.

Never allowed at any layer, for GitHub:

- merging, auto-merge, updating a branch from its base;
- reviews (`pull_request_review_write`, `add_comment_to_pending_review`);
- statuses and check runs;
- anything under Actions;
- repository administration, deleting or creating repositories, forks;
- deleting files or branches;
- Copilot assignments.

## Configuration

Two files per upstream, plus one per agent per upstream, in the style of
the egress allowlists. All of them are root's and writable by no one else,
like `agents.txt`, and strict: an unknown key, a key given twice (list keys
excepted) or a value that does not parse invalidates the file, and an
invalid file publishes nothing for it ("An error never widens access").

`/etc/redoma/mcp/<upstream>.conf`, the upstream:

```
# /etc/redoma/mcp/github.conf
image = ghcr.io/github/github-mcp-server@sha256:250bc6ce70788207608ef74cef4397230ff2e8481218da74d6093e4bd9101132
user = 965:965
port = 8082
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
# Which tools use a credential other than default.
credential_for.create_pull_request = pr
# Request headers removed before the upstream sees them; may end in *.
strip_header = X-MCP-*
strip_header = Mcp-*
# Optional: a header set per agent, {tools} being the agent's list.
agent_header.X-MCP-Tools = {tools}
max_request_bytes = 1048576
```

`/etc/redoma/mcp/<upstream>/<agent>.conf`, an agent's allowlist (and
`_default.conf`, every agent's, read first):

```
# /etc/redoma/mcp/github/example.conf
tool = get_me
tool = pull_request_read
tool = list_pull_requests
tool = create_pull_request
tool = add_issue_comment
tool = add_reply_to_pull_request_comment
tool = create_branch
tool = push_files
# Constraints: <tool or *> <argument path> = <value>, or ^= for a prefix.
constrain = * arguments.owner = example-org
constrain = * arguments.repo = example-repo
constrain = create_branch arguments.branch ^= agent/example/
constrain = push_files arguments.branch ^= agent/example/
constrain = create_pull_request arguments.head ^= agent/example/
```

**Who enables what.**
- An upstream exists when its `.conf` exists and parses, and its container
  runs.
- An agent has an upstream when its own file under that upstream exists.
  `redoma xds` then publishes the agent's route to it. Without the file,
  there is no route, and `/mcp/<upstream>` is a 404 for that agent.
- A file that exists but does not parse publishes a route whose RBAC
  allows nothing.
- `_default.conf` and any other underscore name can never be an agent's
  (`allowlist.ValidName` refuses the underscore).
- The host switch is the presence of `/etc/redoma/mcp/`; nothing in it,
  nothing published.

`redoma xds` turns these into Envoy configuration only: the MCP filter
with `REJECT_NO_MCP`, `BODY`, `reject_duplicate_keys`,
`max_request_body_size` and the extraction rules the constraints need;
the RBAC policy; routes that match the extracted tool name (with
`clear_route_cache`) to the credential's cluster; `request_headers_to_remove`
and the agent's headers; and the clusters, each with its upstream
credential injector and source namespace. It never contacts the upstream
or reads a credential.

### The runtime layout

Following the rc.4 rule that every runtime directory is under
`/run/redoma`:

| Path | Owner, mode | Holds |
|---|---|---|
| `/run/redoma/mcp-credentials` | root, 0700 | the upstreams' SDS files, written by their minters, read by Envoy (root) alone |
| `/run/netns/redoma-mcp-<container ID>` | the hook | each upstream container's namespace, as for agents |

No agent or upstream container may mount any of `/run/redoma`; the hook
already refuses it. An upstream's container gets no credential file, no
workspace and no host socket: its arguments, its egress listener and the
requests Envoy forwards are all it has.

**Users.** Each upstream runs under a uid of its own, from its `user` key.
The hook refuses that uid to agents (it joins `hook.ServiceIDs` through
the configuration) and refuses an agent's uid to upstreams. 965 is
proposed for GitHub's.

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
same pattern: its own container and uid, Envoy's RBAC, the App token
injected by Envoy. It reads the agent's workspace (bind-mounted read-only
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

- Once an agent publishes through an upstream, its egress allowlist no
  longer needs `github.com` or `api.github.com`, and should not have them:
  a container with no GitHub host has no direct path to GitHub, whatever
  it holds. The upstream's own egress listener has them instead, scoped to
  that upstream's namespace.
- **Fetching its own repositories** (to rebase, to read a branch) still
  needs git over HTTPS. Public repositories can stay reachable anonymously
  through the agent's egress (`github.com`, with no credential in the
  container, so what it can do there is what anyone anonymous can).
  Private ones need either a read-only git endpoint on the host (an
  upstream of its own, `upload-pack` only) or the API's read tools
  (`get_file_contents`, `list_commits`).
- **Public dependencies** (release archives that redirect to
  `objects.githubusercontent.com` and similar) go through the agent's
  egress allowlist as today. A CONNECT tunnel is opaque, so Envoy cannot
  tell paths apart there; prefer mirrors that avoid GitHub (a Go module
  proxy, a Bazel download mirror).
- No GitHub host belongs in `egress/_default.txt`.

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

Bodies are not logged. A digest of the body is not available to the access
log without another filter; if hosts need one, the `ext_proc` fallback can
record it. GitHub's own audit log names the App or the person for each
write.

## Rollout

1. The generic pieces: the hook's upstream registration, the MCP listener,
   RBAC, routes and clusters in `redoma xds`, the shim's `--mcp-upstream`,
   and `redoma-mcp@.service`. Plus unit tests against fake upstreams.
2. GitHub's server as the first upstream: read tools only for a first host.
3. Writes (`create_pull_request`, comments, replies), with the second
   credential.
4. Publishing through `create_branch` and `push_files`. Then, if needed,
   `redoma git-push`.
5. GitHub hosts removed from the agents' egress allowlists.

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
  name used as an agent's, or an agent's uid used by an upstream.

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

1. **A second Envoy for the MCP listeners?** The MCP filter is alpha. A
   crash would take the model and relay listeners of every agent with it.
   A separate Envoy for MCP costs a second process and bootstrap.
2. **When to switch to the `ext_proc` fallback.** Before the first host,
   or only if the MCP filter is still alpha by then, or on the first
   finding against it?
3. **Same-repository branches run CI with the repository's secrets.** A
   pull request from a branch of the same repository runs its workflows
   with the base repository's secrets and a token that may write, and with
   the branch's own code (build scripts, tests). Refusing workflow changes
   does not stop changes to what those workflows execute. Should agents
   publish to a fork the App owns and open the pull request across
   repositories, so CI treats it as a fork's? That costs the agent access
   to CI results until a maintainer approves the run.
4. **Tool calls outside a turn.** The shim offers the tools only during a
   turn, but Envoy cannot tell. Should `redoma xds` publish an agent's MCP
   route only while the relay reports an open task for it?
5. **Uids.** One per upstream (965 for GitHub's), or one shared by all
   upstreams?
6. **One upstream instance for all agents, or one per agent?** The GitHub
   server is stateless, so one instance serves everyone. A per-agent
   instance would give each agent its own `--tools` ceiling, at the cost of
   one container per agent and upstream.
7. **Untrusted text.** `--lockdown-mode` filters content from authors
   without push access on public repositories. GitHub says it is
   best-effort and not an authorization boundary. Is it enough, or should
   public repositories be refused to agents entirely?
8. **The read-only git endpoint**: needed by the first host that drops
   GitHub egress, or does anonymous egress for public repositories cover
   it?
9. **Upgrading an upstream image** is a digest change in its `.conf`. Its
   new tool names, flags and request headers must be checked as above
   before the digest moves. Should redoma check a release's tool list
   against the allowlists at start, and refuse a tool name it no longer
   has?
