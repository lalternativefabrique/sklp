# 1. The previous generation is drained, not timed out

Date: 2026-11-04

## Status

Accepted. Ships with the next tagged release.

## Context

A roll on a node is blue/green: the new generation starts, passes its
readiness probe, takes the traffic at cutover, and the previous generation is
kept alive, de-routed, through a soak so a crash of the new one can be undone
by routing back. When the soak elapsed, the node stopped the previous
generation: SIGTERM, then SIGKILL 30 s later.

The soak is a rollback window. It was also, by accident, the only time a
request already running on the previous generation had to finish. A request
longer than the soak died with its process, and the node had no way to know.
On 2026-11-03 three generations in partage were lost that way, on a day with
some thirty rolls. The app could lengthen its soak, or move long work out of
the request, but both put the burden on the app for something the node
decides.

A connection established to the previous generation can only be an old one:
since cutover the proxy sends nothing new to it. The kernel keeps the table of
those connections, per network namespace, in `/proc/<pid>/net/tcp`, readable
by the node without asking anything of the process.

## Decision

After the soak, the node drains the previous generation instead of stopping it
on the clock.

- It counts the connections in ESTABLISHED state to the instance's own ip:port,
  from the instance's network namespace, every 2 s.
- While the count is above zero, the previous generation stays. At zero it is
  stopped as before.
- `ready.drain` caps the wait, 10 min by default, next to `ready.soak`. At the
  cap the generation is stopped anyway and the rollout record says how many
  connections were cut. That line is the signal that a request outgrows what
  a deployment can carry; only then is the app asked to change how it works.
- The rollout is `draining` for the control API, in flight like `soaking`. A
  spec arriving during the drain waits for it, and a crash of the new
  generation during the drain rolls back to the previous one, which is still
  there.
- An instance whose address is not known, or whose table cannot be read, is
  stopped at the end of the soak as before. The drain only waits on what it
  can observe.

The soak keeps its one meaning, the rollback window, and stays at 60 s.

## Consequences

- A request that starts before a roll finishes on the generation it started
  on. The app needs no signal handler, no job queue and no longer soak for
  that.
- A roll takes as long as the longest request still running on the previous
  generation, up to the cap. Two generations run side by side for that time,
  which the soak already allowed.
- An idle keep-alive connection from a proxy counts as established and holds
  the drain until the proxy closes it; traefik does so after 90 s of idleness.
  The cap bounds the rest.
- A crash of an instance still loses what it was running. Nothing on the node
  can carry a request across a dead process.
- Readers of the node's rollout state must know `draining` is not terminal;
  the skalpai core takes it with its next sklp pin.
