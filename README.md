# redoma

Isolated coding-agent containers behind one host Envoy.

redoma runs each coding agent (Claude Code, headless) in its own Podman
container and its own network namespace, with no network at all but the few
listeners one host Envoy binds inside that namespace. The listener a
connection arrives on is the agent's identity: every agent gets a SPIFFE ID,
`spiffe://<trust domain>/agent/<name>`, issued by SPIRE and held by Envoy,
never by the container. A lead (a person, a script, or a Claude Code session
of its own) tasks the agents through a relay and reads what they do,
ask and say.

```
agent container (own netns, --network none)        host
  redoma-shim  -> 127.0.0.1:7070 relay  --+
  claude       -> 127.0.0.1:8080 model  --+-- host Envoy --+-- mTLS (agent SVID) --> redoma relay <-- lead.sock <-- redoma msg / mcp
  git, tools   -> 127.0.0.1:3128 egress --+                +-- API key ------------> api.anthropic.com
                                                           +-- CONNECT ------------> allowed hosts
host Envoy <-- ADS (listeners, clusters, SVIDs) -- redoma xds <-- Delegated Identity API -- SPIRE agent
```

- **`redoma xds`** serves the host Envoy over ADS: each agent's three
  listeners, bound in the agent's namespace; its cluster to the relay; and
  its X.509 SVID, which it fetches from SPIRE's Delegated Identity API as the
  one authorized delegate.
- **`redoma relay`** carries the lead's tasks to the agents and their turns'
  output, questions and messages back, over one mutually authenticated
  stream per agent: Envoy presents the agent's SVID, so the relay knows the
  agent from the connection alone. Delivery is at least once, in order, and
  survives restarts on both ends.
- **`redoma msg`** is the lead's command line (`send`, `reply`, `cancel`,
  `watch`), and **`redoma mcp`** the same as MCP tools for a lead that is a
  Claude Code session.
- **`redoma oci-hook`** makes a rootful Podman container started with
  `--annotation redoma/agent=<name>` an agent: it mounts the container's
  network namespace where Envoy can bind in it, registers it, and refuses a
  container that could reach a host service (root, a service's uid or gid, a
  capability, a device, a forbidden mount, a shared namespace).
- **`redoma-shim`** is the container's entrypoint: it holds the agent's
  stream, runs each task as one `claude -p` turn, resuming one session, and
  gives claude two tools, `ask_lead` and `send_message`.

The model listener forwards only Claude Code's inference paths and injects
the credential on the way, so the container never holds one. The egress
listener is a CONNECT proxy to the hosts the agent's allowlist names, and
nothing else.

## Requirements

- Linux, amd64 or arm64, with systemd and rootful Podman (4.9 or later).
- SPIRE (server and an agent on the host), for the agents' identities and
  the relay's.
- Envoy on the host, running as root (it binds inside the agents'
  namespaces), with a bootstrap like `examples/envoy.yaml`.
- An Anthropic API key for the agents' model calls.

## Building

Bazel is the only build (see AGENTS.md):

```sh
bazel build //deb      # bazel-bin/deb/redoma_amd64.deb and redoma_arm64.deb
bazel test //...
```

Every binary is pure Go and cross-builds: both packages build on either
architecture.

## Installing

Each release on GitHub has the packages, `redoma_<tag>_amd64.deb` and
`redoma_<tag>_arm64.deb`, and their `SHA256SUMS`, where `<tag>` is the
release's tag without its `v`. A release candidate, tagged
`v<VERSION>-rc.<n>`, is a prerelease: its files are named
`redoma_<VERSION>-rc.<n>_<arch>.deb`, while the packages inside are version
`<VERSION>~rc.<n>`, which sorts below the release. With a release's files
downloaded, check them against `SHA256SUMS` and each one's build
provenance (made by this repository's release workflow), then install:

```sh
sha256sum --check --ignore-missing SHA256SUMS
gh attestation verify redoma_<tag>_amd64.deb -R bpalermo/redoma
sudo apt install ./redoma_<tag>_amd64.deb   # or redoma_<tag>_arm64.deb
```

For example, for `v0.1.0-rc.1`:

```sh
sha256sum --check --ignore-missing SHA256SUMS
gh attestation verify redoma_0.1.0-rc.1_amd64.deb -R bpalermo/redoma
sudo apt install ./redoma_0.1.0-rc.1_amd64.deb
```

A package built as above has no version in its name:

```sh
sudo apt install ./bazel-bin/deb/redoma_amd64.deb   # or redoma_arm64.deb
```

A package's version is VERSION on a release and `<VERSION>~...` on any
other build, so a newer build always upgrades an older one. A host that
installed an early package whose version is a bare `0.1.0` from a snapshot
build removes it first (`sudo apt remove redoma`), or the snapshots sort
below it.

The package installs:

| Path | What |
|---|---|
| `/usr/bin/redoma` | the binary |
| `/usr/lib/systemd/system/redoma-xds.service`, `redoma-relay.service` | the units, not enabled |
| `/usr/share/containers/oci/hooks.d/redoma-*.json` | the OCI hook's definitions |
| `/etc/containers/containers.conf.d/50-redoma-hooks.conf` | keeps Podman reading that directory |
| `/usr/lib/sysusers.d/redoma.conf` | the services' users and groups |
| `/etc/redoma/redoma.conf` | the settings, a conffile |
| `/usr/share/doc/redoma/examples/` | an Envoy bootstrap and the model key's SDS file |

It holds no secret and installs neither SPIRE nor Envoy, nor the agents'
image (see "The runtime image").

The services run as system users with fixed ids, which the hook refuses to
any agent container and SPIRE's entries attest the services by:

| User / group | uid / gid | What |
|---|---|---|
| `redoma-sds` | 961 | `redoma xds`, SPIRE's authorized delegate |
| `redoma-relay` | 963 | `redoma relay`; its group reaches the agents' socket |
| `redoma-lead` | 964 (group) | the users who may task agents |

The package makes them, and its installation fails, naming each, when one
of those names already has another id. It adds no one to `redoma-lead`; a
lead's user joins it by hand, then logs in again:

```sh
sudo usermod -aG redoma-lead <user>
```

## Configuration

`/etc/redoma/redoma.conf` holds what every subcommand shares; each key has a
flag that overrides it:

```
trust_domain = redoma.internal
#agent_annotation = redoma/agent
#spire_admin_socket = /run/redoma-spire/admin/admin.sock
#spire_workload_api = unix:///run/redoma-spire/workload/agent.sock
#identity_api = delegated
#spire_broker_socket = /run/redoma-spire/broker/broker.sock
```

From the trust domain come the agents' IDs, `spiffe://<td>/agent/<name>`,
and the relay's, `spiffe://<td>/host/relay`. An unknown key or a bad value
stops every subcommand from starting.

The rest of `/etc/redoma`, written by the administrator and only read by
redoma:

- `agents.txt`: the agent names that may run, one per line (lower-case
  letters, digits and `-`, at most 41). It and every directory above it
  must be owned by root and writable by no one else; otherwise no name is
  allowed.
- `egress/_default.txt` and `egress/<name>.txt`: the hosts an agent's
  egress proxy opens tunnels to (port 443), one host name per line, `#`
  comments; every agent gets the defaults, then its own.
- `secrets/anthropic-key.yaml`: the model API key, an SDS file Envoy reads
  (`examples/anthropic-key.yaml`); readable by Envoy's user alone.

### SPIRE

The host's SPIRE agent must expose its admin API (Delegated Identity) with
uid 961 as the one authorized delegate, and its Workload API to uid 963.
Point `spire_admin_socket` and `spire_workload_api` at its sockets. The
entries, with `<agent>` the host's SPIRE agent ID:

| SPIFFE ID | Parent | Selectors |
|---|---|---|
| `spiffe://<td>/host/sds` | `<agent>` | `unix:uid:961` |
| `spiffe://<td>/host/relay` | `<agent>` | `unix:uid:963` |
| `spiffe://<td>/agent/<name>` | `<agent>` | `redoma:agent:<name>` |

The agent entries' selector type, `redoma`, is one no attestor produces:
those identities exist only through the delegate, for the containers
`redoma xds` names.

`identity_api = broker` has `redoma xds` use the SPIFFE Broker API instead,
naming each agent's container process to SPIRE, whose attestors decide its
selectors. It needs its own SPIRE configuration, entries and host checks:
see `docs/broker-identity.md`. The Delegated Identity API stays the default.

### Envoy

`examples/envoy.yaml` is a bootstrap: ADS from `/run/redoma/xds/xds.sock`,
the `anthropic` cluster for the model listeners and the `egress_dfp`
cluster for the egress proxies. Envoy runs as root with the supplementary
groups `redoma-sds` and `redoma-relay`, sees `/run/netns`, and reads
`/etc/redoma/secrets/anthropic-key.yaml`. When an agent has AWS settings, Envoy
also serves the tunnels to Roles Anywhere (see "AWS credentials"). Envoy keeps its listeners while
`redoma xds` restarts, and redoma knows to wait for a replaced namespace to
be released before binding it again.

### Runtime directories

Everything redoma keeps at run time is under `/run/redoma` (root, 0755),
one directory per user of it, each with its own owner and mode:

| Directory | Owner, mode | Holds |
|---|---|---|
| `/run/redoma/xds` | `redoma-sds`, 0750 (`redoma-xds`'s `RuntimeDirectory`) | `xds.sock` (Envoy's ADS), `register.sock` (the OCI hook's), `state.json`, `registrations.json` |
| `/run/redoma/xds/aws` | `redoma-sds`, 0750 | one socket per agent with AWS settings, which Envoy dials (see "AWS credentials") |
| `/run/redoma/xds/tunnel` | `redoma-sds`, 0770 | the sockets Envoy serves for the tunnels to Roles Anywhere |
| `/run/redoma/relay` | `redoma-relay`, group `redoma-relay`, 0750 (`redoma-relay`'s) | `relay.sock`, the agents' socket, which Envoy dials |
| `/run/redoma/lead` | `redoma-relay`, group `redoma-lead`, 0750 (`redoma-relay`'s) | `lead.sock`, the lead's socket (`redoma msg`, `redoma mcp`) |
| `/run/redoma/envoy` | the host's Envoy unit, root 0700 | Envoy's admin socket, if the host gives it one |
| `/run/redoma/names` | a launcher, root 0755 | its per-agent name files |
| `/run/redoma/active` | a launcher, root | one file per agent it runs (see AGENTS.md, "Launchers") |

An Envoy in a container bind-mounts `/run/redoma/xds` and
`/run/redoma/relay`, never `/run/redoma/lead`. No agent container may have
any of `/run/redoma` mounted: the OCI hook refuses it.

### Model API credential

The model listeners inject the key of `secrets/anthropic-key.yaml` as
`x-api-key` on `/v1/messages` and `/v1/messages/count_tokens` only; any
other path is refused by Envoy before it leaves the host. In the container,
point Claude Code at the model listener with any placeholder key:
`ANTHROPIC_BASE_URL=http://127.0.0.1:8080` and
`ANTHROPIC_API_KEY=placeholder`.

Workload identity federation (an agent's own JWT-SVID exchanged for a
short-lived API token, so no static key is held on the host) is the next
credential mode; it is not implemented yet.

### AWS credentials

An agent can get AWS credentials of its own, for any AWS service, through
the standard container credentials provider of the AWS SDKs and the AWS
CLI, with nothing else to set up in the container. They are session
credentials of an IAM role, issued by IAM Roles Anywhere for the agent's
X.509 SVID. The SVID and its key never enter the container: `redoma xds`
holds them, asks Roles Anywhere for the session, and hands the container
only the session's access key, secret and token.

An agent opts in with a file, `/etc/redoma/aws/<name>.conf`, in the format
of `redoma.conf`:

```ini
trust_anchor_arn = arn:<partition>:rolesanywhere:<region>:<account>:trust-anchor/<id>
profile_arn = arn:<partition>:rolesanywhere:<region>:<account>:profile/<id>
role_arn = arn:<partition>:iam::<account>:role/<name>
# optional, 15m to 12h; 1h by default
#session_duration = 1h
```

The three must be in one partition and account, and the trust anchor and
the profile in one region, which is where the sessions are asked for. The
trust anchor holds the CA that issues the agents' SVIDs (SPIRE's). The
role's trust policy should require the agent's SPIFFE ID, for example a
condition that `aws:PrincipalTag/x509SAN/URI` equals
`spiffe://<trust domain>/agent/<name>`, so that another agent's SVID is
refused. The file is read every second. An unknown key, a missing ARN or a
malformed one makes every credential request of the agent a 503 until it
is fixed. An agent without the file gets nothing new.

Run the agent's container with:

```sh
-e AWS_CONTAINER_CREDENTIALS_FULL_URI=http://127.0.0.1:9911/v1/credentials
```

The port is `redoma xds --aws-credentials-port` (9911 by default). Only
`GET /v1/credentials` is served:

- 200 with `AccessKeyId`, `SecretAccessKey`, `Token` and `Expiration`
  (RFC 3339, UTC);
- 404 for any other path or method;
- 429 past 8 requests in flight for one agent;
- 503 when there is no session, its `Redoma-Reason` header saying why:
  `config_invalid`, `no_svid` (SPIRE has not issued the agent's SVID yet),
  `session_pending` (being asked for: retry), `session_refused` (Roles
  Anywhere refused the SVID for that anchor, profile and role) or
  `rolesanywhere_unavailable`.

The listener is the agent's identity: the request carries none.

The SDKs allow about a second per attempt, so `redoma xds` asks for a
session as soon as the agent's SVID arrives, keeps it in memory, and asks
for the next one 10 minutes before it ends. Requests are answered from
memory.

`redoma xds` has no network of its own. It reaches Roles Anywhere through
the host Envoy:

- `redoma xds` serves Envoy a plain TCP tunnel to
  `rolesanywhere.<region>.amazonaws.com:443` for each region the agents'
  files name, and no tunnel while no file names one. The tunnel's socket
  is in `/run/redoma/xds/tunnel`.
- The TLS is `redoma xds`'s own, end to end, verified against the system's
  CA certificates, so Envoy carries only ciphertext.
- The host's Envoy must be able to resolve and reach
  `rolesanywhere.<region>.amazonaws.com` on port 443, and to create sockets
  in `/run/redoma/xds/tunnel` (an Envoy in a container mounts
  `/run/redoma/xds` writable).
- While Envoy is down no new session can be asked for; the sessions in
  memory are served until they expire.

## The runtime image

The agents run an image with `redoma-shim` as its entrypoint and Claude
Code's native binary in it. It comes in two parts:

- **`redoma-base`**, published, public, with every release from
  v0.1.0-rc.2 on: `quay.io/redoma/redoma-base:<version>`, for amd64 and
  arm64. Debian bookworm with git, jq, python3, ripgrep, a shell and the
  usual tools, the CA bundle, and `redoma-shim` as the entrypoint. No
  Claude Code. Its `org.opencontainers.image.version` label is the version
  of the redoma package of the same release (`0.1.0~rc.2` for the image
  tagged `0.1.0-rc.2`). Each release's notes on GitHub give the image's
  digest. The digest is signed with cosign, keyless, by this repository's
  release workflow, and has a build provenance attestation.
- **Claude Code**, which you download yourself under Anthropic's terms, and
  you are responsible for complying with them. That is why redoma never
  publishes an image containing it. You add it in an image of your own
  built FROM the base, and keep that image local or in a private registry
  of your own.

An image of your own, pinned to the base's digest from its release's notes,
with Claude Code's native binary for the image's architecture (`package/claude`
of the npm package `@anthropic-ai/claude-code-linux-x64` or `-arm64`,
unmodified) next to the Containerfile. An image inherits its base's
labels, so it sets its own: otherwise it would claim the base's Apache-2.0
license, name and description, and its source label would link it to this
repository.

```dockerfile
FROM quay.io/redoma/redoma-base:<version>@sha256:<digest>
COPY --chmod=0755 claude /usr/local/bin/claude
LABEL org.opencontainers.image.title="<your image>" \
      org.opencontainers.image.description="<your description>" \
      org.opencontainers.image.licenses="<your license expression>" \
      org.opencontainers.image.source=""
```

```sh
sudo podman build -t localhost/redoma-runtime:dev .
```

To check the base's signature and provenance by the digest you pinned:
it was signed by this repository's release workflow, at the tag of the
release that names the digest, and nothing else. The identity below is
v0.1.0's; for another release, put its tag in place of `v0.1.0` (its
notes print the command with both filled in). The `cosign verify` needs
cosign v3 or later: the release signs with cosign v3, whose signature is a
Sigstore bundle that an older cosign does not look for by default.

```sh
cosign verify quay.io/redoma/redoma-base@sha256:<digest> \
  --certificate-identity https://github.com/bpalermo/redoma/.github/workflows/release.yml@refs/tags/v0.1.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://quay.io/redoma/redoma-base@sha256:<digest> -R bpalermo/redoma
```

To check the base against the package installed on the host:

```sh
sudo podman image inspect localhost/redoma-runtime:dev \
  --format '{{ index .Labels "org.opencontainers.image.version" }}'
dpkg-query -W -f '${Version}\n' redoma      # the same version
```

To give the agents Claude Code's managed settings, add
`/etc/claude-code/managed-settings.json` in the same image.

This repository also builds the whole image from source with Bazel,
`//runtime:image_index`, the base and Claude Code at the version
MODULE.bazel pins (amd64 and arm64), with its own title, description and
`NOASSERTION` license label, and no source label, for the same reason.
Build it for your host's architecture and load it into the rootful Podman
that runs the agents:

```sh
bazel run --@rules_img//img/settings:load_daemon=tar //runtime:image_load -- \
  --platform linux/amd64 | sudo podman load     # localhost/redoma-runtime:dev
```

### Third-party software in the base

The base redistributes Debian bookworm's binary packages, unmodified, from
snapshot.debian.org at the instant MODULE.bazel pins (`apt.sources_list`):
among them the GNU C Library (LGPL-2.1+), bash, coreutils and git (GPL),
and python3 (PSF). Each package's license is in the image at
`/usr/share/doc/<package>/copyright`, and the GPL and LGPL texts in
`/usr/share/common-licenses`. The exact version of every package, its
dependencies included, and its source package are in the image's
`/var/lib/dpkg/status` (what image scanners read), and in
`MODULE.bazel.lock` at the image's revision (the
`org.opencontainers.image.revision` label). The source of each is Debian's source
package of that version, kept on snapshot.debian.org
(`https://snapshot.debian.org/package/<source package>/<version>/`).

redoma-shim itself is Apache-2.0, built from this repository at the
revision in the image's `org.opencontainers.image.revision` label.

## Running

Start the services, then an agent:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now redoma-xds redoma-relay
echo example | sudo tee -a /etc/redoma/agents.txt

sudo podman run -d --name example \
  --annotation redoma/agent=example \
  --network none --user 1000:1000 --cap-drop all \
  --security-opt no-new-privileges \
  -v /srv/agents/example/work:/work \
  -e ANTHROPIC_BASE_URL=http://127.0.0.1:8080 -e ANTHROPIC_API_KEY=placeholder \
  -e HTTPS_PROXY=http://127.0.0.1:3128 -e NO_PROXY=127.0.0.1 \
  localhost/redoma-runtime:dev
```

`journalctl -u redoma-xds` shows the hook registration and the agent's
listeners; the container's start waits until Envoy has bound them.

Then, as a member of `redoma-lead`:

```sh
redoma msg send example "summarize the README in /work"   # prints example/1
redoma msg watch example                                  # the turn's output
```

For a lead that is a Claude Code session, register the MCP server once:

```sh
claude mcp add --scope user redoma -- /usr/bin/redoma mcp
```

## License

Apache License 2.0; see LICENSE and NOTICE.
