# AGENTS.md

Guidance for contributors, human or not, working in this repository.
README.md says what redoma is and how a host runs it; this file says how the
code is built, laid out and kept correct.

## What this is

redoma runs coding agents in isolated containers on one Linux host. Each
agent runs in its own Podman container and its own network namespace, with
no network but what one host Envoy binds inside that namespace: the
listener a connection arrives on is the agent's identity. redoma is the
control plane around that Envoy:

- `redoma xds` serves the host Envoy, over ADS on a unix socket:
  - LDS: each running agent's listeners, bound in the agent's namespace;
  - CDS: each agent's cluster to the relay;
  - SDS: each agent's SPIFFE identity, `spiffe://<td>/agent/<name>`,
    fetched on the agents' behalf from SPIRE's Delegated Identity API, or
    from the SPIFFE Broker API (`identity_api = broker`, see
    `docs/broker-identity.md`).

  It runs as uid 961 (`redoma-sds`), the one delegate or broker SPIRE
  authorizes.
- `redoma relay` carries the lead's tasks to the agents and their turns'
  output, questions and messages back (see "The relay").
- `redoma msg` and `redoma mcp` are the lead's clients of the relay (see
  "redoma msg and redoma mcp").
- `redoma oci-hook` makes a rootful Podman container annotated
  `redoma/agent=<name>` an agent (see "The OCI hook").
- `redoma-shim`, a separate binary, is the agent container's entrypoint
  (see "redoma-shim"), and `runtime/` the recipe of the image it runs in.

`<td>` is the trust domain, `trust_domain` in `/etc/redoma/redoma.conf`
(see "Configuration"), `redoma.internal` by default.

## Build and test

```bash
bazel test //...                # builds and runs the unit tests
bazel build --config=ci //...   # what CI builds: lockfile check, shellcheck, gocognit
bazel run //:gazelle            # after adding, removing or renaming any .go file
bazel run //:format             # format Starlark, Go and shell; CI runs //:format.check
bazel mod tidy                  # after changing go.mod or a bazel_dep
bazel run //redoma -- --version
```

Building and testing need Bazel (bazelisk reads `.bazelversion`) on Linux,
plus `dpkg` and `dpkg-deb` on the host: `//deb:deb_test` and
`//deb:deb_version_test` read the packages with them and fail without them.

Bazel is the only build. Do not run the host `go` tool, and do not "fix"
go.mod with `go mod tidy`. Change a Go dependency through rules_go's SDK:

```bash
bazel run @rules_go//go -- get github.com/some/module@v1.2.3
```

Follow it with `bazel mod tidy` and `bazel run //:gazelle`. Settings that
belong to one machine (a remote cache, a Go proxy) go in `user.bazelrc`,
which `.bazelrc` imports and git ignores.

Linting is part of the build. Every Go package is compiled with the
analyzers of `//:nogo` (`go vet`'s and staticcheck's), and a finding is a
compile error: fix it rather than excluding the file. Shell scripts are
checked by shellcheck and Go functions by gocognit, aspects that
`--config=ci` turns on; a script is only seen through a Bazel target, so a
new directory of scripts needs an `sh_library` like `//bazel:scripts`. The
gocognit threshold (`bazel/lint/linters.bzl`) is 15 and never goes up:
split a function instead.

`bazel run //:format` formats Starlark (buildifier), Go (gofumpt) and shell
(shfmt, tabs). A change must leave nothing for it to format, Gazelle no
diff, and `MODULE.bazel.lock` current (`--config=ci` refuses a stale one).

## Layout

- No `cmd/`. Each binary lives in a root-level directory with its `main.go`,
  and is listed in `//:binaries_test` (a `build_test`), so one that goes
  missing fails `bazel test //...`:
  - `redoma/` is `redoma`, the cobra root, and `redoma/redoma.conf`, the
    settings file the package installs;
  - `shim/` is `redoma-shim`, the agent image's entrypoint.
- Each subcommand of `redoma` is a root-level package that exports its
  cobra command: `xds/` (`redoma xds`, with its unit
  `xds/redoma-xds.service`), `relay/` (`redoma relay`, with
  `relay/redoma-relay.service`), `msg/` (`redoma msg` and `redoma mcp`),
  `ocihook/` (`redoma oci-hook`, with its `hooks.d/` definitions and the
  `containers.conf.d/` drop-in).
- `api/redoma/relay/v1`: the relay's protocol (`relay.proto`, package
  `redoma.relay.v1`, the `Relay` and `Lead` services), compiled by
  `go_proto_library`; Gazelle generates its BUILD file. Protos live under
  `api/<proto package path>` and are imported without the `api/` prefix
  (`gazelle:proto_strip_import_prefix /api`). They are `edition = "2024"`.
  Go code uses the edition's default, the Opaque API: builders
  (`relayv1.Task_builder{...}.Build()`), getters and setters, and
  `WhichBody()` for a oneof. `relay.proto` keeps the wire format it had as
  proto3 (`features.field_presence = IMPLICIT`), so peers of either version
  interoperate; `//api/redoma/relay/v1:relay_test` holds every envelope to
  bytes captured from the proto3 version (`testdata/wire.txt`). A field
  never changes its number or its presence.
- Every message, field, oneof, enum, enum value, service and rpc of a proto
  has a leading comment, the source of the protocol's documentation: what
  it means, its units and range, what its zero value means, and when it is
  set. `//api/redoma/relay/v1:relay_test` (`doc_test.go`) fails on an
  element without one, reading the descriptors with their comments
  (`:relay_source_info`, `protoc --include_source_info`).
- Code private to a subcommand goes under its `internal/`; code shared
  between them under `common/`.
- `xds/internal/`:
  - `agent`: an agent (ID, namespace path, selectors) and `Source`, the
    interface of whatever says which agents run;
  - `activedir`: a launcher's `Source`, its active files and namespaces,
    rescanned every second (see "Launchers");
  - `hooksource`: the OCI hook's `Source`, registrations on a root-only
    socket; `agent.Merge` runs both, one owner per agent name;
  - `egress`: an agent's CONNECT allowlist, the hosts of
    `<etc>/egress/_default.txt` (every agent's) then those of
    `<etc>/egress/<name>.txt`, each host once. Both files take one host name
    per line and `#` comments; a missing file adds nothing, and a bad line in
    either fails the agent's whole list (see "An error never widens access").
    Both are re-read at every scan, so an edit to `_default.txt` republishes
    every agent's egress listener. `_default` can never be an agent's name
    (`allowlist.ValidName` refuses the underscore; a test holds that);
  - `modelauth`: how the model listeners inject the credential;
  - `lds`: an agent's listeners, model and egress, and its relay listener;
  - `cds`: an agent's relay cluster;
  - `identity`: the Delegated Identity API client and the Broker API's
    (X.509 SVIDs, the bundle, JWT SVIDs), `Workload`, redoma xds's own SVID
    in Broker mode, and `identitytest`, the Delegated API's fake;
  - `fdstore`: systemd's file descriptor store, where the agents' pidfds
    outlive a restart;
  - `hostcheck`: Broker mode's checks of the host at start (SPIRE's sockets
    under the hook's forbidden paths, the Workload API's directory);
  - `controlplane`: the snapshot, kept in step with all of the above;
  - `server`: ADS, LDS, CDS and SDS on the unix socket.
- `ocihook/internal/hook`: the hook's createRuntime and poststop logic.
- `relay/internal/`: `hub`, the relay's state and its two services; `mtls`,
  the agent a connection's client certificate names (and `mtlstest`, a test
  CA and a static source); `svid`, the relay's SVID from the Workload API.
- `shim/internal/`: `shim`, the daemon (the stream, the task queue, the
  turns); `mcp`, the MCP server claude runs (`redoma-shim mcp`), its tools;
  `bridge`, between the two; `reaper`, PID 1's reaping; `fakeclaude`, the
  claude of the shim's tests.
- `msg/internal/`: `leadmcp`, `redoma mcp`'s tools; `feed`, its Watch and
  the buffer poll reads; `lines`, how an event is shown (raw, compact);
  `leadtest`, a fake Lead service.
- `common/config`: `/etc/redoma/redoma.conf` and the `--config` flag;
  `common/mcpserver`: the stdio MCP server (JSON-RPC, tools only) of both
  `redoma-shim mcp` and `redoma mcp`; `common/register`: the protocol
  between the hook and `hooksource`; `common/atomicfile`: whole-file
  replacement for the state files; `common/allowlist`:
  `/etc/redoma/agents.txt`; `common/reliable`: the relay stream's
  at-least-once delivery, used by both of its ends; `common/sockettest`:
  the unix-socket test helpers; `common/version`: the stamped version.
- `examples/`: an Envoy bootstrap and an SDS secret file, held to the code
  by `//xds/internal/lds:lds_test`.
- `deb/`: the Debian package of the host side (see "The package").
- `runtime/`: the base image, `redoma-base`, and the runtime image,
  `redoma-runtime`, the base and Claude Code (see "The runtime image");
  `bazel/claude_code` is the repository rule that fetches Claude Code for
  it.
- `bazel/` holds build rules and CI scripts; `bazel/platforms` the target
  platforms and `platform_files`, which builds a target for one of them.

## Configuration

`/etc/redoma/redoma.conf` (`--config` before or after the subcommand) holds
what every subcommand shares. Lines of `key = value`, `#` comments:

| Key | Default | Used by |
|---|---|---|
| `trust_domain` | `redoma.internal` | `xds` (`--trust-domain`; the relay's ID, `--relay-id`, is `spiffe://<td>/host/relay` unless given), `relay` (`--trust-domain`) |
| `agent_annotation` | `redoma/agent` | `oci-hook` (`--annotation`) |
| `spire_admin_socket` | `/run/redoma-spire/admin/admin.sock` | `xds` (`--spire-admin-socket`) |
| `spire_workload_api` | `unix:///run/redoma-spire/workload/agent.sock` | `relay` (`--workload-api`) |

A flag given on the command line wins over the file. A missing file at the
default path is all defaults; a `--config` that does not exist, an unknown
key, a key given twice or a value that does not parse fails the command
before it opens anything: a typo never falls back to a default.

`redoma/redoma.conf` is the file the package installs, every key at its
default; `//redoma:redoma_test` holds it to `config.Default()`. A new key
goes in `common/config`, that file, and the table above.

## Design rules that code must keep

- **A launcher's state is read, never written.** `/run/redoma/active` and
  `/run/netns` belong to whatever starts the agents; `/etc/redoma` to the
  host's administrator.
- **One goroutine owns the state.** `controlplane.Run` applies the Source's
  events and the subscriptions' updates, which reach it on a channel, and
  re-reads the allowlists on a timer; nothing is locked. A Source that stops
  stops the control plane.
- **Namespaces and selectors come from the Source.** Nothing outside
  `activedir` derives a namespace path (`/run/netns/redoma-<name>`) or a
  selector from an agent's name: `lds` and `identity` take them from the
  `agent.Agent` the Source emits. The hook passes a container's own
  namespace.
- **For `activedir`, an agent is running when it has an active file AND a
  namespace.** Without the namespace Envoy could not bind its listeners.
- **A namespace is its device and inode, not its path.** Envoy gives a new
  listener the socket of a live or draining listener at a compatible
  address, and the address is the namespace path, so a namespace replaced at
  the same path would be served from the old socket. The record carries
  `NetnsID`. On a replacement at the same path the controller publishes the
  agent's removal, waits until Envoy reports that version applied (the
  `version_info` of its next SotW LDS request,
  `server.ListenerVersions` → `Controller.ListenersApplied`) and
  `--rebind-grace` (500ms) more, then binds it again and resubscribes. A
  removal not reported within `--rebind-timeout` (5s) is published again;
  the agent is never bound without the report. The grace narrows, without
  closing, the window in which Envoy has reported but not yet closed the
  old socket (`controlplane/rebind.go`).
- **Versions are this process's.** Every version is `<boot nonce>-<n>`, and a
  reported version with another prefix (Envoy reconnecting after a restart
  of this service) is ignored: it says nothing about this process's
  snapshots.
- **What Envoy holds outlives this process.** Envoy keeps its listeners
  while `redoma xds` is down, so `--state-file`
  (`/run/redoma-xds/state.json`, a tmpfs cleared at boot) records, per
  namespace path, the namespace Envoy may hold listeners in. It is written
  before each snapshot that could bind something new. At start, an agent
  found in its recorded namespace is bound at once; one in another
  namespace (replaced while this process was down) waits for Envoy's report
  like any replacement (`controlplane/state.go`). An unreadable file is
  logged and ignored: such agents then need a restart.
- **The first snapshot waits for the Source's initial set** (its `Synced`
  event). A snapshot of a partial set would make an Envoy still holding a
  previous run's listeners drop the ones not in it. When `--sync-timeout`
  (10s) passes without it (activedir sends none while its scans fail):
  with a non-empty state record, Envoy may hold the previous run's
  listeners, so nothing is published, an error is logged at every timeout,
  and the control plane keeps waiting; with none (a first start, or after a
  reboot), what the Source has is published, since a fresh Envoy holds
  nothing and needs an answer.
- **SotW only.** The rebind needs SotW LDS reports, so the server refuses
  delta streams (ADS, LDS, CDS and SDS) with Unimplemented and logs it.
- **An error never widens access.** An allowlist that cannot be parsed allows
  no host (every CONNECT a 403); a model credential mode that cannot be read
  or parsed injects no credential (every model request a 401); a launcher
  state that cannot be read keeps the last one; a settings file that cannot
  be parsed starts nothing.
- **An agent's identity is subscribed by its Source's selectors and only
  those** (Delegated mode). SPIRE takes the delegate's selectors as
  attested, so a Source decides identities. Every Source sends
  `redoma:agent:<name>`, the selector the SPIRE entries select on. A new
  selector format is a change every host's SPIRE entries must follow. An
  SVID whose ID is not `spiffe://<td>/agent/<name>` is not served, in
  either mode. In Broker mode an agent is named by its container's process,
  a pidfd the hook bound to it; SPIRE's attestors decide its selectors, and
  an answer is used only if the process was still alive once SPIRE had
  answered (`docs/broker-identity.md`).
- **The name is the identity, so the name policy is the security
  boundary.** Whoever can make a Source emit a name gets
  `spiffe://<td>/agent/<name>`. activedir: whoever can write
  `/run/redoma/active` (root). hooksource: only uid 0 on its socket, only
  names in `/etc/redoma/agents.txt` (a whole line per name, as `grep -qxF`
  reads it; the file and every directory above it owned by root and
  writable by no one else, no symlink, or nothing is allowed), one live
  holder per name and per namespace. Between Sources, `agent.Merge` gives a
  name to the launcher whenever it claims it; a hook claim waiting on it is
  refused, any other is kept and offered again once the launcher lets the
  name go.
- **An agent's namespace is its own.** The control plane refuses a start in
  a namespace another agent has (dev/inode). The hook registers only a
  container whose spec has the runtime create a new network namespace, so
  not `--network host`, `container:`, `ns:` or a pod sibling (see "The OCI
  hook").
- **Never log a private key or a credential.** Errors about a key say what
  is wrong, not what it is.
- **Packing is deterministic.** A SotW update resends every listener, so an
  unchanged one must be the same bytes (`lds.mustAny`).

## xDS served

Over ADS, SotW only (the bootstrap's `api_type: GRPC`; delta streams are
refused), on `/run/redoma-xds/xds.sock`, to any node (there is one client):

| Type | Name | What |
|---|---|---|
| Listener | `agent_<name>_model`, `agent_<name>_egress` | per running agent, 127.0.0.1:8080 and 127.0.0.1:3128 in its Source's namespace |
| Listener | `agent_<name>_relay` | per running agent, 127.0.0.1:7070 in the same namespace, unless `--relay-socket` is empty |
| Cluster | `agent_<name>_relay` | per running agent whose SVID and the bundle are in the snapshot, with a relay |
| Secret | `agent/<name>` | the agent's X.509 SVID, `tls_certificate` (chain and PKCS#8 key, PEM inline) |
| Secret | `spiffe://<td>` | the trust domain's bundle, `validation_context.trusted_ca` |

A secret is only sent when Envoy asks for it by name: the relay clusters ask
for their agent's and the bundle, over ADS. The bootstrap's own clusters,
`anthropic` and `egress_dfp`, are the host's: `examples/envoy.yaml` is one
such bootstrap, and `//xds/internal/lds:lds_test` holds its cluster names
and the egress DNS cache to what the listeners use.

The relay listener is the agent's stream to the relay (see "The relay"):
HTTP/2 without TLS, only `/redoma.relay.v1.Relay/Connect` (anything else a
403), no route timeout and no maximum stream duration, a 5-minute stream
idle timeout (the shim's heartbeat is every 30s), pings every 30s (10s to
answer) and a connection idle timeout of an hour on both hops. Its route
does not validate the cluster, which comes over CDS once the SVID is there.
The cluster connects to `--relay-socket` (`/run/redoma-relay/relay.sock`)
with TLS 1.3, the agent's SVID as its client certificate (`agent/<name>`),
and requires of the relay a certificate of the bundle that carries
`--relay-id` (`spiffe://<td>/host/relay`). One cluster per agent: a cluster
has one client certificate, and an agent's own cluster is not rebuilt, its
stream drained, when another agent comes or goes.

The model listener forwards two exact paths (the query string aside: Claude
Code sends `?beta=true`), `/v1/messages` and `/v1/messages/count_tokens`;
any other path is a local 403 that never reaches the API, so the credential
is only ever spent on inference. Claude Code's other calls to
`api.anthropic.com` get that 403 too; a path it cannot do without is added
to `lds.modelPaths`, never a prefix. The two paths go to the bootstrap's
`anthropic` cluster with the API key injected as `x-api-key` (the
credential_injector filter, `overwrite`; any `authorization` header the
agent sent is stripped) from the SDS file
`/etc/redoma/secrets/anthropic-key.yaml`, a `generic_secret` that Envoy
reads and this service never does.

## The OCI hook

A rootful Podman container started with `--annotation redoma/agent=<name>`
(the key is `agent_annotation`) is an agent, with no launcher. An
annotation, not a `--label`: Podman's hooks match OCI annotations
(`when.annotations`), and labels are not passed to the runtime. The hook
definitions in `ocihook/hooks.d` match the default key; a host that changes
`agent_annotation` needs definitions that match its key.

- **createRuntime** (runtime namespace, root, before the container's
  process runs): the hook bind-mounts `/proc/<pid>/ns/net` at
  `/run/netns/redoma-oci-<container ID>`. That path is never reused, and
  Envoy is never handed `/proc/<pid>` (PIDs are reused). `/run/netns` is
  where the host Envoy sees new namespaces (rslave in a container). The
  hook then registers `{container, agent, netns}` on
  `/run/redoma-xds/register.sock` and waits for the answer, which comes
  once Envoy has reported a snapshot with the agent's listeners applied
  (`agent.Event.Bound`). A refusal or a timeout (`--timeout`, 30s; the
  control plane's `--bound-timeout` is 25s) fails the start, and the hook
  undoes the mount.
- **Who may register which name** (the name is the identity): the socket
  is 0600 `redoma-sds` and the peer must be uid 0 (SO_PEERCRED), so
  rootless Podman, whose hooks run as the user, can never claim one. The
  name must be in `/etc/redoma/agents.txt`, which must be owned by root
  and writable by no one else, like each directory above it, and not be a
  symlink (otherwise no name is allowed); a name is allowed when a line is
  exactly the name. One live container holds a name at a time, and none
  holds one a launcher runs (`agent.Merge`).
- **Its own namespace, new, empty.** An agent container runs
  `--network none` in the host user namespace (no `--userns`), and the
  hook enforces both from the bundle's `config.json` (read strictly: exact
  keys, no repeated key, no namespace type twice, an absolute bundle path).
  It requires a `network` namespace with no `path`, which the runtime
  creates for this container, and no `user` namespace entry:
  - no network namespace is the host's (`--network host`);
  - a path is a namespace joined, not created (`--network container:<x>`,
    `ns:<path>`, a pod sibling), or the default bridge's (libpod creates
    `/run/netns/netns-<uuid>` and passes the path);
  - a `user` namespace (`--userns=auto` or any other) makes Podman create a
    bridge's network namespace without a path and attach the network after
    the runtime made it (`postConfigureNetNS`), so no path would not prove
    `--network none`.

  Envoy's listeners are then all the agent reaches. On the namespace it
  mounts, the hook opens `/proc/<pid>/ns/net` once, compares that open file
  with `/proc/1/ns/net` and bind-mounts it through `/proc/self/fd/<n>`, so
  nothing can be swapped in between the check and the mount. The control
  plane also refuses a namespace another agent has.
  **What remains assumed:** nothing enters the agent's namespace after it is
  created. A later container started with `--network container:<the agent>`
  (it has no annotation, so no hook runs for it), or `nsenter` by root,
  would share the agent's proxy and identity. Both need root on the host,
  which owns every identity here anyway.
- **No way to a host service.** In the host user namespace a container's
  ids and capabilities are the host's, so the hook refuses, before any mount
  or registration (`hook/process.go`, `spec.go`, `mounts.go`):
  - root: no `process.user`, uid 0, or gid 0 in the gid or
    `additionalGids` (an agent runs `--user`);
  - a service's uid, gid or `additionalGids` entry (`hook.ServiceIDs`: 961
    redoma-sds, 963 redoma-relay, 964 redoma-lead, and whatever ids
    `/etc/passwd` and `/etc/group` give those names; a lookup that fails is
    logged and the static ids still hold): `--user` or `--group-add` with one
    reaches that service's sockets;
  - `noNewPrivileges` not true (`--security-opt no-new-privileges`);
  - any capability but `AllowedCaps` (CAP_KILL, CAP_NET_BIND_SERVICE,
    CAP_AUDIT_WRITE) in any set, bounding, effective, permitted, inheritable
    or ambient (`--cap-drop all`);
  - a `linux.devices` entry other than the runtime's defaults (null, zero,
    full, random, urandom, tty, ptmx, by type and number: `--device`), and a
    `linux.resources.devices` allow rule of anything else
    (`--device-cgroup-rule`); null for either is none. That allowlist
    (`defaultDeviceRules`) is written from crun's defaults: a rule it does
    not know fails closed, refusing the start;
  - a namespace other than the network's joined by path (`--pid`, `--ipc`,
    `--uts`, `--cgroupns` of another process; a mount namespace by path),
    and no PID namespace of its own (`--pid host`): the agent would see the
    host's processes, and through `/proc/<pid>/root` of one with its uid
    every file that process reaches, with no mount;
  - a mount of another type than a bind or the runtime's own file systems
    (proc, tmpfs, devpts, mqueue, sysfs, cgroup, cgroup2); a bind whose
    source is not absolute (the runtime takes it against the bundle) or does
    not resolve on the host (it could not be checked: fail closed); a bind
    whose source, cleaned or resolved through symlinks, is, is inside, or
    holds one of `hook.ForbiddenSources` (`/run/redoma-xds`,
    `/run/redoma-relay`, `/run/redoma-relay-lead`, `/run/redoma-spire`,
    `/run/redoma`, `/etc/redoma`, `/run/podman`, `/var/lib/containers`,
    `/proc`, `/dev`), so neither `/run` nor `/` either, nor the directory
    of any SPIRE socket `/etc/redoma/redoma.conf` names, as written and as
    it resolves (`forbidden.Sources`): a host that moves a socket keeps it
    forbidden. This check is the one barrier against a bind of a SPIRE
    socket file itself, which SPIRE makes `0777`. The one exception is
    the container's own `shm`, `passwd` and `group` in its userdata under
    `/var/lib/containers/storage/<driver>-containers/<its id>/userdata/`,
    which Podman 4.9 binds itself (its `hosts`, `resolv.conf`, `hostname`,
    `.containerenv` and `run/secrets` are under the runroot,
    `/run/containers`, which is not forbidden). A `--secret` (bound from
    `<graphroot>/.../userdata/secrets`) and a named or anonymous volume
    (`<graphroot>/volumes`) are refused: an agent has neither;
  - what the container has mounted, read from its `/proc/<pid>/mountinfo`
    at createRuntime and compared with the host's by device and path within
    the device (the mounts under the container's rootfs, before the pivot),
    when it reaches a forbidden place by any name (`/var/run`, another bind
    of `/run`, a symlink swapped after the check above), or any part of a
    file system that is forbidden whole (the host's `/dev` devtmpfs, a tmpfs
    mounted inside `/var/lib/containers` such as another container's shm).
    The runtime's masks are not refused: the host's `/dev/null` bound on
    each of the spec's `maskedPaths` (crun binds it on `/proc/kcore` and the
    rest), and the container's own rootfs and shm. Its own rootfs is the
    overlay its storage records for its id (`containers.json`, or
    `volatile-containers.json` for a `--rm` container, whose record Podman
    4.9 marks volatile, in `<graphroot>/overlay-containers` or, with
    `transient_store`, `<runroot>/overlay-containers`: id, layer,
    `<graphroot>/overlay/<layer>/merged`), not the spec's `root.path`: a
    root that is another container's merged dir (`--rootfs`) or the host's
    `/dev/shm` is refused, and so is every other mount inside a forbidden
    path. `root.path` is resolved through symlinks first (one that does not
    resolve refuses the start), so a link to another container's merged dir
    is that dir. A mountinfo that cannot be read refuses the start. The
    graphroot and runroot are rootful Podman's defaults,
    `/var/lib/containers/storage` and `/run/containers/storage`
    (`hook.Graphroot`, `hook.Runroot`); the hook does not read
    `/etc/containers/storage.conf`. A host that moves them is not covered
    (its storage is no forbidden source, and its records are not read)
    until the constants and `hook.ForbiddenSources` follow; one on another
    driver than overlay has no rootfs that is an agent's own. Tested on a
    real capture (podman 4.9.3, crun: `hook/testdata/podman-4.9.3`);
  - a spec key that is a spec key in another case (`Mounts`, `Source`, and
    Unicode's folds as encoding/json makes them: `ſource` is `source`, the
    Kelvin sign is k; `strings.EqualFold`): runc folds case and crun does
    not, so the two, and this hook, would read it apart; and two keys of one
    object that differ only in case.

  **What the hook guarantees**, then, for a container it registers: its
  process starts as a user that is neither root nor a service, in the host
  user namespace and a network namespace of its own, with no dangerous
  capability, unable to gain one through exec, with the runtime's default
  devices only, and with none of those places mounted under any name.
  **What it assumes:** the runtime does what the spec says; nothing grants
  the container more after its start (`podman exec --privileged`,
  `nsenter`, a second container joining its namespaces, a mount propagated
  into it later: all root's, who owns every identity here anyway); the
  host's ids for those names are in `ServiceIDs` or the host's files; no
  hard link to one of those sockets exists elsewhere (a hard link is the
  same file under another path and another place, which neither check
  sees; the services' directories are 0750 and their own, so only root
  could make one).
- **The process:** once the namespace is mounted, the hook opens a pidfd
  for the container's process and binds it to it (`hook.BindProcess`): its
  network namespace is the one mounted, its cgroup is the container's
  (`libpod-<id>`), and it is still alive after both reads. A process in
  another network namespace than the one mounted, or in another
  container's cgroup, fails the start (`hook.ErrPIDReused`: its PID was
  reused, and the namespace mounted may be another process's);
  one that fails the checks otherwise is logged and registered without a
  pidfd: in Broker mode its agent then gets no SVID. The register
  carries it as `SCM_RIGHTS` data (`register.WriteRequest`); `redoma xds`
  keeps it in systemd's file descriptor store under the container's ID, so
  a restart gets it back, and gives a name back to its registration only
  when exactly one live pidfd is stored under it.
- **poststop:** unregister (best effort) and unmount. A registration whose
  mount is gone (a crash, an unmount by hand) is removed by the rescan.
  Registrations are recorded in `/run/redoma-xds/registrations.json`, so a
  restart of `redoma xds` keeps them.
- **Selectors:** `redoma:agent:<name>` (`hooksource.Selectors`).

The definitions go in `/usr/share/containers/oci/hooks.d`, not
`/etc/containers/oci/hooks.d`: Podman 4.9 reads only the directories of
containers.conf's `hooks_dir`, which defaults to
`/usr/share/containers/oci/hooks.d` alone (containers/common v0.57.4
`pkg/config/default.go:77,344`; podman `libpod/container_internal.go:2282-2283`).
The `/etc` directory is read only when `hooks_dir` is set empty
(`container_internal.go:2259-2263`). Podman skips a missing hooks directory
without a word.

`containers.conf.d/50-redoma-hooks.conf` pins it: `hooks_dir` is an
attributed list, so a containers.conf or a drop-in that sets it replaces
the default, and the hook would silently stop running. The drop-in appends
the directory (`{append=true}`, containers/common
`internal/attributedstring/slice.go`) to whatever was set before it. Files
are read in this order: `/usr/share/containers/containers.conf`,
`/etc/containers/containers.conf`, `/etc/containers/containers.conf.d/*.conf`
sorted by name, then a rootless user's own; `CONTAINERS_CONF` replaces them
all (`pkg/config/new.go:153-186`). So a drop-in sorting after `50-` that
sets `hooks_dir` without `{append=true}`, or `CONTAINERS_CONF`, still turns
the hook off. README.md's manual check shows that: no "hook registration"
line.

Rootless Podman reads the same directory, so it runs the hook too, as the
user, and is refused.

## Launchers

A launcher is a host process (root) that starts agent containers itself
instead of through the hook: it creates the named namespace
`/run/netns/redoma-<name>`, starts the container in it, and writes an empty
file `/run/redoma/active/<name>`; `activedir` reads both. It names its agents
from the same allowlist rules (`allowlist.ValidName`; a whole line of
`/etc/redoma/agents.txt`) and passes the hook's process checks with
`--user`, `--cap-drop all` and no-new-privileges. The hook is the way README.md
documents; the launcher Source stays for hosts that run one.

## The relay

The lead talks to the agents through it, and only through it; agents never
talk to each other. In each container, `redoma-shim` holds one long-lived
bidi stream, `Relay.Connect` (`api/redoma/relay/v1/relay.proto`), to 127.0.0.1:7070
in its namespace; the host Envoy's per-agent relay listener forwards it to
`/run/redoma-relay/relay.sock` over mutual TLS with the agent's own SVID
(the container never sees it). The lead uses the `Lead` service on
`/run/redoma-relay-lead/lead.sock` through `redoma msg` or `redoma mcp`.

- **The client certificate is the identity, and nothing else is.** A
  connection is accepted only with a certificate that chains to the trust
  domain's bundle and has exactly one URI SAN,
  `spiffe://<td>/agent/<name>`, with a valid name that
  `/etc/redoma/agents.txt` has (read like the hook reads it,
  `common/allowlist`); the allowlist is checked again when a stream opens.
  No certificate, another trust domain, another path, two IDs or a name not
  allowed fail the handshake (`relay/internal/mtls`).
- **One stream per agent; the newer wins.** A second stream of the same agent
  ends the first with `Aborted` and takes over its queue once the first has
  stopped. A reconnect is what follows a connection that broke without either
  end noticing, so the older stream is the dead one; both carry the same
  agent's certificate, so replacing is no escalation.
- **At-least-once, in order** (`common/reliable`). Every envelope but an Ack
  and a Heartbeat has a seq, per direction, kept by its sender on disk until
  acked (cumulative acks). On a reconnect both ends resend what is not acked;
  each receiver takes a seq once (its record: the peer's queue epoch and the
  last seq taken, saved before it is acked). A queue's epoch (metadata
  `redoma-epoch`, both ways) tells a receiver its peer's queue was made
  again from nothing, so its seqs starting at 1 are not taken for old ones.
  The relay syncs its queue on every push (tasks, replies and cancels are
  few); its record and acks follow every `--ack-every` (200ms).
- **The agent side is hostile.** The turn runs as the shim's uid and can
  dial the relay listener itself, so nothing the shim does is assumed
  (`relay/internal/hub/limits.go`). Every TurnEvent, Question and Message
  must name a task the relay gave THAT agent and has not closed (a task
  closes with its last event; a cancelled one takes only its turn's end);
  messages go to the lead only; ids and texts are bounded (a chunk 192 KiB,
  the shim splitting longer lines; a question or message 16 KiB; question
  ids `[A-Za-z0-9_-]{1,64}`); an agent has at most 32 questions pending,
  256 held messages and 256 KiB of them, 256 bytes counted per message
  besides its text (8 MiB across agents) and 64 open tasks; turn
  output and notes are rate limited per agent. A last event's error over
  4 KiB is cut, not dropped, so the task still ends. What fails is acked and
  dropped (never resent, never stored), and the lead's watches get a
  `Dropped` event, at most one a second per agent. The agents' server takes
  an envelope of 256 KiB at most and one stream per connection.
- **What the lead has not seen is kept.** Questions stay pending until
  answered and are replayed to every new `Watch`; a message stays held until
  a watch has sent it, so one that arrives while nobody watches goes to the
  next. Ids the lead sees are `<agent>/<id>`: tasks are numbered by the
  relay, questions by the shim.
- **Watches are fair.** A watch keeps a queue per agent and sends from them
  in turn; questions and messages go ahead of turn output and are not
  dropped for it; an agent's turn output past 2 MiB of lag is dropped, that
  agent's only, with a `Dropped` marker. A watch is never cut off.
- **State:** `/var/lib/redoma-relay/agents/<name>/`: `out/`, `in.json`, and
  `lead.log`, a journal of the open tasks, pending questions and held
  messages: a record is written before it is applied, compacted now and
  then, and synced before every ack, which covers only what the sync did
  (`Inbox.FlushAfter`); a torn last line is dropped, a bad line elsewhere
  refuses the journal. A failed write is cut back out; one that cannot be
  cut back, or a compacted journal that cannot be reopened, breaks it (a
  failed compaction alone does not fail the append it follows).
- **One broken agent, not a broken relay.** An agent whose state cannot be
  opened (a refused journal, a record that does not parse) is quarantined:
  logged, its streams refused (`Unavailable`) and the lead's calls for it
  too (`FailedPrecondition`), each later use trying the open again, while
  every other agent is served. A journal that breaks while the relay runs
  refuses the agent the same way, takes and acks nothing more of it, and
  tells the lead's watches once. Every new watch is told of each such agent
  first, with a `Dropped` whose reason says its state is broken.
- **Its own identity** is `spiffe://<td>/host/relay`, from the host SPIRE
  agent's Workload API (`spire_workload_api`, go-spiffe's X509Source: it
  waits for the first SVID, follows rotations live and redials a restarted
  agent). SPIRE attests it by uid: it runs as `redoma-relay` (963:963),
  never as 961 (`redoma-sds`, the delegate). Agents are verified with
  go-spiffe's `tlsconfig.MTLSServerConfig` and an authorizer that takes the
  trust domain and `/agent/<name>` only, so the delegate's, the relay's own
  and any other ID are refused.
- **Sockets.** Both are 0660 and the relay's own, their group set explicitly
  after the bind and checked, never inherited from the directory:
  `relay.sock` in `redoma-relay` (963), which the host Envoy joins;
  `lead.sock` in `redoma-lead` (964), the users who may task agents. The two
  groups must differ (the relay refuses to start otherwise): a lead user must
  not reach the agents' socket.
- **The unit's sandbox** (`relay/redoma-relay.service`): no capabilities,
  AF_UNIX only, and a syscall filter of `@system-service` without
  `@privileged @resources`, then `@chown` given back: the relay chgrps its
  sockets and their directories, and `@privileged` holds `@chown` (without
  it the relay dies of SIGSYS at its first chown). Without capabilities a
  chown only moves a file of its own to a group it is in.
  `relay/unit_test.go` reads the filter and fails when it denies a syscall
  the relay makes; a group added to the denied ones must list the relay's
  syscalls it holds there.

## redoma msg and redoma mcp

The lead's clients of the `Lead` service on
`/run/redoma-relay-lead/lead.sock` (`--socket`), which only members of
`redoma-lead` (964) reach: `redoma msg send`, `reply`, `cancel` and `watch`
on the command line, and `redoma mcp`, the same as MCP tools on stdio for a
lead that is a Claude Code session. Claude Code's Bash sandbox on Linux
refuses `socket(AF_UNIX)`; an MCP server runs as the user's own process,
outside it, with the user's groups. It dials nothing but the lead socket.

- **Tools.** `send(agent, text)` returns the task id; `reply(question,
  text)` and `cancel(task)` take `<agent>/<id>`; `poll(agent?, cursor?,
  wait_seconds?, max_bytes?, raw?)` returns `{cursor, events, more, omitted,
  watch}`. Inputs are checked as the relay checks them (an agent name, a text
  of 1 byte to 1 MiB, ids `<agent>/[A-Za-z0-9_-]{1,64}`), and unknown
  arguments are refused.
- **One Watch per process** (`msg/internal/feed`), of every agent, redialled
  with a backoff when it breaks (`watch` in a poll says which), into a buffer
  read by cursor. A cursor is `<epoch>.<n>`; one of an earlier process starts
  over from the oldest event buffered (`reset`). A poll of one agent passes
  over the others' events, which it does not return: its cursor,
  `<epoch>.<n>.<low>`, also keeps `low`, up to which every event was
  returned (`feed.Pos`), and a poll from it, of all agents or any one,
  offers the events from there up to `n` that no poll has returned since:
  what it passed over is seen, never lost, and nothing returned comes again
  (so neither does a question already answered). `low` moves on once what
  was passed over is returned.
- **What the lead must see is kept.** A question or a message stays in the
  buffer until a poll has returned it; past 8 MiB of them unreturned the
  process stops reading the Watch (the relay keeps them; it never drops them
  for a slow watch), and `watch` in a poll says `paused`: polls of one agent
  only can cause it, a poll of all ends it. The relay replays the pending
  questions to every new Watch; one already buffered is not buffered twice.
  A held message the relay sent to this Watch is this process's to keep: one
  that exits before a poll loses it.
- **Turn output is bounded** (32 MiB, 50000 events): the oldest goes first,
  and what no poll returned is counted by one `dropped` event per agent.
- **Compact by default**: the assistant's text and the names of the tools it
  used, each result and turn end, questions and messages whole, the relay's
  drops; the rest (system/init, tool results, partial messages, the parts of
  a line over 192 KiB) is counted in `omitted`. `raw` gives each event as
  `redoma msg watch` prints it. A response stays under `max_bytes` (default
  64 KiB, 4 to 256 KiB) and the rest follows from its cursor; one event over
  the bound alone is cut to fit (`truncated`), except a question or a
  message, which comes whole (it may go once returned). `wait_seconds` (at
  most 50, under the MCP call timeout) long-polls when there is nothing new.

## redoma-shim

The other end of the relay's stream, in each agent's container: the runtime
image's entrypoint. It connects to 127.0.0.1:7070 (`--relay`), the host
Envoy's relay listener in the container's namespace, in plain HTTP/2: the
mutual TLS is Envoy's, with the agent's SVID, which the container never
holds. It reconnects with a jittered doubling backoff (`--min-backoff` 500ms
to `--max-backoff` 30s) and heartbeats every 30s.

- **One turn at a time.** Tasks queue in order; each is one
  `claude -p --output-format stream-json --verbose --mcp-config <the shim's>
  --allowedTools mcp__redoma__ask_lead,mcp__redoma__send_message
  [--resume=<session>] [--claude-arg ...]`, run in `--workdir` (`/work`) with
  the task's text on its standard input. Each line of its output is a
  TurnEvent (a line over 192 KiB in parts, all but the last `partial`); the
  last event has `done`, and `error` when claude failed, was cancelled, or
  the shim stopped in the middle (reported `interrupted` at the next start,
  not run again: the workspace has what the turn did, and the lead decides).
  A task that ended (ran, cancelled, interrupted) is remembered (the last
  1024), so the relay delivering it again is not a second run.
- **One session.** The session id claude's lines carry is kept in
  `<state>/session` and resumed by the next turn, as one argument,
  `--resume=<id>`. The turn can write that file, so only a UUID is resumed:
  anything else is logged, dropped, and a new session starts. A resume that
  claude answers with "No conversation found" (a container without that
  session's files) forgets the session and runs the turn again in a new one;
  any other failure fails the turn and keeps it. That wording is claude's,
  pinned in `sessionNotFound` (and its test): a Claude Code upgrade in the
  image must recheck it, since claude gives no sturdier signal (stream-json
  has no line yet, and the exit status is 1 for any failure). Resuming
  across containers needs claude's own session files (`~/.claude`) to
  persist: mount them.
- **PID 1.** The shim is the container's init: a child subreaper
  (`PR_SET_CHILD_SUBREAPER`) that reaps, on SIGCHLD and every second, every
  zombie child it does not wait for itself, found in /proc rather than with
  `wait4(-1)`, which would take claude's exit status (`shim/internal/reaper`).
  Running the container with `--init` as well does no harm.
- **The turn is the shim's subtree.** The shim starts nothing but the turn,
  so a cancel ends every process below it, whatever its process group or
  session (a `setsid` daemon included): SIGTERM, then SIGKILL after
  `--kill-grace` (10s), each signal through a pidfd to a process found below
  the shim after the pidfd was opened (so never to a pid reused meanwhile),
  the SIGKILLs again and again until nothing is left (a process forked
  meanwhile is caught by the next scan). When claude exits, what it left
  running goes the same way, and claude's output and error pipes are the
  shim's own, so a leftover that holds them cannot hang the turn. The turn's
  end is sent first, its error naming any survivor, so the task closes and a
  Cancel is answered whatever happens next; then no turn starts until the
  last one's processes are gone. One that survives SIGKILL (stuck in the
  kernel) is logged and killed again every 5s, and the task next in the
  queue gets a TurnEvent (`{"type":"redoma_shim", "subtype":"waiting",
  "error":...}`, not its last) saying why it waits, at once and every
  minute. A task still queued is dropped. Either ends `cancelled`.
- **What the shim sends is valid.** Every string it sends is made valid
  UTF-8 (claude's stderr is cut at any byte), a done's error is cut to
  4 KiB and a question or message over 16 KiB refused to the tool; a done
  that still cannot be queued is queued with a fixed error, so a task always
  ends on the relay.
- **ask_lead and send_message** are the tools of `redoma-shim mcp`, the MCP
  server claude starts from `--mcp-config`. It reaches the daemon on
  `<state>/shim.sock` (0600; `REDOMA_SHIM_BRIDGE`). ask_lead sends a
  Question with the turn's task and blocks until its Reply, the turn's end
  or a cancel; claude's tool timeout is a day (`MCP_TOOL_TIMEOUT`). Both
  need a running turn: the relay takes nothing without its task.
- **State:** `--state-dir` (`/work/.redoma`, in the workspace so that it
  outlives the container): `out/` and `in.json` (common/reliable, saved on
  the way out too), `tasks.json` (the tasks received and not yet run, saved
  before they are acked, and the ended ones), `session`.

## Tests

- Unit tests sit next to the code and run under `bazel test //...`.
- No test needs root, a real SPIRE, a real network namespace or a real Envoy.
  The SPIRE side is a fake Delegated Identity server (`identitytest`), in
  memory over bufconn; a launcher's state is a directory in `t.TempDir()`.
  The tests of the real unix sockets skip where the environment refuses
  `socket(2)` (some sandboxes do), and fail there instead when `CI` is set
  (`--config=ci` passes it to tests), so CI never skips them.
- Run the tests with `--@rules_go//go/config:race` after touching
  `controlplane` or `identity`.
- Never touch `/run/redoma`, `/etc/redoma`, systemd, Podman or SPIRE on the
  host from a test or a script.

## The package

`//deb` builds `redoma_amd64.deb` and `redoma_arm64.deb` (rules_pkg's
`pkg_deb`), each with the binary cross-built for its architecture
(`platform_files`). `deb/BUILD.bazel` lists what goes in: the binary, the
units, the hook's definitions and drop-in, `deb/sysusers.conf` as
`/usr/lib/sysusers.d/redoma.conf`, `redoma/redoma.conf` and the drop-in as
conffiles, and the docs (`deb/copyright`, machine-readable) and examples.
Never the shim, a secret, or anything of SPIRE or Envoy.

The packages' version is stamped (`deb/deb_version.sh`, from VERSION and
`bazel/workspace_status.sh`):

| Commit | Version |
|---|---|
| tagged `v<VERSION>` | `<VERSION>` |
| tagged `v<VERSION>-rc.<n>` | `<VERSION>~rc.<n>` |
| after candidate `n` of VERSION | `<VERSION>~rc.<n>.git<commit time>.<sha7>` |
| any other | `<VERSION>~git<commit time>.<sha7>` |
| unstamped build | `<VERSION>~dev` |

A candidate's `n` counts from 1, without a leading zero: a tag such as
`v<VERSION>-rc.0` or `v<VERSION>-rc.01` is no candidate, and the release
workflow refuses it. A snapshot of a tree with uncommitted changes ends in `.dirty`. `~` sorts
first, so snapshots order by time, a candidate's snapshots sit between it
and the next candidate, and the release upgrades them all. Bump VERSION
right after a release, or the next snapshots sort below it.
`//deb:deb_version_test` holds that order to `dpkg --compare-versions`.

Packages built before the version was stamped are all `0.1.0`, which sorts
above every `0.1.0~` snapshot: a host that installed one removes it
(`sudo apt remove redoma`; its conffiles stay) before a snapshot will
install over it.

The maintainer scripts:

- `postinst` (configure) runs `systemd-sysusers` on the package's file,
  then fails, naming each, when `redoma-sds`, `redoma-relay` or
  `redoma-lead` has another id than 961, 963 or 964: the hook refuses those
  ids to agents and SPIRE attests the services by them, so a package that
  installs with other ids would leave a host open or broken. It reloads
  systemd and restarts the services only when they run (an upgrade); it
  enables nothing and adds no one to `redoma-lead`.
- `prerm` stops the services on removal; `postrm` reloads systemd. The
  users, the groups and `/var/lib/redoma-relay` stay.

`//deb:deb_test` holds each package to its exact file list, modes, owners,
conffiles, control fields and stamped version, checks that its binary is an
ELF of its architecture, and runs the packaged `postinst` against fake ids. A file added to the
package is added to that list.

## The runtime image

`runtime/` builds two images with rules_img and no Containerfile, each for
linux/amd64 and linux/arm64 (an index of both):

- `//runtime:base_index`, **redoma-base**, with no Claude Code in it:
  - Debian bookworm packages (`ca-certificates`, `git`, `jq`, `python3`,
    `ripgrep`, and a shell with the usual tools; MODULE.bazel, `apt`) and
    their whole dependency closure, for both architectures, resolved by
    rules_distroless from snapshot.debian.org at one instant and fetched by
    sha256; Bazel keeps the resolution in `MODULE.bazel.lock`. No
    maintainer script runs: `//runtime` makes what they would (the CA
    bundle, `/etc/passwd` and `/etc/group` with root and nobody, and
    `/var/lib/dpkg/status`, what scanners read), plus Debian's `/etc/nsswitch.conf` (`hosts: files
    dns`, so localhost never waits on DNS) and an `/etc/hosts` with
    localhost, the base Podman writes a container's hosts file from
    (without it Podman takes the host's). New packages: move the snapshot
    in MODULE.bazel and run `bazel mod tidy`.
  - `/var/lib/dpkg/status` names every package installed, dependencies
    included: rules_distroless writes it from the packages `apt.install`
    lists only, so MODULE.bazel lists the whole resolved closure.
    `//runtime:dpkg_status_test_amd64` and `_arm64` hold the file to the
    packages installed (each with Version, Architecture and Status) and
    name what is missing on either side; a snapshot that adds a
    dependency fails there until it is listed.
  - `redoma-shim` at `/usr/local/bin/redoma-shim`, the entrypoint, built in
    the same graph for the same platform.
  - The config has no environment but `PATH`, no user (the container's
    `--user` sets one), no working directory, and the labels
    `org.opencontainers.image.source`, `.revision` (the commit), `.title`,
    `.description`, `.licenses` (`Apache-2.0`) and `.version`: the
    packages' version, from the same stamped `//deb:deb_version` (so
    `0.1.0~rc.2` for `v0.1.0-rc.2`; see "The package"). An image and the
    packages of one build always agree, and a host can check its image
    against the package it installed.
  - `//runtime:base_push` pushes it to `quay.io/redoma/redoma-base`: both
    architectures' manifests by digest, then the index, tagged with the
    same version with `-` for `~` (`//runtime:base_tag`, `0.1.0-rc.2`), and
    no other tag. Only the release workflow runs it.
  - **The registry is set up once, by hand**, before the first release
    that pushes the base: the Quay organization `redoma`, its public
    repository `redoma-base`, a robot account with write on it, and that
    account's name and token as the secrets `QUAY_USERNAME` and
    `QUAY_PASSWORD` of the GitHub Environment `release` (environment
    secrets, not repository secrets), whose deployment rule limits it to
    `v*` tags. The release workflow fails at its login when either secret
    is missing.
  - It redistributes Debian's binary packages (GPL and LGPL among them):
    each one's `copyright` and `/usr/share/common-licenses` stay in it
    (`base_test.yaml` checks a few), and README.md, "Third-party software
    in the base", says where their sources are. A package added to
    MODULE.bazel is redistributed too: check its license allows that.
- `//runtime:image_index`, **redoma-runtime**, the base and Claude Code's
  native binary at `/usr/local/bin/claude` (0755), the `package/claude` of
  `@anthropic-ai/claude-code-linux-x64` or `-arm64`, exactly as the tarball
  has it (Anthropic's terms require it unmodified: never patch or strip
  it). Node is not in the image. It is built `FROM` the base (rules_img's
  `base`), the example of an image of one's own: the same entrypoint and
  `PATH`, and its own `.title`, `.description`, `.licenses`
  (`NOASSERTION`) and an empty `.source`. Inherited, the base's labels
  would claim Apache-2.0 for an image with Claude Code in it, and link an
  image pushed to a private registry to this repository; README.md's
  Containerfile example overrides them the same way.
- Nothing in `/etc/claude-code` in either.
- `//runtime:base_test_amd64` and `_arm64`, `//runtime:image_test_amd64`
  and `_arm64` check each image's structure per architecture (paths, modes,
  owners, the entrypoint, `PATH`, the labels, the dynamic loader) from its
  config and file listing: `image_test.yaml` what both have,
  `base_test.yaml` the base's (no claude at a few fixed paths only),
  `runtime_test.yaml` the runtime image's. A Claude Code binary at another
  path is caught by `//runtime:base_tree_test_amd64` and `_arm64` (no file
  named `claude*` or with `anthropic` in its path, at any path of the
  base's packages and system files) and by `//runtime:base_deps_test`,
  which holds the base's whole build graph, its push included, to no
  target of Claude Code's (a `genquery`). `//runtime:base_version_test` holds both
  architectures' version label to `deb_version.txt` and the tag to it.
  `//runtime:claude_code_test` checks that each tarball is the version
  pinned and the architecture it was fetched for.
- **No layer lists a path twice.** A tar may hold two entries for one
  path, but containers/storage refuses to apply such a layer
  ("duplicates of file paths not supported"), so Podman cannot pull the
  image. Flattening several tarballs into one layer repeats their
  directories (`./etc/` once per tarball), so every `flatten` that feeds a
  layer sets `deduplicate = True`. `//runtime:base_layers_test_amd64` and
  `_arm64`, `//runtime:image_layers_test_amd64` and `_arm64` list every
  layer of each image (paths without `./` or a trailing `/`) and fail on
  any path listed twice.
- **The runtime image is never published from here.** Whoever runs
  redoma downloads Claude Code under Anthropic's terms and is responsible
  for complying with them, which is why redoma never publishes an image
  containing it: they build the image
  (`//runtime:image_load`, or one of their own FROM the base, README.md)
  and push it to a private registry of their own, never a public one: it
  carries Claude Code's binary. It has no push target,
  `//runtime:image_index` is private to its package so none can be added
  elsewhere, and CI only builds it, when its inputs change. Nothing of
  Claude Code's ever goes into the base: `base_deps_test`,
  `base_tree_test_*` and `base_test.yaml` fail on it.
- **Bumping Claude Code** is one change to `claude_code(...)` in
  MODULE.bazel: `version`, and the `sha256` of each tarball. A wrong hash
  fails the fetch; a tarball of another version or architecture fails
  `//runtime:claude_code_test`. The shim pins Claude Code's "No conversation
  found" wording (see "redoma-shim"): recheck it on a bump.

## Versions

- The binaries are pure Go (`pure = "on"`).
- `VERSION` is the release being worked towards, the base of the packages'
  version (see "The package"); the binaries report `git describe` of their commit
  (`bazel/workspace_status.sh`).

## Pull requests

- `main` is protected: squash merges only, and the `ci` check must pass.
  `ci` (`.github/workflows/ci.yml`) aggregates `lint` (the formatter and
  Gazelle), `gitleaks`, `diff` (bazel-diff's impacted targets) and `build`
  (builds and tests those, with `--config=ci`); `build` is skipped only when
  nothing is impacted. The images are built there when their inputs
  change, and never pushed: only the release workflow pushes the base.
- Releases are tagged on a commit of `main` after CI has passed on it.
  A tag `v<VERSION>` runs `.github/workflows/release.yml`.
  - Its `build` job (`contents: read`, plus `id-token` and `attestations`
    for the packages' attestation) fails unless the tagged commit is on `main`
    (`git merge-base --is-ancestor` against `origin/main`). It runs
    `//deb:deb_test` alone, not the rest of CI's build, and checks that the
    stamped `deb_version.txt` is the tag's version. It then uploads
    `redoma_<asset version>_{amd64,arm64}.deb` and their `SHA256SUMS` as an
    artifact (`bazel/release-assets.sh`), after attesting the build
    provenance of each of the three (`actions/attest-build-provenance`,
    `subject-path`; `gh attestation verify <file> -R bpalermo/redoma`).
  - Its `base` job, after `build` and the only one in the `release`
    environment, so the only one given the Quay secrets (with `id-token: write` for cosign and the attestation, and
    `attestations: write`), builds
    redoma-base from the tagged commit, runs its tests (`base_test_*`,
    `base_tree_test_*`, `dpkg_status_test_*`, `base_deps_test`,
    `base_version_test`), and checks that its version is
    `build`'s and its tag the release tag without the `v`. It pushes it
    (`//runtime:base_push`), checks that the tag, as `quay.io` serves it,
    is the index it built (the sha256 of its bytes), signs that digest
    (never a tag) with cosign, keyless (Fulcio and Rekor, this run's OIDC
    identity; the installer action and cosign's version pinned), checks
    that the signature verifies as a consumer's `cosign verify` would
    (README.md), and attests the index's build provenance
    (`actions/attest-build-provenance`, kept in GitHub's attestation store,
    not pushed to the registry). A failure after the push leaves an image with no
    release; a re-run pushes the same digest again.
  - Its `publish` job runs only after `base` pushed the base, so no
    release names a base that is not in the registry. The only one with
    `contents: write`, it checks the sums and that the tag has no release
    yet, published or draft. It creates the
    GitHub Release as a draft, its notes naming the base by digest, how to
    verify its signature and provenance, and the
    snapshot.debian.org archives its packages, and their GPL and LGPL
    sources, come from (MODULE.bazel's, read by `base`), and
    checks that GitHub holds exactly the
    uploaded assets: each name, and each asset's `digest` against its line
    in `SHA256SUMS` (and `SHA256SUMS`'s own sum). Only then does it publish
    the release.
  - When the job fails after it created the draft, its last step deletes
    that draft (only a draft: a published release is never deleted, and the
    tag stays), and the failed jobs can be re-run (`gh run rerun <run id>
    --failed`) once the cause is fixed.

  If a draft is still there anyway (the clean-up itself failed, or the run
  was cancelled), a re-run stops at "no release of the tag yet". Make sure
  it is a draft, delete it, keep the tag, and re-run:

  ```bash
  gh release view <tag> -R bpalermo/redoma --json isDraft   # must say true
  gh release delete <tag> -R bpalermo/redoma --yes          # no --cleanup-tag
  gh run rerun <run id> -R bpalermo/redoma --failed
  ```

  A release that was published is never deleted to retry it: a bad
  candidate is followed by the next one, `-rc.<n+1>`.

  A tag `v<VERSION>-rc.<n>` does the same as a prerelease, its packages
  versioned `<VERSION>~rc.<n>`. Any other `v*` tag fails the run
  (`bazel/release-tag.sh`).

  The asset version is the packages' version with `~` as `-`, the tag's
  spelling: `redoma_0.1.0-rc.1_amd64.deb` holds version `0.1.0~rc.1`.
  GitHub renames an uploaded asset whose name has a character outside
  `[A-Za-z0-9._-]` or a period at either end (a `~` becomes `.`), and
  `SHA256SUMS` would then name files nobody downloads. The control
  `Version` keeps its `~`: dpkg orders by it. `release-assets.sh` refuses
  any name GitHub would rewrite and sums the files under their uploaded
  names; `//bazel:release_assets_test` holds it to that.
- GitHub Actions are pinned by commit SHA, with the version in a comment.
  Dependabot keeps those current, but not the `multitool.lock.json` pins:
  bump bazel-diff by hand, its version in every URL and the sha256s (GitHub's
  asset digests) together.
- Every commit is signed off (`git commit -s`, the Developer Certificate of
  Origin).
- This repository is public. Nothing in it names a real host, network,
  person's machine or account: examples use `example.org`,
  `redoma.internal`, the RFC 5737 documentation addresses (192.0.2.0/24,
  198.51.100.0/24, 203.0.113.0/24) and generic paths.
- What CI's `diff` job decides can be reproduced locally, with the same
  pinned bazel-diff, from the root of a throwaway clone (the script checks
  revisions out; never run it in the checkout you work in):
  `BASE_SHA=origin/main bazel/ci-impacted-targets.sh`, then read
  `.bazel-diff-out/impacted_{build,test}.txt`. bazel-diff is the prebuilt
  release binary pinned by URL and sha256 in `multitool.lock.json`
  (rules_multitool, `@multitool//tools/bazel-diff`; linux amd64 and arm64,
  macOS arm64). `BAZEL_DIFF=<path>` overrides it.
- Keep a pull request to one slice of behaviour, with its tests.
