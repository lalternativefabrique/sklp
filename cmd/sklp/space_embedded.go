//go:build linux

// space_embedded.go implements `sklp space`: materialise the closed environment
// described by .sklp/space.yaml (base image + toolchain + apt packages + skt
// tools) as a runc rootfs, launch it as a space.Space, and drop the user into
// an interactive shell inside it. This is the on-demand, interactive twin of the
// environment CI builds for every pipeline — same recipe, same builder, same
// Space runtime, just a live shell instead of a step list.
//
// Forms:
//
//	sklp space                 # build the space, open an interactive shell in it
//	sklp space -- go test ./...# run a one-shot command in the space, stream output
//	sklp space build           # build/cache the space rootfs only, no shell
//
// Linux + embedded_runtime only (the Space runtime is runc-backed); the !embedded
// build gets the stub in space_stub.go.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/toolchain"
	"github.com/lalternativefabrique/sklp/pipeline/runnerimage"
	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/space/runnerbuild"
	"github.com/lalternativefabrique/sklp/space/spaceapi"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// spaceCommand declares the `sklp space` verb tree on the façade. The new
// up/ls/get/down/exec/purge sub-verbs are thin transport glue over the runc
// Registry (read args → call Registry → render/exit). `build` and the bare
// interactive shell keep the legacy runSpace path (recipe build + runc exec),
// which has no Registry equivalent yet.
func spaceCommand() cli.Command {
	return cli.Command{
		Name:        "space",
		Short:       "Manage closed environments from .sklp/space.yaml (up | ls | get | down | exec | purge | build)",
		Passthrough: true,
		// Bare `sklp space` lists its subcommands (like every other group), so the
		// user is not silently dropped into a recipe build that fails with a
		// cryptic "open .sklp/space.yaml: no such file". The legacy interactive
		// shell is kept ONLY when the user passes a `-- cmd` tail (or positionals):
		// `sklp space -- bash` still builds the default space and runc-execs into it.
		Run: func(ctx context.Context, args cli.Args) int {
			legacy := append(args.Positionals(), passthroughTail(args)...)
			if len(legacy) == 0 {
				printSpaceUsage()
				return 2
			}
			// A mistyped subcommand must not become a space name: `sklp space dow
			// <name>` would otherwise pull a base image and build a whole rootfs
			// named "dow" before anything looked wrong. Only reject near-misses of
			// a real subcommand — an arbitrary word stays a legitimate space name.
			if suggestion := nearestSpaceSubcommand(legacy[0]); suggestion != "" {
				fmt.Fprintf(os.Stderr, "sklp space: unknown subcommand %q — did you mean %q?\n", legacy[0], suggestion)
				fmt.Fprintf(os.Stderr, "  (to build a space actually named %q, use: sklp space up %s)\n", legacy[0], legacy[0])
				return 2
			}
			runSpace(legacy)
			return 0
		},
		Sub: []cli.Command{
			spaceUpCommand(),
			spaceLsCommand(),
			spaceGetCommand(),
			spaceStopCommand(),
			spaceDownCommand(),
			spaceExecCommand(),
			spacePurgeCommand(),
			{
				Name:    "build",
				Short:   "Build the space rootfs only (no shell)",
				RawArgs: true,
				Run: func(ctx context.Context, args cli.Args) int {
					runSpace(append([]string{"build"}, args.Passthrough()...))
					return 0
				},
			},
		},
	}
}

// printSpaceUsage lists the space subcommands for a bare `sklp space`. Derived
// from the command tree (spaceCommand().Sub) so it never drifts from the real
// set. Mirrors the framework's group-usage format.
func printSpaceUsage() {
	w := os.Stderr
	fmt.Fprintln(w, "Usage: sklp space <subcommand> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Subcommands:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range spaceCommand().Sub {
		if c.Hidden {
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\n", c.Name, c.Short)
	}
	_ = tw.Flush()
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run a command in the default space with: sklp space -- <cmd>")
}

// nearestSpaceSubcommand returns the subcommand `word` most likely meant, or ""
// when it looks like a genuine space name. The bar is deliberately high (one
// edit away, or a prefix of 3+ chars): a false positive blocks a legitimate
// `sklp space mydev`, while a false negative only costs the old behaviour.
func nearestSpaceSubcommand(word string) string {
	for _, c := range spaceCommand().Sub {
		if c.Name == word {
			return "" // exact match is routed by the framework, not here
		}
		if editDistanceAtMostOne(word, c.Name) || isTransposition(word, c.Name) {
			return c.Name
		}
		if len(word) >= 3 && len(word) < len(c.Name) && strings.HasPrefix(c.Name, word) {
			return c.Name
		}
	}
	return ""
}

// editDistanceAtMostOne reports whether a and b differ by at most one insertion,
// deletion or substitution — the typo class that hits a fixed verb list (dow,
// dwon, purg, exce).
func editDistanceAtMostOne(a, b string) bool {
	if a == b {
		return true
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	if len(b)-len(a) > 1 {
		return false
	}
	if len(a) == len(b) {
		diff := 0
		for i := range a {
			if a[i] != b[i] {
				diff++
			}
		}
		return diff == 1
	}
	// b is exactly one longer: a must be b with one char removed.
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return a[i:] == b[i+1:]
		}
	}
	return true
}

// isTransposition reports whether a is b with two adjacent characters swapped
// (dwon/down, exce/exec) — the most common typing slip, and two edits away, so
// editDistanceAtMostOne cannot see it.
func isTransposition(a, b string) bool {
	if len(a) != len(b) || a == b {
		return false
	}
	for i := 0; i+1 < len(a); i++ {
		if a[i] == b[i] {
			continue
		}
		swapped := []byte(a)
		swapped[i], swapped[i+1] = swapped[i+1], swapped[i]
		return string(swapped) == b
	}
	return false
}

// passthroughTail re-prefixes a captured `--` tail so the legacy runSpace parser
// (which splits on `--` itself) sees the same argv the user typed.
func passthroughTail(args cli.Args) []string {
	tail := args.Passthrough()
	if len(tail) == 0 {
		return nil
	}
	return append([]string{"--"}, tail...)
}

// newSpaceRegistry builds the runc Registry, injecting control.ListSpaces so the
// Supervised column reflects which spaces are live deploys (a seam: package
// runtime cannot import deploy/control without a cycle).
func newSpaceRegistry() (spaceapi.Registry, error) {
	return space.NewRegistry(control.ListSpaces)
}

// spaceUpCommand: create a named space and open an interactive shell in it,
// FOREGROUND. The session holds the SpaceNet worker alive until the user exits —
// the space is the shell. (An earlier detach-then-reattach Registry path lost the
// worker between Up and Exec, so the shell could never attach: "is up" then "not
// running".) Use --image for a bare OCI image, or a .sklp/space.yaml recipe.
func spaceUpCommand() cli.Command {
	return cli.Command{
		Name:        "up",
		Short:       "Open a space and an interactive shell in it (from --image or a space.yaml recipe)",
		Positionals: []cli.Positional{{Name: "name", Required: true}},
		Flags: []cli.Flag{
			{Name: "image", Type: cli.StringFlag, Usage: "open a bare space straight from this OCI image (no recipe / space.yaml needed)"},
			{Name: "file", Type: cli.StringFlag, Usage: "explicit path to the space recipe (space.yaml); overrides the .sklp lookup"},
			{Name: "repo", Type: cli.StringFlag, StringDefault: ".", Usage: "project repo holding .sklp/space.yaml"},
			{Name: "mount", Type: cli.StringSliceFlag, Usage: "bind a host path into the space, repeatable: /host/dir:/in/space[:ro]; a later mount covers an earlier one (/dev/null:/work/.env:ro hides a file)"},
			{Name: "workdir", Type: cli.StringFlag, Usage: "directory the shell starts in, e.g. a -mount destination"},
		},
		Run: func(ctx context.Context, args cli.Args) int {
			name, ok := firstPositional(args)
			if !ok {
				fmt.Fprintln(os.Stderr, "sklp space up: a space name is required")
				return 2
			}
			mounts, err := parseSpaceMounts(args.StringSlice("mount"))
			if err != nil {
				fmt.Fprintln(os.Stderr, "sklp space up:", err)
				return 2
			}
			code, err := spaceMain(ctx, spaceOpts{
				name:    name,
				repo:    args.String("repo"),
				file:    args.String("file"),
				image:   args.String("image"),
				mounts:  mounts,
				workdir: args.String("workdir"),
			})
			if err != nil {
				fmt.Fprintln(os.Stderr, "sklp space up:", err)
				return 1
			}
			return code
		},
	}
}

// spaceLsCommand: list every space (runc-state scan), incl. deploy rows.
func spaceLsCommand() cli.Command {
	return cli.Command{
		Name:  "ls",
		Short: "List all spaces (running or stopped)",
		Run: func(ctx context.Context, args cli.Args) int {
			reg, err := newSpaceRegistry()
			if err != nil {
				fmt.Fprintln(os.Stderr, "sklp space ls:", err)
				return 1
			}
			infos, err := reg.List(ctx)
			if err != nil {
				fmt.Fprintln(os.Stderr, "sklp space ls:", err)
				return 1
			}
			renderSpaceList(os.Stdout, infos)
			return 0
		},
	}
}

// spaceGetCommand: one space's detail.
func spaceGetCommand() cli.Command {
	return cli.Command{
		Name:        "get",
		Short:       "Show one space's detail",
		Positionals: []cli.Positional{{Name: "name", Required: true, Complete: "space"}},
		Run: func(ctx context.Context, args cli.Args) int {
			name, ok := firstPositional(args)
			if !ok {
				fmt.Fprintln(os.Stderr, "sklp space get: a space name is required")
				return 2
			}
			reg, err := newSpaceRegistry()
			if err != nil {
				fmt.Fprintln(os.Stderr, "sklp space get:", err)
				return 1
			}
			info, err := reg.Get(ctx, name)
			if err != nil {
				if errors.Is(err, spaceapi.ErrSpaceNotFound) {
					fmt.Fprintf(os.Stderr, "sklp space get: space %q not found\n", name)
					return 1
				}
				fmt.Fprintln(os.Stderr, "sklp space get:", err)
				return 1
			}
			renderSpaceDetail(os.Stdout, info)
			return 0
		},
	}
}

// spaceStopCommand: halt a space but keep its data, the counterpart to down.
func spaceStopCommand() cli.Command {
	return cli.Command{
		Name:        "stop",
		Short:       "Stop a space, keeping its named volumes (data survives)",
		Positionals: []cli.Positional{{Name: "name", Required: true, Complete: "space"}},
		Run: func(ctx context.Context, args cli.Args) int {
			name, ok := firstPositional(args)
			if !ok {
				fmt.Fprintln(os.Stderr, "sklp space stop: a space name is required")
				return 2
			}
			reg, err := newSpaceRegistry()
			if err != nil {
				fmt.Fprintln(os.Stderr, "sklp space stop:", err)
				return 1
			}
			info, _ := reg.Get(ctx, name)
			if err := reg.Stop(ctx, name); err != nil {
				if errors.Is(err, spaceapi.ErrSpaceNotFound) {
					fmt.Fprintf(os.Stderr, "sklp space stop: space %q not found\n", name)
					return 1
				}
				fmt.Fprintln(os.Stderr, "sklp space stop:", err)
				return 1
			}
			fmt.Printf("space %q stopped%s — `sklp space up %s` to bring it back\n",
				name, keptVolumesSuffix(info.Volumes), name)
			return 0
		},
	}
}

// keptVolumesSuffix names the volumes a stop preserved, so the operator can tell
// at a glance that this was not the destructive verb.
func keptVolumesSuffix(volumes []string) string {
	if len(volumes) == 0 {
		return ""
	}
	return fmt.Sprintf(" (kept %d volume(s): %s)", len(volumes), strings.Join(volumes, ", "))
}

// spaceDownCommand: stop + remove a space, volumes included.
func spaceDownCommand() cli.Command {
	return cli.Command{
		Name:        "down",
		Short:       "Stop + remove a space and its named volumes",
		Positionals: []cli.Positional{{Name: "name", Required: true, Complete: "space"}},
		Run: func(ctx context.Context, args cli.Args) int {
			name, ok := firstPositional(args)
			if !ok {
				fmt.Fprintln(os.Stderr, "sklp space down: a space name is required")
				return 2
			}
			reg, err := newSpaceRegistry()
			if err != nil {
				fmt.Fprintln(os.Stderr, "sklp space down:", err)
				return 1
			}
			info, getErr := reg.Get(ctx, name)
			existed := getErr == nil
			if err := reg.Down(ctx, name); err != nil {
				fmt.Fprintln(os.Stderr, "sklp space down:", err)
				return 1
			}
			if !existed {
				fmt.Printf("space %q has no state on disk; reaped any leftover container cgroup\n", name)
				return 0
			}
			fmt.Printf("space %q torn down%s\n", name, deletedVolumesSuffix(info.Volumes))
			return 0
		},
	}
}

// deletedVolumesSuffix names the volumes a teardown destroyed. Data deletion is
// never silent: the operator must see what is gone, since nothing will bring it
// back.
func deletedVolumesSuffix(volumes []string) string {
	if len(volumes) == 0 {
		return ""
	}
	return fmt.Sprintf(" — deleted %d volume(s): %s", len(volumes), strings.Join(volumes, ", "))
}

// spaceExecCommand: exec / interactive shell into a live space.
//
// RawArgs (docker/kubectl-style, like `deploy exec`): the first token is the
// space name, EVERYTHING after it is the command verbatim — no `--` required and
// the command may carry its own dashes (`space exec dev go test -run X`). A bare
// `--` before the command is tolerated for symmetry with the documented form.
// The default command is `sh`, so `space exec dev` opens a shell.
//
// `-c <svc>` picks WHICH container to enter in a multi-service space (dev/ci/
// deploy run one per service). It is a flag rather than a second positional
// because RawArgs gives the whole tail to the command: a bare `space exec dev
// core` cannot be told apart from running the binary `core`.
func spaceExecCommand() cli.Command {
	return cli.Command{
		Name:    "exec",
		Short:   "Exec a command (or open a shell) in a live space (-c <svc> to pick a container)",
		RawArgs: true,
		// Metadata-only (RawArgs owns parsing): feeds shell completion the space
		// name slot so `space exec <TAB>` offers existing spaces.
		Positionals: []cli.Positional{{Name: "name", Required: true, Complete: "space"}},
		Run: func(ctx context.Context, args cli.Args) int {
			name, container, cmd := parseSpaceExecArgs(args.Passthrough())
			if name == "" {
				fmt.Fprintln(os.Stderr, "sklp space exec: a space name is required")
				fmt.Fprintln(os.Stderr, "usage: sklp space exec <name> [-c <svc>] [cmd args...]")
				return 2
			}
			reg, err := newSpaceRegistry()
			if err != nil {
				fmt.Fprintln(os.Stderr, "sklp space exec:", err)
				return 1
			}
			return spaceExecInto(ctx, reg, name, container, cmd)
		},
	}
}

// parseSpaceExecArgs splits the raw exec tail into the space name, the optional
// -c container target, and the command argv. The first token is the name; the
// rest is the command verbatim, except a leading `-c <svc>` (or `-c=<svc>`),
// which selects the container. A single leading `--` between the name and the
// command is dropped (the documented `space exec dev -- go test` form), so both
// `dev go test` and `dev -- go test` yield the same command. Only a LEADING -c
// is consumed: past that point the tail belongs to the command, which may
// legitimately carry its own -c (`space exec dev sh -c 'echo hi'`).
func parseSpaceExecArgs(tail []string) (name, container string, cmd []string) {
	if len(tail) == 0 {
		return "", "", nil
	}
	name = tail[0]
	cmd = tail[1:]
	if len(cmd) > 0 && cmd[0] == "--" {
		cmd = cmd[1:]
	}
	switch {
	case len(cmd) > 0 && strings.HasPrefix(cmd[0], "-c="):
		container = strings.TrimPrefix(cmd[0], "-c=")
		cmd = cmd[1:]
	case len(cmd) > 1 && cmd[0] == "-c":
		container = cmd[1]
		cmd = cmd[2:]
	}
	if len(cmd) > 0 && cmd[0] == "--" {
		cmd = cmd[1:]
	}
	return name, container, cmd
}

// spacePurgeCommand: down every space with no running container, volumes incl.
func spacePurgeCommand() cli.Command {
	return cli.Command{
		Name:  "purge",
		Short: "Tear down every space with no running container, volumes included",
		Run: func(ctx context.Context, args cli.Args) int {
			reg, err := newSpaceRegistry()
			if err != nil {
				fmt.Fprintln(os.Stderr, "sklp space purge:", err)
				return 1
			}
			// Read the volumes BEFORE the purge: once a space is gone there is
			// nothing left to enumerate, and a data deletion must still be named.
			volumes := volumesBySpace(ctx, reg)
			reaped, err := reg.Purge(ctx)
			if err != nil {
				fmt.Fprintln(os.Stderr, "sklp space purge:", err)
				return 1
			}
			if len(reaped) == 0 {
				fmt.Println("no idle spaces to purge")
				return 0
			}
			fmt.Printf("purged %d space(s): %s\n", len(reaped), strings.Join(reaped, ", "))
			if names := qualifiedVolumes(volumes, reaped); len(names) > 0 {
				fmt.Printf("deleted %d volume(s): %s\n", len(names), strings.Join(names, ", "))
			}
			return 0
		},
	}
}

// volumesBySpace snapshots each space's named volumes so a teardown can report
// what it destroyed after the state is gone.
func volumesBySpace(ctx context.Context, reg spaceapi.Registry) map[string][]string {
	infos, err := reg.List(ctx)
	if err != nil {
		return nil
	}
	out := map[string][]string{}
	for _, info := range infos {
		if len(info.Volumes) > 0 {
			out[info.Name] = info.Volumes
		}
	}
	return out
}

// qualifiedVolumes renders the volumes of the reaped spaces as space/volume, so
// two spaces each holding a "pgdata" stay distinguishable.
func qualifiedVolumes(bySpace map[string][]string, reaped []string) []string {
	var out []string
	for _, name := range reaped {
		for _, v := range bySpace[name] {
			out = append(out, name+"/"+v)
		}
	}
	sort.Strings(out)
	return out
}

// spaceExecInto attaches the user's stdio to a space, requesting a PTY when the
// terminal is interactive (raw mode locally), and returns the exit code. Shared
// by `space up` (post-create attach) and `space exec`.
func spaceExecInto(ctx context.Context, reg spaceapi.Registry, name, container string, cmd []string) int {
	opts := spaceapi.ExecOpts{Cmd: cmd, Container: container}
	eio := spaceapi.ExecIO{Stdin: os.Stdin, Stdout: os.Stdout}
	interactive := len(cmd) == 0 && isSpaceTerminal()
	var restore func()
	if interactive {
		opts.TTY = true
		fd := int(os.Stdin.Fd())
		if st, terr := term.MakeRaw(fd); terr == nil {
			restore = func() { _ = term.Restore(fd, st) }
		}
		if w, h, serr := term.GetSize(int(os.Stdout.Fd())); serr == nil {
			eio.Cols, eio.Rows = uint16(w), uint16(h)
		}
	}
	code, err := reg.Exec(ctx, name, opts, eio)
	if restore != nil {
		restore()
	}
	if err != nil {
		switch {
		case errors.Is(err, spaceapi.ErrSpaceNotFound):
			fmt.Fprintf(os.Stderr, "sklp space exec: space %q not found (sklp space up %s)\n", name, name)
		case errors.Is(err, spaceapi.ErrSpaceNotRunning):
			fmt.Fprintf(os.Stderr, "sklp space exec: space %q is not running (sklp space up %s)\n", name, name)
		case errors.Is(err, spaceapi.ErrContainerNotFound):
			fmt.Fprintf(os.Stderr, "sklp space exec: %v (sklp space get %s lists them)\n", err, name)
		case errors.Is(err, spaceapi.ErrContainerNotRunning):
			fmt.Fprintf(os.Stderr, "sklp space exec: %v\n", err)
		default:
			fmt.Fprintln(os.Stderr, "sklp space exec:", err)
		}
		return 1
	}
	return code
}

// spaceName scopes the runc state + SpaceNet for an interactive space, kept
// distinct from dev/deploy spaces so a `sklp space` shell never collides with a
// running `sklp dev` stack.
const spaceName = "sklp-space"

// defaultSpaceBaseImage is the base used when space.yaml sets no base_image: a
// minimal glibc rootfs (the recipe layers the toolchain on top). Mirrors the dev
// backend's default; overridable via SKLP_DEV_BASE_IMAGE.
const defaultSpaceBaseImage = "debian:trixie-slim"

func runSpace(args []string) {
	fs := flag.NewFlagSet("sklp space", flag.ExitOnError)
	repo := fs.String("repo", ".", "path of the project repo containing .sklp/space.yaml")
	buildOnly := false

	// Subcommand / command split:
	//   space build       -> build the rootfs only
	//   space -- cmd...    -> one-shot command in the space
	//   space [flags]      -> interactive shell
	var cmd []string
	if len(args) > 0 && args[0] == "build" {
		buildOnly = true
		args = args[1:]
	}
	if pre, post, ok := splitAtDoubleDash(args); ok {
		args = pre
		cmd = post
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), unix.SIGINT, unix.SIGTERM)
	defer stop()

	code, err := spaceMain(ctx, spaceOpts{repo: *repo, buildOnly: buildOnly, cmd: cmd})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp space:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// spaceOpts configures a foreground space session.
type spaceOpts struct {
	name      string   // space name (runc-state + SpaceNet scope); "" → legacy shared
	repo      string   // project root for recipe lookup (recipe mode)
	file      string   // explicit recipe path (recipe mode); overrides repo lookup
	image     string   // OCI image ref → bare-image mode (no recipe build)
	buildOnly bool     // recipe mode only: build the rootfs and stop
	cmd       []string // command to run; empty → interactive shell
	mounts    []space.DevMount
	workdir   string // overrides the image's working directory
}

// spaceMain prepares the space rootfs and (unless buildOnly) runs opts.cmd in it
// FOREGROUND — the same process holds the SpaceNet worker alive for the whole
// session, so the shell stays attached until the user exits (this is why up must
// route here, not through a detach-then-reattach Registry path which loses the
// worker between calls). Returns the command's exit code.
func spaceMain(ctx context.Context, opts spaceOpts) (int, error) {
	name := opts.name
	if name == "" {
		name = spaceName
	}
	sp, err := space.NewSpace(name)
	if err != nil {
		return -1, err
	}
	defer sp.Close()

	var rootfs, workdir string
	var bakedEnv []string
	if opts.image != "" {
		// Bare-image mode: the OCI image IS the rootfs (writable via the overlay),
		// no recipe build, no space.yaml. env/workdir from the image config.
		fmt.Fprintf(os.Stderr, "[sklp space] preparing image %s\n", opts.image)
		rfs, env, ierr := space.PrepareImageWithEnv(ctx, sp.CacheRoot(), opts.image)
		if ierr != nil {
			return -1, fmt.Errorf("space image %q: %w", opts.image, ierr)
		}
		rootfs, bakedEnv = rfs, env
	} else {
		recipePath := opts.file
		if recipePath == "" {
			recipePath = runnerimage.ResolvePath(opts.repo)
		}
		if _, serr := os.Stat(recipePath); serr != nil {
			return -1, fmt.Errorf("no space recipe at %s: open a bare space with `--image <ref>`, or add a .sklp/space.yaml (or pass --file)", recipePath)
		}
		cfg, lerr := runnerimage.Load(recipePath)
		if lerr != nil {
			return -1, fmt.Errorf("load space recipe: %w", lerr)
		}
		r := cfg.Runner
		// The recipe must render in from-base mode (it has no skalpai sources to
		// compile sklp from), so force a concrete base when none is declared.
		if r.BaseImage == "" {
			if v := os.Getenv("SKLP_DEV_BASE_IMAGE"); v != "" {
				r.BaseImage = v
			} else {
				r.BaseImage = defaultSpaceBaseImage
			}
		}
		fmt.Fprintf(os.Stderr, "[sklp space] preparing base %s\n", r.BaseImage)
		baseRootfs, perr := space.PrepareImage(ctx, sp.CacheRoot(), r.BaseImage)
		if perr != nil {
			return -1, fmt.Errorf("prepare base %q: %w", r.BaseImage, perr)
		}
		// Bake the host skt so the recipe's `RUN skt install …` steps resolve.
		var bake []runnerbuild.BakeFile
		if sktPath, serr := toolchain.EnsureSkt(); serr == nil {
			bake = append(bake, runnerbuild.BakeFile{Src: sktPath, Dest: runnerimage.SktBinPath(), Mode: 0o755})
		}
		fmt.Fprintln(os.Stderr, "[sklp space] building space rootfs (cached by recipe hash)")
		res, berr := runnerbuild.Build(ctx, sp.CacheRoot(), baseRootfs, r, bake, runnerbuild.UnshareStepRunner{
			Stdout: os.Stderr,
			Stderr: os.Stderr,
		})
		if berr != nil {
			return -1, fmt.Errorf("build space rootfs: %w", berr)
		}
		if opts.buildOnly {
			fmt.Fprintf(os.Stderr, "[sklp space] built %s\n", res.Rootfs)
			return 0, nil
		}
		rootfs, workdir, bakedEnv = res.Rootfs, res.Workdir, bakedEnvSlice(res)
	}

	// Interactive shell when no command was given.
	shellCmd := opts.cmd
	interactive := false
	if len(shellCmd) == 0 {
		shellCmd = []string{"sh"}
		interactive = isSpaceTerminal()
	}

	if opts.workdir != "" {
		workdir = opts.workdir
	}
	ds := space.DeployService{
		Name:             "shell",
		DevMounts:        opts.mounts,
		RootfsDir:        rootfs,
		RootfsCfg:        space.ImageConfig{Env: bakedEnv, WorkingDir: workdir},
		Env:              bakedEnv,
		IgnoreEntrypoint: true,
		// Park the container init on sklp pause; we attach the real work via
		// `runc exec` (RuncExecTTY) so the shell gets a fresh PTY session.
		PauseInit: true,
	}
	if _, err := sp.Start(ctx, ds); err != nil {
		return -1, fmt.Errorf("start space: %w", err)
	}
	defer sp.Stop(ds.Name)

	runcBin, runcRoot, id := sp.ExecCoords(ds.Name)

	var rows, cols uint16
	var restore func()
	if interactive {
		fd := int(os.Stdin.Fd())
		if st, terr := term.MakeRaw(fd); terr == nil {
			restore = func() { _ = term.Restore(fd, st) }
			defer restore()
		}
		if w, h, serr := term.GetSize(int(os.Stdout.Fd())); serr == nil {
			cols, rows = uint16(w), uint16(h)
		}
	}

	code, err := space.RuncExecTTY(ctx, runcBin, runcRoot, id, shellCmd,
		stdioPeer{in: os.Stdin, out: os.Stdout}, rows, cols)
	if restore != nil {
		restore()
	}
	if err != nil {
		return -1, fmt.Errorf("exec in space: %w", err)
	}
	return code, nil
}

// stdioPeer adapts the process stdin/stdout into the single io.ReadWriter
// RuncExecTTY proxies against the container PTY: reads pull the user's
// keystrokes from stdin, writes push the container's PTY output to stdout.
type stdioPeer struct {
	in  io.Reader
	out io.Writer
}

func (p stdioPeer) Read(b []byte) (int, error)  { return p.in.Read(b) }
func (p stdioPeer) Write(b []byte) (int, error) { return p.out.Write(b) }

// bakedEnvSlice flattens the built recipe env into KEY=value lines in
// declaration order (mirrors the dev backend).
func bakedEnvSlice(res runnerbuild.Result) []string {
	out := make([]string, 0, len(res.Env))
	for _, k := range res.EnvOrder {
		out = append(out, k+"="+res.Env[k])
	}
	return out
}

// isSpaceTerminal reports whether both stdin and stdout are TTYs, so we only
// request raw-mode interactivity when the user is actually at a terminal.
var isSpaceTerminal = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// renderSpaceList prints the `space ls` table: one row per space with its
// aggregate status, container count, volume count, and whether it is a deploy.
func renderSpaceList(w io.Writer, infos []spaceapi.SpaceInfo) {
	if len(infos) == 0 {
		fmt.Fprintln(w, "no spaces")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tCONTAINERS\tVOLUMES\tSUPERVISED")
	for _, s := range infos {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\n",
			s.Name, dashIfEmpty(s.Status), len(s.Containers), len(s.Volumes), yesNo(s.Supervised))
	}
	_ = tw.Flush()
}

// renderSpaceDetail prints `space get`: the space header plus its containers and
// volumes.
func renderSpaceDetail(w io.Writer, s spaceapi.SpaceInfo) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "Name:\t%s\n", s.Name)
	fmt.Fprintf(tw, "Status:\t%s\n", dashIfEmpty(s.Status))
	fmt.Fprintf(tw, "Supervised:\t%s\n", yesNo(s.Supervised))
	_ = tw.Flush()
	if len(s.Containers) > 0 {
		fmt.Fprintln(w, "\nContainers:")
		ctw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(ctw, "  ID\tSTATUS")
		for _, c := range s.Containers {
			fmt.Fprintf(ctw, "  %s\t%s\n", c.ID, c.Status)
		}
		_ = ctw.Flush()
	}
	if len(s.Volumes) > 0 {
		fmt.Fprintf(w, "\nVolumes: %s\n", strings.Join(s.Volumes, ", "))
	}
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// parseSpaceMounts reads -mount values, "/host:/space" or "/host:/space:ro".
// Both sides must be absolute and the host side must exist: a missing
// source would otherwise be created empty by the runtime, hiding a typo.
func parseSpaceMounts(values []string) ([]space.DevMount, error) {
	mounts := make([]space.DevMount, 0, len(values))
	for _, v := range values {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || len(parts) > 3 || (len(parts) == 3 && parts[2] != "ro" && parts[2] != "rw") {
			return nil, fmt.Errorf("-mount %q: want /host/path:/space/path[:ro]", v)
		}
		if !filepath.IsAbs(parts[0]) || !filepath.IsAbs(parts[1]) {
			return nil, fmt.Errorf("-mount %q: both paths must be absolute", v)
		}
		if _, err := os.Stat(parts[0]); err != nil {
			return nil, fmt.Errorf("-mount %q: %w", v, err)
		}
		mounts = append(mounts, space.DevMount{Source: parts[0], Destination: parts[1], ReadOnly: len(parts) == 3 && parts[2] == "ro"})
	}
	return mounts, nil
}
