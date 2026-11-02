# Changelog

Changes that a host running redoma must act on, newest first. Until 1.0, a
release may break what an earlier one set up; each such change is listed
here with what to do about it.

## Unreleased

### New: AWS credentials for the agents that opt in

`redoma xds` serves an agent with `/etc/redoma/aws/<name>.conf` AWS session
credentials from IAM Roles Anywhere, for its SVID, on
`http://127.0.0.1:9911/v1/credentials` in its namespace (README.md, "AWS
credentials"). Nothing changes for an agent without the file, nor for a
host where no agent has one.

To use it:

1. Create the trust anchor (SPIRE's CA), the profile and the role in AWS,
   and write the agent's file: `trust_anchor_arn =
   arn:<partition>:rolesanywhere:<region>:<account>:trust-anchor/<id>`,
   `profile_arn = arn:<partition>:rolesanywhere:<region>:<account>:profile/<id>`
   and `role_arn = arn:<partition>:iam::<account>:role/<name>`.
2. Let the host's Envoy reach `rolesanywhere.<region>.amazonaws.com:443`
   and create sockets in `/run/redoma/xds/tunnel`: an Envoy in a container
   mounts `/run/redoma/xds` writable, not read-only.
3. Run the agent's container with
   `AWS_CONTAINER_CREDENTIALS_FULL_URI=http://127.0.0.1:9911/v1/credentials`.

`redoma xds` makes two directories under its own: `/run/redoma/xds/aws`
(0750) and `/run/redoma/xds/tunnel` (0770). Its unit is unchanged: it still
has no network, and reaches Roles Anywhere through a tunnel the host Envoy
serves, with TLS of its own.

## 0.1.0

The first release. What a host acts on depends on the release candidate it
runs:

- v0.1.0-rc.1, rc.2 or rc.3: the move of the runtime directories below
  (it shipped in rc.4).
- v0.1.0-rc.4: nothing; it already has every change of this release.

### Breaking (rc.4): every runtime directory moves under /run/redoma

The runtime directories are now subdirectories of one `/run/redoma` (root,
0755). Each keeps its owner, group and mode; systemd makes the parent.

| Before | Now |
|---|---|
| `/run/redoma-xds/` | `/run/redoma/xds/` |
| `/run/redoma-xds/xds.sock` | `/run/redoma/xds/xds.sock` |
| `/run/redoma-xds/register.sock` | `/run/redoma/xds/register.sock` |
| `/run/redoma-xds/state.json` | `/run/redoma/xds/state.json` |
| `/run/redoma-xds/registrations.json` | `/run/redoma/xds/registrations.json` |
| `/run/redoma-relay/relay.sock` | `/run/redoma/relay/relay.sock` |
| `/run/redoma-relay-lead/lead.sock` | `/run/redoma/lead/lead.sock` |
| `/run/redoma/envoy/` | unchanged |
| `/run/redoma/active/` | unchanged |

What changes with it:

- The units: `redoma-xds` has `RuntimeDirectory=redoma/xds`, `redoma-relay`
  `RuntimeDirectory=redoma/relay redoma/lead`.
- The flags' defaults: `redoma xds` `--state-file`, `--register-socket`,
  `--registrations-file`, `--xds-socket` and `--relay-socket`; `redoma
  relay` `--socket` and `--lead-socket`; `redoma oci-hook` `--socket`;
  `redoma msg` and `redoma mcp` `--socket`.
- `examples/envoy.yaml`: the `redoma_xds` cluster's pipe is
  `/run/redoma/xds/xds.sock`. A bootstrap made from it follows.
- The OCI hook forbids `/run/redoma` as a whole in place of each service's
  directory: every directory under it, now and later, is out of an agent's
  reach. The old paths (`/run/redoma-xds`, `/run/redoma-relay`,
  `/run/redoma-relay-lead`) are no longer forbidden. A host that keeps a
  socket at one of them, through a drop-in or a flag, would leave it open
  to an agent's bind mount, `lead.sock` included: move to the new paths.
  The hook still forbids the directory of its own `--socket` wherever it
  is, so redoma xds's sockets stay covered; the relay's do not.

To upgrade:

1. Stop every agent container. A running agent registered by the OCI hook
   is recorded in `/run/redoma-xds/registrations.json`, which the new
   `redoma xds` does not read: it would start with no registrations and
   close their pidfds, and those agents would keep running with no
   listeners until restarted.
2. Stop Envoy, `redoma-xds` and `redoma-relay`, and install the package.
3. Change whatever names the old paths: the Envoy bootstrap, a
   containerized Envoy's bind mounts (`/run/redoma/xds` and
   `/run/redoma/relay`), unit drop-ins, launchers, and any client given an
   explicit `--socket`. A tool sandbox that allows the lead's socket by
   path allows `/run/redoma/lead/lead.sock` instead.
4. Start the services and Envoy, then the agents.
5. Remove the old directories, `/run/redoma-xds`, `/run/redoma-relay` and
   `/run/redoma-relay-lead`: the units keep them
   (`RuntimeDirectoryPreserve`) until the next reboot otherwise.

Agents can instead keep running through the upgrade if, between steps 2
and 4, `/run/redoma/xds/` is made (owner and group `redoma-sds`, 0750) and
`state.json` and `registrations.json` are moved into it from
`/run/redoma-xds/`, keeping their owner and mode, before `redoma-xds`
starts. In Broker mode this also needs systemd 254 or later: the agents'
pidfds live in the unit's file descriptor store, and only
`FileDescriptorStorePreserve=yes` keeps the store across the stop in step
2 and the start in step 4. An older systemd ignores the setting and drops
the store at the stop; each running agent then keeps its listeners but gets
no SVID until its container is restarted. Delegated mode does not use the
pidfds. Stopping the agents is simpler and is the recommended way.
