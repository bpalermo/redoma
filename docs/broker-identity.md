# The agents' identities through SPIRE's Broker API

Status: design, not implemented. It moves `redoma xds` from SPIRE's
Delegated Identity API to the SPIFFE Broker API, behind a setting, with
the Delegated Identity API kept as the default until a host has run on the
Broker API.

## Why

`redoma xds` holds every agent's X.509 SVID and serves it to Envoy over
SDS; the agent container never sees it. Today it gets the SVIDs from the
Delegated Identity API on the SPIRE agent's admin socket, as the one
authorized delegate (uid 961, `spiffe://<td>/host/sds`). A delegate hands
SPIRE the selectors itself, `redoma:agent:<name>`, and SPIRE takes them as
attested: which container is which agent is decided by redoma alone.

The Broker API is where SPIFFE's delegation is being standardized. It
implements the SPIFFE Broker API specification: the broker names a
workload by a reference (here, a process ID) and SPIRE's own workload
attestors decide the selectors, rather than taking them from the caller. SPIRE
marks it experimental and may change it before it is stable; redoma
follows it anyway, and tracks it (see "Versions").

## How it works

```
podman run --annotation redoma/agent=<name> --label redoma.agent=<name> ...
   |
   | createRuntime: redoma oci-hook (root)
   |   mounts the netns, reads the container's init PID and its start time
   v
redoma xds  -- registration {container, agent, netns, pid, start_time} --
   |
   | pidfd_open(pid); start time checked; pidfd held while the agent runs
   |
   | mTLS with its own SVID (spiffe://<td>/host/sds, from the Workload API)
   | metadata broker.spiffe.io: true
   v
SPIRE agent, Broker API (UDS)
   | WorkloadPIDReference{pid}
   | -> unix attestor: unix:uid:1000, ...
   | -> docker attestor: /proc/<pid>/cgroup -> libpod-<id>.scope
   |      -> rootful Podman API: inspect <id> -> labels
   |      -> docker:label:redoma.agent:<name>
   v
entry spiffe://<td>/agent/<name> selected by docker:label:redoma.agent:<name>
   |
   v
SubscribeToX509SVID stream -> redoma xds checks pidfd and SPIFFE ID -> SDS -> Envoy
```

1. **The container.** A rootful Podman agent is started with both the
   annotation the hook matches, `redoma/agent=<name>`, and a label,
   `redoma.agent=<name>`. Both are needed: Podman runs OCI hooks by
   annotation, and SPIRE's docker attestor sees labels only (it reads them
   from the Podman API; annotations are not among the selectors it makes).
   The hook cannot set or read the label: Podman keeps labels in its own
   database and does not put them in the OCI spec.
2. **The hook** runs as today (createRuntime, root) and adds two fields to
   its registration: the container's init PID, from the runtime's state,
   and that process's start time (field 22 of `/proc/<pid>/stat`, clock
   ticks since boot), read right after it bind-mounts the namespace from
   the same PID. The init process cannot exit on its own at this point: the
   runtime holds it until the container starts.
3. **redoma xds** opens a pidfd for the PID (`pidfd_open(2)`), reads the
   start time from `/proc/<pid>/stat`, and accepts the registration only if
   it matches and the pidfd still reports the process alive (see "PID
   reuse"). It holds the pidfd for as long as the agent is registered. The
   registration file (`/run/redoma-xds/registrations.json`) records the PID
   and start time, and a restart of `redoma xds` reopens the pidfd and
   checks them again.
4. **Its own identity.** In Broker mode, `redoma xds` fetches its own X.509
   SVID, `spiffe://<td>/host/sds`, from the Workload API (`spire_workload_api`,
   the socket `redoma relay` uses) with go-spiffe's X509Source, and dials
   the broker socket with mutual TLS: it presents that SVID and accepts the
   SPIRE agent's SVID when it is a member of the trust domain under
   `/spire/agent/`. Every call carries the gRPC metadata
   `broker.spiffe.io: true`, which SPIRE requires.
5. **X.509 SVIDs.** For each agent, `SubscribeToX509SVID` with a
   `WorkloadPIDReference` naming the PID. SPIRE runs its workload attestors
   on the PID once, when the stream opens, and streams the SVIDs of the
   entries those selectors match, rotations included, for the life of the
   stream. `redoma xds` keeps the SVID whose SPIFFE ID is the agent's,
   `spiffe://<td>/agent/<name>`, exactly as `pickSVID` does today, and
   ignores the others. The stream is opened once Envoy has bound the
   agent's listeners (after the hook has returned, see "Podman's container
   lock"), and resubscribed with backoff while the agent is registered.
6. **The bundle** comes from `redoma xds`'s own Workload API stream (the
   trust domain's X.509 bundle), not from the Broker API: Broker's bundle
   streams take a workload reference too, and the bundle must be served
   while no agent runs.
7. **JWT SVIDs** (not served to Envoy yet; the interface is for the coming
   workload identity federation): `FetchJWTSVID` with the agent's PID
   reference, its audience and `spiffe_id` set to the agent's ID. SPIRE
   attests the PID again on every call, so each fetch costs a Podman API
   inspect; the consumer caches a token until it nears expiry.

The rest of `redoma xds` (LDS, CDS, SDS, the agent sources, the
registration's allowlist and ownership checks) does not change.

## Selectors and the threat model

### What changes

With the Delegated Identity API the agents' entries select on
`redoma:agent:<name>`, a type no attestor produces. Only the delegate can
present it, so only `redoma xds` can obtain an agent's SVID, and a process
inside an agent container gets nothing from SPIRE even if it reaches a
SPIRE socket.

With the Broker API the entries must select on what an attestor produces,
`docker:label:redoma.agent:<name>`. Any workload whose attestation yields
that selector gets the SVID, through the Broker API or through the
ordinary Workload API. The uid does not help: every agent runs as uid
1000, so `unix:uid:1000` tells no agent from another (or from a host user
with that uid). The selector is therefore a regression unless whatever can
carry the label is also unable to reach a SPIRE socket.

### Who can carry the label

- **A rootful container**, labelled by whoever runs rootful Podman: root,
  or whoever reaches `/run/podman/podman.sock`, which is root-equivalent.
  The OCI hook refuses an agent that mounts `/run/podman` or anything above
  it (`hook.ForbiddenSources`), so an agent cannot start a labelled
  container.
- **A rootless container**, labelled by any local user. SPIRE's docker
  attestor attests rootless Podman containers through the user's own
  socket, which that user controls: such a container can present any label.
  From spiffe/spire#7293 on, this is opt-in (`use_rootless_podman`, default
  false); before it, SPIRE always did it. The SPIRE agent must run a version
  with #7293 and keep `use_rootless_podman` false (see "Versions").
- **An image**: image labels become container labels unless the run
  overrides them. The runtime image sets no `redoma.agent` label, and
  Broker mode adds a test to `//runtime` that keeps it so. An agent run without `--label`, from
  an image that has one, gets that image's SVID offered by SPIRE, and
  `redoma xds` refuses it because its SPIFFE ID is not the agent's (step 5).

### The mitigation: agents never reach a SPIRE socket

The Workload API is the only door through which a labelled process other
than `redoma xds` could ask for an agent's SVID (the Broker API admits only
the brokers its configuration lists, by SPIFFE ID, at the TLS layer). It is
kept out of the agents' reach in three independent ways:

1. **No mount.** The OCI hook refuses to start an agent that has
   `/run/redoma-spire`, anything in it or any directory above it mounted, under
   any name, and checks the mounts the runtime actually made, not only the
   spec (`hook.ForbiddenSources`, the "SPIRE's workload API" case in
   `mounts_test.go`). All of SPIRE's sockets for redoma (Workload API,
   admin, broker) live under `/run/redoma-spire`.
2. **No network path.** The SPIRE agent serves the Workload API on a unix
   socket only, never on TCP, and the agent's own network namespace has no
   route to the host. Abstract unix sockets are per network namespace, so a
   path-less socket on the host is out of reach too.
3. **No permission.** The Workload API socket's directory grants nothing to
   others (no `o+x`), and its group holds only the services that need it:
   `redoma-sds` (961) and `redoma-relay` (963). The hook already refuses an
   agent running with a service's uid or gid. In Broker mode `redoma xds`
   refuses to start when the socket's directory grants any permission to
   others, so a host that relaxed it is told at once.

What remains is root, who can mount anything into any container, and who
can already obtain every identity on the host through the Delegated
Identity API or SPIRE's own key. The design does not try to defend against
root.

### What the Broker API itself admits

The broker block lists one broker, `spiffe://<td>/host/sds`, allowed one
reference type, `WorkloadPIDReference`, over UDS only. Its socket's
directory is `0750`, group `redoma-sds`. The broker can name any PID on the
host, so it can obtain the SVID of any workload SPIRE would attest: the
broker identity is as powerful as a delegate, and is held by the same uid.
That is unchanged from today.

## PID reuse

A `WorkloadPIDReference` is a number. If the process exits and the kernel
gives the number to another process before SPIRE reads `/proc/<pid>`, SPIRE
attests the other process. SPIRE has no pidfd-based reference, so redoma
closes the race on its side, with the pidfd it holds:

- A live process keeps its PID. If the pidfd reports the process still
  alive (`poll(2)` gives no `POLLIN`) after SPIRE has answered, the process
  was alive during the whole attestation, and the PID SPIRE read was its.
  A zombie counts as exited, which only errs on the safe side.
- So: on every `SubscribeToX509SVID` stream, once its first response
  arrives (SPIRE attests before it sends), and after every `FetchJWTSVID`,
  `redoma xds` checks the pidfd. If the process has exited, the response is
  discarded, the stream is closed and not resubscribed: the agent is
  stopping, and poststop unregisters it.
- A stream's later responses need no check: SPIRE attested once, at the
  start, and the selectors it serves do not change with the process.
- Independently, a response is used only for the SVID whose ID is the
  agent's own. A reused PID in another agent's container would yield that
  agent's selectors and SVID, which is refused even if the pidfd check were
  wrong.

Binding the pidfd to the right process is the remaining step, done once at
registration and again after a restart of `redoma xds`: open the pidfd,
read the start time from `/proc/<pid>/stat`, compare it with the one the
hook recorded, then check the pidfd is alive. If the process was replaced
before `pidfd_open`, the start times differ; if it is replaced after, the
pidfd reports it exited. A registration that fails the check is refused (the
hook then fails the container's start), and one read back from the
registration file that fails it is dropped as stale.

Reading another user's `/proc/<pid>/stat` requires `/proc` to show other
users' processes. The unit runs with `ProtectProc=invisible`, which hides
them; Broker mode needs `ProtectProc=default` (a drop-in, until Broker
becomes the default and the unit itself changes). `redoma xds` in Broker
mode refuses to start, naming the setting, when it cannot read
`/proc/1/stat`. `pidfd_open` needs no permission over the target.

### Podman's container lock

The hook runs while Podman starts the container, and Podman holds the
container's lock for the whole of it. SPIRE's docker attestor inspects the
container through the Podman API, which takes the same lock, so an
attestation started while the hook waits would wait for the hook in turn.
The hook waits for Envoy to bind the agent's listeners, which never depends
on the SVID, so there is no deadlock; but to keep SPIRE's attestor from
being blocked at all, `redoma xds` opens an agent's SVID stream only after
the agent's listeners are bound and the hook has been answered.

## SPIRE agent configuration

redoma does not install or configure SPIRE. A host running Broker mode
needs, in its SPIRE agent:

```hcl
agent {
    trust_domain = "redoma.internal"
    socket_path  = "/run/redoma-spire/workload/agent.sock"
    admin_socket_path = "/run/redoma-spire/admin/admin.sock"   # until the cutover is done

    experimental {
        broker {
            # Not in the Workload API's directory: SPIRE refuses that.
            socket_path = "/run/redoma-spire/broker/broker.sock"
            brokers = [
                {
                    id = "spiffe://redoma.internal/host/sds"
                    allowed_reference_types = [
                        { type_url = "type.googleapis.com/spiffe.broker.WorkloadPIDReference" },
                    ]
                },
            ]
        }
    }
}

plugins {
    WorkloadAttestor "unix" {
        plugin_data {}
    }
    WorkloadAttestor "docker" {
        plugin_data {
            podman_socket_path  = "unix:///run/podman/podman.sock"
            use_rootless_podman = false
        }
    }
}
```

- The SPIRE agent runs in the host's PID namespace and reads every
  process's `/proc/<pid>/cgroup`; it talks to rootful Podman's API socket,
  which is root-equivalent, so it must be trusted as root is. The Podman API
  service must be running (`podman.socket`).
- The docker attestor's default container locator (cgroup v2) finds the
  container ID in `libpod-<id>.scope`.
- Directories: `/run/redoma-spire/workload` `0750`, group admitting 961 and
  963 only; `/run/redoma-spire/broker` `0750`, group `redoma-sds`;
  `/run/redoma-spire/admin` as today.

Registration entries, with `<agent>` the host SPIRE agent's ID:

| SPIFFE ID | Parent | Selectors | Mode |
|---|---|---|---|
| `spiffe://<td>/host/sds` | `<agent>` | `unix:uid:961` | both (the broker's own SVID in Broker mode) |
| `spiffe://<td>/host/relay` | `<agent>` | `unix:uid:963` | both |
| `spiffe://<td>/agent/<name>` | `<agent>` | `redoma:agent:<name>` | Delegated |
| `spiffe://<td>/agent/<name>` | `<agent>` | `docker:label:redoma.agent:<name>` | Broker |

No agent entry may be `admin` or `downstream`.

## redoma's configuration

`/etc/redoma/redoma.conf` gains two keys, each with a flag of `redoma xds`:

```
# How redoma xds obtains the agents' SVIDs: delegated (the Delegated
# Identity API on spire_admin_socket) or broker (the Broker API on
# spire_broker_socket, authenticated with redoma xds's own SVID from
# spire_workload_api).
#identity_api = delegated
#spire_broker_socket = /run/redoma-spire/broker/broker.sock
```

Validation: `identity_api` is `delegated` or `broker`; any other value stops
every command from starting, like any bad value. `spire_broker_socket` is an
absolute path and is not in the directory of `spire_workload_api`'s socket
(SPIRE refuses the same). In Broker mode `spire_admin_socket` is not used.

Agents registered by a launcher (`/run/redoma/active`) carry no PID. In
Broker mode they get no SVID, and `redoma xds` logs why; a launcher that
needs Broker mode would write the PID and start time to its state file,
which is left for when one does.

## Versions

- **SPIRE.** Broker mode needs a SPIRE agent with spiffe/spire#7293
  (rootless Podman attestation opt-in, see the threat model). It should also
  have #7259 (the Broker API serves no JWT-SVID of an admin or downstream
  entry; redoma's entries are neither, so redoma does not depend on it) and
  #7288 (the container locator no longer falls back to the forgeable
  mountinfo; agents cannot mount, so this is defence in depth). None of the
  three is in a release as of v1.15.5. Until a release has them, a host runs
  a SPIRE agent built from a pinned commit of `main` that does, and moves to
  the first release that contains all three.
- **go-spiffe.** The Broker API's Go bindings are
  `github.com/spiffe/go-spiffe/v2/exp/proto/spiffe/broker`, already in
  redoma's module graph at v2.9.0, the version SPIRE's main builds with.
  `exp/` carries no compatibility promise, so go.mod keeps an exact version,
  a bump of go-spiffe is checked against the SPIRE agent the host runs, and
  a test pins the wire contract redoma relies on: the type URL
  `type.googleapis.com/spiffe.broker.WorkloadPIDReference`, the metadata
  header, and the fields redoma reads.

## Migration from the Delegated Identity API

A host moves one step at a time; until the last, setting
`identity_api = delegated` back and restarting `redoma xds` undoes it.

1. Run a SPIRE agent with the versions above, and add the broker block,
   the docker attestor and the directory permissions. The admin socket stays.
2. Add the Broker entries next to the Delegated ones. The two entries of an
   agent have the same SPIFFE ID and disjoint selectors: a Delegated
   subscription matches only the first, a Broker attestation only the
   second.
3. Upgrade redoma to a version with Broker mode. Its hook sends the PID; a
   registration made by an older hook has none, so restart the agents, now
   with `--label redoma.agent=<name>` next to the annotation.
4. Add the `ProtectProc=default` drop-in, set `identity_api = broker` and
   restart `redoma xds`. Envoy keeps its listeners and the SVIDs it holds
   across the restart; each agent's SVID is then served from a Broker
   stream. Check that every agent's relay stream reconnects.
5. Once the host has run on Broker mode long enough to trust it, remove the
   Delegated entries and the admin socket's `authorized_delegates`, which
   closes the Delegated Identity API on that host.

redoma's own path: this change adds Broker mode with Delegated the default.
A later release makes `broker` the default, with the unit's `ProtectProc`
changed to match, and a release after that removes Delegated mode.

## The work

- **Broker mode in `redoma xds`.** The setting and its validation; the
  hook's PID and start time in the registration and the registration file;
  the pidfd binding and the checks around every attestation; the Broker
  client (mTLS with the Workload API SVID, the metadata header, the
  subscription after the listeners are bound); the bundle from the Workload
  API; the startup checks (`/proc`, the Workload API directory). Tests cover
  both modes against fake SPIRE servers, a process that exits during an
  attestation, a start time that does not match, and the configuration's
  validation.
- **Nothing in the hook beyond the two fields.** The label is set on
  `podman run` by whoever starts the agent, documented in README.md once
  Broker mode is the default; the hook cannot set it or read it, and a
  mismatch with the annotation gives the agent no SVID at all.
