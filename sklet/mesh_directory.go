package sklet

import (
	"context"
	"errors"
	"strings"

	"github.com/lalternativefabrique/sklp/spacemanager/control"
	"github.com/lalternativefabrique/sklp/spacemanager/mesh"
)

type meshDirectory struct{ client *mesh.Client }

func (d meshDirectory) ListPeers(ctx context.Context) ([]control.MeshPeer, error) {
	peers, err := d.client.ListPeers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]control.MeshPeer, 0, len(peers))
	for _, p := range peers {
		out = append(out, control.MeshPeer{
			ID: p.ID, SignKey: p.SignKey, PrivateIP: p.PrivateIP, Endpoint: p.Endpoint,
			Spaces: p.Spaces, Services: len(p.Services), LastSeenAt: p.LastSeenAt,
		})
	}
	return out, nil
}

func (d meshDirectory) DeletePeer(ctx context.Context, id string) error {
	err := d.client.DeletePeer(ctx, id)
	if err != nil && strings.Contains(err.Error(), "status 404") {
		return errors.Join(control.ErrMeshPeerNotFound, err)
	}
	return err
}
