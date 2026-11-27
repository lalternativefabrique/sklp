# 0002 — A granted service drives its own node over a mounted socket

## Context

A dashboard that runs on the node it manages (the public playground, a
Coolify-like console) needs the node's control API. Every space is cut off
from its node by the host guard (`space/spacenet/hostguard.go`): a container
reaches neither the node's private nor its public address, apart from the
gateway's 80/443. That guard is what keeps a compromised app from driving the
node, and it must stay closed for every other space.

## Decision

The node operator grants one service of one space, on the node:
`sklet console allow <space>/<service> -scope read|operate|admin`. A manifest
cannot ask for it.

- The grant lives in `~/.config/sklp/control-grants.json` (0600), with the
  space's owner (its recorded stack) at the time it was given.
- The agent serves one Unix socket per grant, in a directory bind-mounted
  read-only at `/run/sklp` in that service only. No TLS and no token: holding
  the socket is the credential, and no TCP path opens in the host guard.
- Each request is checked against the grant: the space must still be owned by
  the owner the grant recorded (a space recreated under the same name by
  another repo inherits nothing), and the route must be in the scope.
  - read: spaces, state, manifests, datastores, logs, follows, watches, restores.
  - operate: read, plus follow, unfollow and restart.
  - admin: operate, plus apply, delete, exec, run.
  - never: token rotation, mesh peers, gateway control, public NIC, dial.
- Writes are logged by the agent. Revoking closes the socket within seconds.

## Consequences

A granted service with `admin` can do what the node's token can do for spaces,
so it is given only to a service the operator trusts as much as the node. The
mount appears on the next start of the service: granting is followed by a
restart.
