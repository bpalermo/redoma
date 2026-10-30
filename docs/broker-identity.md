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
   |   mounts the netns, opens a pidfd for the container's init process
   v
redoma xds  -- registration {container, agent, netns} + pidfd (SCM_RIGHTS) --
   |
   | pidfd kept in systemd's file descriptor store while the agent runs
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
2. **The hook** runs as today (createRuntime, root). After it bind-mounts
   the namespace from the container's init PID, it opens a pidfd for that
   PID (`pidfd_open(2)`) and binds it to the process exactly (see "Binding
   the pidfd"). It sends the pidfd with its registration request, as
   `SCM_RIGHTS` ancillary data on the registration socket.
3. **redoma xds** receives the pidfd with the request, from root only (the
   socket's peer check is unchanged), and accepts the registration if the
   descriptor is a pidfd of a live process. It keeps the pidfd for as long
   as the agent is registered, in its own table and in systemd's file
   descriptor store, so a restart of `redoma xds` gets it back (see "The
   file descriptor store"). The PID it names in a Broker request is the
   pidfd's, read from `/proc/self/fdinfo/<fd>` (the `Pid:` line): its own
   process's file, which `ProtectProc=invisible` leaves readable. It never
   reads another process's `/proc`.
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
   agent's listeners and the hook has been answered (see "Podman's
   container lock"), and resubscribed with backoff while the agent is
   registered.
6. **The bundle** comes from `redoma xds`'s own Workload API stream (the
   trust domain's X.509 bundle), not from the Broker API: Broker's bundle
   streams take a workload reference too, and the bundle must be served
   while no agent runs.
7. **JWT SVIDs** (not served to Envoy yet; the interface is for the coming
   workload identity federation): `FetchJWTSVID` with the agent's PID
   reference and its audience. SPIRE ignores the request's `spiffe_id`: it
   mints and returns a JWT-SVID for every entry the attested selectors
   match (for example an entry selecting `unix:uid:1000`, if the host has
   one). So `redoma xds` keeps only the token whose SPIFFE ID is the
   agent's, as the Delegated `FetchJWTSVID` does today, and filtering by
   `spiffe_id` in the request is never relied on. SPIRE attests the PID
   again on every call, so each fetch costs a Podman API inspect; the
   consumer caches a token until it nears expiry.

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
  attestor recognizes a rootless Podman container by a `/user-<uid>.slice/`
  in its cgroup path, and inspects it through that user's own Podman socket,
  which the user controls: such a container can present any label. Up to
  v1.15.5 SPIRE always does this; spiffe/spire#7293 makes it opt-in
  (`use_rootless_podman`, default false). On a SPIRE without #7293, setting
  `podman_socket_path_template` to a root-owned path that does not exist
  (it must keep its one `%d`) has the same effect: the lookup fails, the
  docker attestor errors, and SPIRE drops its selectors for that process.
- **A rootless container in a delegated cgroup.** Rootless Podman run by a
  system service with `Delegate=` gets a cgroup without a
  `/user-<uid>.slice/`. SPIRE then takes it for a rootful container and
  looks up, on the rootful socket, the container ID it reads from a cgroup
  name that user chose, so it can name a rootful agent's container and
  present that agent's labels. Neither #7293 nor the template covers this.
  Only keeping such users off the Workload API does: see the host
  requirements below.
- **An image**: image labels become container labels unless the run
  overrides them. The runtime image sets no `redoma.agent` label, and
  Broker mode adds a test to `//runtime` that keeps it so. An agent run
  without `--label`, from an image that has one, gets that image's SVID
  offered by SPIRE, and `redoma xds` refuses it because its SPIFFE ID is
  not the agent's (step 5).

Rootless attestation, of either kind, only matters to a process that can
reach the Workload API. The Broker API is not a way in: it admits only the
brokers its configuration lists, by SPIFFE ID, at the TLS layer, and the
one broker names only PIDs the hook registered.

### The mitigation: agents never reach a SPIRE socket

The Workload API is the door through which a labelled process other than
`redoma xds` could ask for an agent's SVID. Here is what keeps the agents
from it, and how far each layer goes. The layers are not independent: all
of the ones that concern the agents are enforced by the OCI hook.

1. **The hook's mount check is the barrier.** The hook refuses to start an
   agent that has any of `hook.ForbiddenSources` mounted, anything in one of
   them, or any directory above one, under any name, and it checks the
   mounts the runtime actually made, not only the spec. All of SPIRE's
   sockets for redoma (Workload API, admin, broker) live under
   `/run/redoma-spire`, which is in the list. This is the only thing that
   stops a bind mount of the Workload API socket file itself: SPIRE makes
   the socket `0777` when it creates it, so once the file is mounted into a
   container no directory permission applies. Broker mode adds a mounts test
   for exactly that case, a bind of the socket file.
2. **The forbidden list follows the configuration.** Today
   `ForbiddenSources` names `/run/redoma-spire` as a constant, while
   `spire_workload_api`, `spire_admin_socket` and the new
   `spire_broker_socket` accept any path: a host that moved a socket would
   lose layer 1 without a word. So the hook adds the directory of each
   configured SPIRE socket to its forbidden list, both as written and as it
   resolves through symlinks, read from `/etc/redoma/redoma.conf` at every
   container start. And `redoma xds` in Broker mode refuses to start unless
   each socket, resolved through symlinks, lies under that same list (the
   same function computes it for both).
3. **Directory permissions, a second layer against exposure of the
   directory.** The Workload API socket's directory grants nothing to
   others, and its group has no members but the services that need it:
   `redoma-sds` (961) and `redoma-relay` (963). That stops an agent that
   has the whole directory, or a path through it, mounted, which layer 1
   already refuses; it does nothing against a mount of the socket file. And
   the condition it relies on, that an agent runs with no service uid or gid
   and no capability, is enforced by the same hook. In Broker mode
   `redoma xds` refuses to start when the directory grants any permission to
   others, is writable by its group, or has a group with any member other
   than 961 and 963 (members as `/etc/group` lists them, and users whose
   primary group it is in `/etc/passwd`).
4. **No TCP.** The SPIRE agent serves the Workload API on a unix socket
   only. The agents' own network namespaces rule out a TCP endpoint and an
   abstract socket, and nothing more: a pathname unix socket is reached
   through the file system, across network namespaces, which is why
   layers 1 to 3 are about files.

Outside the agents, the host must keep off the Workload API every user who
can run Podman rootless from a delegated cgroup (above): the directory's
group admits only 961 and 963, and neither of those users may run
containers (no subordinate ids, no `Delegate=` service). That is a host
requirement, not something redoma can check.

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

### Binding the pidfd

The hook binds the pidfd to the container's process, as root, while the
runtime holds that process (it cannot exit on its own before the container
starts):

1. bind-mount the namespace from `/proc/<pid>/ns/net`, as today;
2. `pidfd_open(pid)`;
3. check that `/proc/<pid>/ns/net` is the namespace it mounted (same device
   and inode), then that the pidfd still reports the process alive.

If the PID was reused before step 2, step 3's namespace differs; if after,
the pidfd reports the process exited. Either way the hook refuses the
container's start. A pidfd that passes refers to the process whose
namespace is the agent's, with no start-time comparison, and `redoma xds`
never needs to read another process's `/proc`: the unit keeps
`ProtectProc=invisible`.

### The file descriptor store

`redoma xds` puts each pidfd in systemd's file descriptor store
(`sd_notify` with `FDSTORE=1`), named `FDNAME=<container ID>`: the OCI
container ID that already names the registration in
`/run/redoma-xds/registrations.json`. The unit gains:

```
NotifyAccess=main
FileDescriptorStoreMax=1024
FileDescriptorStorePreserve=yes
```

- **After a restart**, systemd passes the stored descriptors back
  (`LISTEN_FDS`, `LISTEN_FDNAMES`). `redoma xds` reads the registration
  file, then matches each registration to the descriptor of the same name.
  A descriptor is used only if it is a pidfd (its fdinfo has a `Pid:`
  line) of a live process; one with no registration, or of a process that
  exited, is closed and removed from the store (`FDSTOREREMOVE=1`).
- **Unregistering** (poststop, or the rescan that finds a namespace gone)
  closes the pidfd and removes it from the store. systemd also drops a
  stored pidfd by itself once the process is reaped (it polls stored
  descriptors and drops one that reports `POLLHUP`).
- **When the store is lost**: a reboot loses it, but also every container;
  `FileDescriptorStorePreserve=yes` (systemd 254 and later) keeps it across
  `systemctl stop` and `restart`, and older systemd keeps it only across
  the unit's own automatic restarts. A store lost while agents run (an old
  systemd and a manual restart, `redoma xds` started outside systemd, the
  store full) leaves registrations without a pidfd. Such an agent keeps its
  listeners and, in Broker mode, gets no SVID: `redoma xds` logs the
  container to restart, and restarting it registers it again with a new
  pidfd. In Delegated mode a missing pidfd changes nothing.
- The hook sends a pidfd in both modes, and `redoma xds` stores it in both,
  so switching a host to Broker mode needs no restart of the agents once
  they were started by a hook that sends one.

### Podman's container lock

Podman holds a container's lock for the whole of its start: from the
runtime's create, through the hooks, until the container has started.
SPIRE's docker attestor inspects the container through the Podman API, and
an inspect takes the same lock. An attestation started while the hook waits
would wait for the hook in turn. The hook waits for Envoy to bind the
agent's listeners, which never depends on the SVID, so there is no deadlock.
To keep the wait short, `redoma xds` opens an agent's SVID stream only after
the agent's listeners are bound and the hook has been answered. The first
attestation can still wait briefly, while Podman finishes starting the
container after the hook.

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
            # Neither in the Workload API socket's directory nor below it:
            # SPIRE refuses both.
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
            podman_socket_path = "unix:///run/podman/podman.sock"
            # With spiffe/spire#7293:
            use_rootless_podman = false
            # Without it, instead: a root-owned path that does not exist.
            # podman_socket_path_template = "unix:///run/redoma-spire/no-rootless-podman/%d.sock"
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
- Directories, siblings under `/run/redoma-spire`: `workload` `0750`, its
  group with no members but 961 and 963; `broker` `0750`, group
  `redoma-sds`; `admin` as today.
- Host requirement (see the threat model): no user who can reach the
  Workload API may run containers, rootless or from a delegated cgroup.

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

Validation, for every command, like any bad value:

- `identity_api` is `delegated` or `broker`.
- `spire_broker_socket` is an absolute path, neither in the directory of
  `spire_workload_api`'s socket nor in any directory below it: SPIRE's own
  check, a prefix match on that directory and `/`, applied the same way.

At startup in Broker mode, `redoma xds` also refuses to run unless each
SPIRE socket resolves under the hook's forbidden list and the Workload API
directory passes the permission check (threat model, layers 2 and 3). In
Broker mode `spire_admin_socket` is not used for identities.

Agents registered by a launcher (`/run/redoma/active`) carry no pidfd. In
Broker mode they get no SVID, and `redoma xds` logs why; a launcher that
needs Broker mode would pass a pidfd as the hook does, which is left for
when one does.

## Versions

- **SPIRE.** Broker mode works with a released SPIRE agent, v1.15.5, given
  the `podman_socket_path_template` setting above. Three changes on SPIRE's
  `main`, in no release as of v1.15.5, harden it, and none is required:
  spiffe/spire#7293 (rootless Podman attestation opt-in: the cleaner form
  of that setting); #7259 (the Broker API serves no JWT-SVID of an admin or
  downstream entry; redoma's entries are neither); #7288 (the container
  locator no longer falls back to the forgeable mountinfo; agents cannot
  mount). A host moves to the first release that has them; running a
  pinned commit of `main` before then is its own choice.
- **go-spiffe.** The Broker API's Go bindings are
  `github.com/spiffe/go-spiffe/v2/exp/proto/spiffe/broker`, already in
  redoma's module graph at v2.9.0, the version SPIRE's main builds with.
  `exp/` carries no compatibility promise, so go.mod keeps an exact version,
  a bump of go-spiffe is checked against the SPIRE agent the host runs, and
  a test pins the wire contract redoma relies on: the type URL
  `type.googleapis.com/spiffe.broker.WorkloadPIDReference`, the metadata
  header, and the fields redoma reads.
- **Linux and systemd.** `pidfd_open` (Linux 5.3) and a pidfd's `Pid:`
  line in fdinfo: Linux 5.5 or later is assumed.
  `FileDescriptorStorePreserve=` needs systemd 254.

## Migration from the Delegated Identity API

A host moves one step at a time; until the last, setting
`identity_api = delegated` back and restarting `redoma xds` undoes it.

1. Configure the SPIRE agent as above: the broker block, the docker
   attestor with rootless attestation off, the directory permissions. The
   admin socket stays.
2. Add the Broker entries next to the Delegated ones. The two entries of an
   agent have the same SPIFFE ID and disjoint selectors: a Delegated
   subscription matches only the first, a Broker attestation only the
   second.
3. Upgrade redoma to a version with Broker mode. Its hook sends a pidfd; a
   registration made by an older hook has none, so restart the agents, now
   with `--label redoma.agent=<name>` next to the annotation.
4. Set `identity_api = broker` and restart `redoma xds`. Envoy keeps its
   listeners and the SVIDs it holds across the restart; each agent's SVID
   is then served from a Broker stream. Check that every agent's relay
   stream reconnects.
5. Once the host has run on Broker mode long enough to trust it, remove the
   Delegated entries and the admin socket's `authorized_delegates`, which
   closes the Delegated Identity API on that host.

redoma's own path: this change adds Broker mode with Delegated the default.
A later release makes `broker` the default, and a release after that
removes Delegated mode.

## The work

- **Broker mode in `redoma xds`.** The setting and its validation; the
  pidfd in the registration, its binding in the hook, and the file
  descriptor store; the checks around every attestation; the Broker client
  (mTLS with the Workload API SVID, the metadata header, the subscription
  after the listeners are bound, the SPIFFE ID filter on X.509 and JWT
  responses); the bundle from the Workload API; the forbidden list derived
  from the configured sockets, and the startup checks. Tests cover both
  modes against fake SPIRE servers, a process that exits during an
  attestation, a pidfd bound to the wrong namespace, descriptors matched
  after a restart and a store that lost one, a bind mount of the socket
  file, a socket outside the forbidden list, the directory checks, and the
  configuration's validation.
- **Nothing about the label in the hook.** The label is set on
  `podman run` by whoever starts the agent, documented in README.md once
  Broker mode is the default; the hook cannot set it or read it, and a
  mismatch with the annotation gives the agent no SVID at all.
