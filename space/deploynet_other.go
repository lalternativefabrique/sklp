//go:build !linux

package space

import (
	"context"
	"fmt"
	"net"

	"github.com/lalternativefabrique/sklp/space/ociproc"
)

// spaceNet is unavailable off Linux: the spacenet rootless bridge relies on
// Linux user+net namespaces and netlink. The launcher degrades to standalone
// containers (its own namespaces, no space bridge) when newSpaceNet errs.
type spaceNet struct{}

// attachPlan is the off-linux placeholder for the createRuntime hook attach
// plan. The launcher never builds a space off Linux (newSpaceNet errs), so
// no plan is ever produced; the type exists only so the shared launcher code
// compiles.
type attachPlan struct{}

func (p *attachPlan) ip() string                 { return "" }
func (p *attachPlan) sockPath() string           { return "" }
func (p *attachPlan) moveHookEnv() []string      { return nil }
func (p *attachPlan) configureHookEnv() []string { return nil }

func newSpaceNet(_ context.Context, _, _ string, _ bool, _ []string, _ string) (*spaceNet, error) {
	return nil, fmt.Errorf("space network requires linux")
}

func (c *spaceNet) join() *joinNamespaces      { return nil }
func (c *spaceNet) identity() ociproc.Identity { return ociproc.Identity{} }
func (c *spaceNet) spawnOverlayChild(_, _, _, _ string, _ int) (int, ociproc.Identity, error) {
	return 0, ociproc.Identity{}, fmt.Errorf("child userns requires linux")
}
func (c *spaceNet) enterAsSpaceRoot() []string                  { return nil }
func (c *spaceNet) handToChild(int, ...string) error            { return nil }
func (c *spaceNet) killAsSpaceRoot(int) error                   { return nil }
func serviceIdentity(*spaceNet) ociproc.Identity                { return ociproc.Identity{} }
func (c *spaceNet) spawnOverlay(_, _, _, _ string) (int, error) { return 0, nil }
func (c *spaceNet) resolvConf() string                          { return "" }
func (c *spaceNet) hostsConf() string                           { return "" }
func (c *spaceNet) attach(string, int) (string, error)          { return "", nil }
func (c *spaceNet) hookAttachReady() bool                       { return false }
func (c *spaceNet) setL7Routes(string) error                    { return nil }
func (c *spaceNet) setACLEdges([]ACLEdge) error                 { return nil }
func (c *spaceNet) planAttach(string) (*attachPlan, error) {
	return nil, fmt.Errorf("space network requires linux")
}
func (c *spaceNet) bindAttach(*attachPlan, int) {}
func (c *spaceNet) releaseAttach(*attachPlan)   {}
func (c *spaceNet) cancelAttach(*attachPlan)    {}
func (c *spaceNet) detach(int)                  {}
func (c *spaceNet) sendPacketConn(string, int, *net.UDPConn) error {
	return fmt.Errorf("udp ingress requires linux")
}

func (c *spaceNet) sendConn(string, int, bool, net.Conn) error {
	return fmt.Errorf("space ingress requires linux")
}
func (c *spaceNet) sendConnReplay(string, int, net.Conn, []byte) error {
	return fmt.Errorf("space ingress requires linux")
}
func (c *spaceNet) probeTCP(string) bool             { return false }
func (c *spaceNet) probeHTTP(string) bool            { return false }
func (c *spaceNet) close()                           {}
func (c *spaceNet) reclaimFromChild(...string) error { return nil }
