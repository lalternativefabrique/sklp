//go:build linux

// Package spacenet builds a rootless space network: a Linux bridge living in
// a detached network namespace, onto which space containers attach by veth
// and reach each other by IP. It is the all-Go equivalent of what netavark +
// a bridge would do for podman, but with zero real root and no external
// helper binaries — only github.com/vishvananda/netlink inside a user+net
// namespace, exactly the mechanism the netspike PoC validated.
//
// # Topology
//
//	       space netns (own userns)
//	┌───────────────────────────────────┐
//	│  cap-<name>-br  10.88.0.1/24 (gw)  │
//	│     │            │                 │
//	│   veth0        veth1               │
//	└────┼─────────────┼─────────────────┘
//	     │ (LinkSetNsPid)                │
//	┌────┴─────┐   ┌────┴─────┐
//	│ ctr A    │   │ ctr B    │   <- each its OWN netns,
//	│ eth0     │   │ eth0     │      SHARING the space userns
//	│ .0.2     │   │ .0.3     │
//	└──────────┘   └──────────┘
//
// # Rootless ownership rule (the hard constraint)
//
// The kernel only lets a process move an interface into a target netns if it
// holds CAP_NET_ADMIN in BOTH netns relative to its own user namespace. A
// process owning the space netns therefore CANNOT push a veth into a netns
// owned by a *sibling* user namespace — which is exactly what a default runc
// container (its own userns + netns) is. Empirically, LinkSetNsPid into such
// a netns returns EPERM, and even opening /proc/<pid>/ns/net is denied.
//
// The validated fix: a space container must JOIN the space's user
// namespace (share UsernsPath) while keeping a PRIVATE network namespace.
// Then the space netns and the container netns are owned by the same
// userns, the space worker has CAP_NET_ADMIN over both, and the veth push
// + addressing + ping all succeed rootless. Attach therefore expects the
// target container PID to live in SpaceNet.UsernsPath; see Attach.
//
// # Where the netlink runs
//
// A multithreaded Go host process cannot setns() into a child user namespace
// (the kernel rejects setns(CLONE_NEWUSER) on a threaded process), so all
// netlink work happens INSIDE the space by a re-exec'd worker that owns the
// namespaces and serves a tiny line-delimited JSON command protocol. The
// worker's init PID gives the kernel-stable /proc/<pid>/ns/{user,net} handles
// callers thread into a container spec — the same leader/joiner mechanism the
// CI multi-container path already uses.
package spacenet
