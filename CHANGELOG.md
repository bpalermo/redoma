# Changelog

Changes that a host running redoma must act on, newest first. Until 1.0, a
release may break what an earlier one set up; each such change is listed
here with what to do about it.

## Unreleased

### Breaking: every runtime directory moves under /run/redoma

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
  reach.

To upgrade: stop Envoy and both services, install the package, and change
whatever names the old paths: the Envoy bootstrap, a containerized Envoy's
bind mounts (`/run/redoma/xds` and `/run/redoma/relay`), unit drop-ins,
launchers, and any client given an explicit `--socket`. A tool sandbox that
allows the lead's socket by path allows `/run/redoma/lead/lead.sock`
instead. Then start them again. The old directories are left empty until
the next reboot, or remove them.
