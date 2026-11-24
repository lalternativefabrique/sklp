//go:build !linux

package devstack

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/lalternativefabrique/sklp/internal/limarelay"
)

func forwardToSpace(string, int, forwardTarget) int {
	fmt.Fprintln(os.Stderr, "sklp dev forward: needs the embedded runtime (Linux)")
	return 1
}

// relayForward runs the forward inside the VM on a host port chosen here, so the
// same port can be published on this machine.
func relayForward(repoAbs, stack string, t forwardTarget) (int, bool) {
	hostPort := t.HostPort
	if hostPort == 0 {
		p, err := freeLoopbackPort()
		if err != nil {
			fmt.Fprintf(os.Stderr, "sklp dev forward: %v\n", err)
			return 1, true
		}
		hostPort = p
	}
	release := limarelay.ForwardHostPorts([]int{hostPort})
	defer release()
	argv := []string{"sklp", "dev", "forward", stack,
		t.Service + ":" + strconv.Itoa(t.Port), strconv.Itoa(hostPort)}
	err := limarelay.RelayCommand(repoAbs, argv)
	var exit *limarelay.GuestExitError
	switch {
	case errors.As(err, &exit):
		return exit.Code, true
	case err != nil:
		fmt.Fprintf(os.Stderr, "sklp dev forward: %v\n", err)
		return 1, true
	}
	return 0, true
}

func freeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("pick a free port: %w", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
