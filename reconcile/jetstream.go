package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// StateDir is where the node's reconcile state lives. Its existence is what
// tells an agent this node reconciles at all.
func StateDir(cacheRoot string) string {
	return filepath.Join(cacheRoot, "reconcile", "nats")
}

// Embedded is the in-process JetStream the reconcile stores live in.
type Embedded struct {
	JS   jetstream.JetStream
	nc   *nats.Conn
	srv  *natsserver.Server
	lock *os.File
}

// Close stops the server and waits for it before releasing the state, so the
// next opener never meets a store still being written.
func (e *Embedded) Close() {
	e.nc.Close()
	e.srv.Shutdown()
	e.srv.WaitForShutdown()
	e.lock.Close()
}

// OpenEmbeddedJetStream starts that JetStream on an ephemeral loopback port
// nothing dials from outside, refusing if another process holds the state.
func OpenEmbeddedJetStream(cacheRoot string) (*Embedded, error) {
	lock, err := lockState(cacheRoot)
	if err != nil {
		return nil, err
	}
	srv, err := natsserver.NewServer(&natsserver.Options{
		Port:      -1,
		JetStream: true,
		StoreDir:  StateDir(cacheRoot),
		NoSigs:    true,
	})
	if err != nil {
		lock.Close()
		return nil, fmt.Errorf("start jetstream: %w", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(15 * time.Second) {
		srv.Shutdown()
		lock.Close()
		return nil, fmt.Errorf("jetstream did not come up")
	}
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		srv.Shutdown()
		lock.Close()
		return nil, fmt.Errorf("connect jetstream: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		srv.Shutdown()
		lock.Close()
		return nil, fmt.Errorf("open jetstream: %w", err)
	}
	return &Embedded{JS: js, nc: nc, srv: srv, lock: lock}, nil
}
