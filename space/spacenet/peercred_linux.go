//go:build linux

package spacenet

import (
	"fmt"
	"net"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// peercred_linux.go authenticates the PEER of a space control socket by its uid.
//
// The attach and ingress sockets are unauthenticated command channels whose
// paths are derivable from the space name alone (sha256(name)[:12]). sockDir
// keeps them in a 0700 dir, which is the primary control; this is the second
// layer, so a mis-created or mis-inherited directory does not immediately hand a
// local user the ability to splice himself onto any service inside the space
// netns. SO_PEERCRED is set by the kernel at connect() time and cannot be forged
// by the client.

// checkPeerUID reports whether conn's peer runs as the same uid as this process.
// A peer whose credentials cannot be read is REJECTED: on these sockets an
// unidentifiable caller is exactly the case worth refusing.
func checkPeerUID(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("peer credentials: raw conn: %w", err)
	}
	var ucred *unix.Ucred
	var credErr error
	ctrlErr := raw.Control(func(fd uintptr) {
		ucred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if ctrlErr != nil {
		return fmt.Errorf("peer credentials: control: %w", ctrlErr)
	}
	if credErr != nil {
		return fmt.Errorf("peer credentials: getsockopt: %w", credErr)
	}
	if self := os.Getuid(); int(ucred.Uid) != self && !isChildRootUID(int(ucred.Uid)) {
		return fmt.Errorf("peer uid %d is not %d nor a child root of this space", ucred.Uid, self)
	}
	return nil
}

// childRootUIDs are the roots of the child user namespaces this worker has
// spawned, as this worker's own ids (what SO_PEERCRED reports here). The
// createRuntime hook of a child-userns service connects as that root.
var (
	childRootMu   sync.Mutex
	childRootUIDs = map[int]bool{}
)

func addChildRootUID(uid int) {
	childRootMu.Lock()
	defer childRootMu.Unlock()
	childRootUIDs[uid] = true
}

func isChildRootUID(uid int) bool {
	childRootMu.Lock()
	defer childRootMu.Unlock()
	return childRootUIDs[uid]
}
