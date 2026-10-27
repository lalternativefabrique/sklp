package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/remotecache"
)

// cacheCommand declares `sklp cache <subcommand>` — inspection of the repo-local
// pipeline cache and the budgeted collection of the CI tool cache. Wiping caches
// is `sklp clean`, which also reaps the runtime state a wipe would otherwise
// strand.
func cacheCommand() cli.Command {
	return cli.Command{
		Name:  "cache",
		Short: "Inspect the repo cache / collect the CI tool cache (status, gc)",
		Sub: []cli.Command{
			{
				Name:  "status",
				Short: "Show local + remote cache state",
				Flags: []cli.Flag{
					{Name: "repo", Type: cli.StringFlag, StringDefault: ".", Usage: "repository root whose local cache should be inspected"},
				},
				Run: func(ctx context.Context, args cli.Args) int {
					repoAbs, err := filepath.Abs(args.String("repo"))
					if err != nil {
						fmt.Fprintf(os.Stderr, "sklp cache status: %v\n", err)
						return 1
					}
					if err := writeCacheStatus(ctx, os.Stdout, repoAbs); err != nil {
						fmt.Fprintf(os.Stderr, "sklp cache status: %v\n", err)
						return 1
					}
					return 0
				},
			},
			cacheGCCommand(),
		},
	}
}

// parseCacheSize accepts a plain byte count or a K/M/G/T suffix. An empty value
// means "use the built-in budget", signalled by -1 so an explicit 0 (disable
// the LRU pass) stays distinguishable from "unset".
func parseCacheSize(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return -1, nil
	}
	mult := int64(1)
	switch unit := raw[len(raw)-1]; unit {
	case 'K', 'k':
		mult = 1 << 10
	case 'M', 'm':
		mult = 1 << 20
	case 'G', 'g':
		mult = 1 << 30
	case 'T', 't':
		mult = 1 << 40
	}
	if mult > 1 {
		raw = raw[:len(raw)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid --max-size %q: want a byte count, optionally suffixed K/M/G/T", raw)
	}
	return n * mult, nil
}

// writeCacheStatus renders the cache status report to w. Split out from
// the verb shell so tests can capture the output without spawning a
// subprocess.
func writeCacheStatus(ctx context.Context, w io.Writer, repoAbs string) error {
	root := filepath.Join(repoAbs, ".sklp", "cache")
	size, entries, err := dirSize(root)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "local cache:\n")
	fmt.Fprintf(w, "  root:    %s\n", root)
	fmt.Fprintf(w, "  size:    %s (%d entries)\n", humanBytes(size), entries)

	rc, err := remotecache.New(ctx)
	if err != nil {
		fmt.Fprintf(w, "remote cache:\n  error:   %v\n", err)
	} else if rc == nil {
		fmt.Fprintln(w, "remote cache: disabled (SKLP_REMOTE_CACHE_URL / _BUCKET unset)")
	} else {
		st := rc.Status()
		fmt.Fprintln(w, "remote cache:")
		fmt.Fprintf(w, "  endpoint: %s\n", st.Endpoint)
		fmt.Fprintf(w, "  bucket:   %s\n", st.Bucket)
	}

	if last, err := remotecache.ReadLastUpload(repoAbs); err == nil && !last.IsZero() {
		fmt.Fprintf(w, "  last upload: %s\n", last.Format("2006-01-02 15:04:05 MST"))
	} else {
		fmt.Fprintln(w, "  last upload: (none)")
	}
	return nil
}

// dirSize returns the cumulative size of every regular file under root,
// plus the count of regular file entries. A missing root is treated as
// an empty cache (0, 0, nil).
func dirSize(root string) (int64, int, error) {
	var total int64
	var count int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return filepath.SkipDir
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		total += info.Size()
		count++
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return 0, 0, err
	}
	return total, count, nil
}

func humanBytes(n int64) string {
	const (
		k = 1024
		m = 1024 * k
		g = 1024 * m
	)
	switch {
	case n >= g:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(g))
	case n >= m:
		return fmt.Sprintf("%.2f MiB", float64(n)/float64(m))
	case n >= k:
		return fmt.Sprintf("%.2f KiB", float64(n)/float64(k))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
