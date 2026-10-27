package control

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/spacemanager"
)

// handleImage rolls one service of the live space onto a new image. It reads the
// manifest the node already persisted, swaps that one service's Image, and hands
// the result to the SAME reconcile path a full apply takes — so an image roll is
// an ordinary apply that happens to differ by one field, not a second mutation
// route with its own semantics.
//
// This is what lets a registry watcher deploy without the deploy.yaml: the node
// owns the manifest, the caller only names a service and a tag. Caller holds the
// server's apply mutex, as for handleApply.
func handleImage(space string, sup Supervisor, body io.Reader) (ApplyResult, error) {
	var req ImageRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		return ApplyResult{}, fmt.Errorf("decode request: %w", err)
	}
	if err := validServiceName(req.Service); err != nil {
		return ApplyResult{}, err
	}
	if err := validImageRef(req.Image); err != nil {
		return ApplyResult{}, err
	}

	services, meta, err := LoadManifest(space)
	if err != nil {
		return ApplyResult{}, err
	}

	patched := false
	for i := range services {
		if services[i].Name != req.Service {
			continue
		}
		// Compared against what the supervisor runs, not the manifest on disk:
		// after a rollback the manifest can name an image nobody runs, and a
		// watcher re-posting that tag must get it applied, not "unchanged".
		running := services[i].Image
		if cur, ok := sup.DesiredService(req.Service); ok {
			running = cur.Image
		}
		if running == req.Image {
			return ApplyResult{Result: ResultUnchanged}, nil
		}
		services[i].Image = req.Image
		services[i].RolledImages = spacemanager.JournalRolledImage(services[i].RolledImages, req.Image)
		patched = true
		break
	}
	if !patched {
		return ApplyResult{}, fmt.Errorf("unknown service %q in space %q", req.Service, space)
	}

	res, err := applyDesired(sup, services, meta)
	if err == nil {
		collectImagesAfterRoll()
	}
	return res, err
}

// collectImagesAfterRoll reclaims generations retired by earlier rolls.
//
// Every roll extracts a fresh rootfs and leaves the previous one behind; with a
// watcher rolling on each published tag, that is the accumulation that filled a
// production disk. Collecting here ties the cleanup to the event that creates
// the garbage.
//
// The generation THIS roll just retired is deliberately not among them: it is
// inside the grace window, still restartable without a re-pull if the new tag
// turns out to be broken at boot. A later roll collects it — at the latest the
// third one after it, since the collector keeps only the newest few
// generations per service whatever their age.
//
// Detached so a slow filesystem walk never delays the watcher's response. The
// retiring container may still be shutting down as this starts — its overlay
// keeps its image out of reach until it is really gone.
func collectImagesAfterRoll() {
	go func() {
		defer func() { _ = recover() }()
		CollectImages()
	}()
}

// CollectImages runs one image GC pass and journals what it freed or why it
// failed; a silent failure is how a node filled its disk unnoticed.
func CollectImages() {
	res, err := space.ImageGC(false)
	if err != nil {
		log.Printf("image gc: %v", err)
		return
	}
	if len(res.Removed) > 0 {
		log.Printf("image gc: removed %d image(s), freed %d MiB", len(res.Removed), res.FreedBytes>>20)
	}
}

// validImageRef bounds the image reference before it reaches the resolver. It is
// deliberately shape-only (no registry lookup): a reference carrying whitespace
// or a shell metacharacter has no legitimate form, and rejecting it here keeps
// an untrusted watcher from steering the pull anywhere unexpected.
func validImageRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("image is required")
	}
	if strings.ContainsAny(ref, " \t\n\r\"'`$\\;|&<>") {
		return fmt.Errorf("invalid image reference %q", ref)
	}
	if !strings.Contains(ref, ":") {
		return fmt.Errorf("image reference %q must be tagged (no floating tag)", ref)
	}
	return nil
}
