package main

import (
	"fmt"
	"io"

	"github.com/lalternativefabrique/sklp/space/embed"
)

func warnMissingHelpers(w io.Writer) {
	if !embed.HelpersEmbedded() {
		fmt.Fprintln(w, "sklp: warning: built without runc/slirp4netns, spaces cannot start; run `go generate ./space/embed/` and rebuild")
	}
}
