package sklet

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/lalternativefabrique/sklp/sklet/publicnic"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

const publicNICInterval = 30 * time.Second

// startPublicNIC keeps the public NIC's policy routing and SMTP egress in
// place; a nil config leaves the host untouched.
func startPublicNIC(ctx context.Context, cfg *publicnic.Config) {
	if cfg == nil {
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("component", "public-nic")
	host, err := publicnic.NewHost()
	if err != nil {
		log.Error("public nic reconciler disabled", "err", err)
		return
	}
	rec := publicnic.NewReconciler(*cfg, host, log)
	control.SetPublicNIC(func() (json.RawMessage, error) { return json.Marshal(rec.Status()) })
	events, err := publicnic.Events(ctx)
	if err != nil {
		log.Warn("public nic: netlink events unavailable, polling only", "err", err)
	}
	go rec.Run(ctx, publicNICInterval, events)
}
