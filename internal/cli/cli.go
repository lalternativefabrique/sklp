// Package cli is the framework-agnostic command façade for sklp.
//
// It exists to STOP the coupling to the stdlib `flag` package + hand-written
// `switch` dispatch that the CLI grew (41 sites). Commands declare themselves
// DECLARATIVELY (name, flags, positionals, sub-commands) and receive their
// parsed inputs through the Args interface — they never import `flag`, `cobra`,
// or any concrete parser. A single Adapter implementation (flag today, cobra or
// another lib tomorrow) translates a Command tree into the underlying engine.
//
// Two orthogonal seams meet here:
//
//   - THIS package is the CLI seam: it decouples commands from the parsing
//     framework. Swapping the framework means writing one new Adapter; no
//     Command changes.
//   - The BUSINESS seam (e.g. a space/deploy Registry interface) is separate: a
//     Command.Run is a thin adapter that parses nothing and calls the business
//     handler, so the SAME handler also backs an HTTP API. Run holds transport
//     glue only — never domain logic.
//
// Invariant: nothing under this package may import `flag` or any CLI framework
// except the adapter files (cli_flag.go, future cli_cobra.go). Commands depend
// only on the types declared here.
package cli

import "context"

// Command is one node in the CLI tree: a leaf (has Run) or a group (has Sub),
// or both (a parent that also runs when invoked bare). It is a pure value —
// declaring a Command never touches a parser.
type Command struct {
	// Name is the token that selects this command (e.g. "deploy", "ls").
	Name string
	// Aliases are alternate tokens that select this same command
	// (e.g. "img" for "image", "follow" handled at the flag level not here).
	Aliases []string
	// Short is the one-line help shown in a parent's command list.
	Short string
	// Long is the full help shown for `sklp <cmd> -h` (optional).
	Long string

	// Flags this command accepts. Compose shared groups (e.g. net flags,
	// -profile) by appending their Flag slices — a group is just a []Flag.
	Flags []Flag
	// Positionals this command accepts, in order. The last may be Variadic to
	// soak up remaining tokens (e.g. an issue id, then nothing; or a service
	// name). Positional placement among flags is tolerated by the adapter
	// (mirrors the legacy parseWithStack / parseWithPositional behaviour).
	Positionals []Positional

	// Passthrough enables the `--` hard separator: tokens after a bare `--` are
	// NOT parsed as flags and are returned verbatim via Args.Passthrough()
	// (e.g. `space up web -- go test ./...`, `deploy exec svc -- sh`).
	Passthrough bool
	// RawArgs, when true, disables flag parsing entirely for this command: every
	// token after the command name flows verbatim to Run via Args.Passthrough().
	// The escape hatch for `sklp run …` whose tail is handed to runner.Main as-is.
	RawArgs bool

	// Hidden keeps this command out of help/listing (e.g. __complete,
	// __supervise). It is still dispatchable.
	Hidden bool

	// Sub are child commands. A command with Sub is a group; selecting a child
	// token dispatches to it. A group with no Run prints its command list when
	// invoked bare.
	Sub []Command

	// Run executes the command. It receives the parsed Args and returns the
	// process exit code (0 = success, 2 = usage, etc.) — it MUST NOT call
	// os.Exit, so the adapter owns the process lifecycle and Run stays testable.
	// nil Run on a leaf is a programming error; on a group it means "list subs".
	Run func(ctx context.Context, args Args) int
}

// FlagType is the closed set of flag value kinds the façade supports. It mirrors
// exactly what the legacy flag sites use (string/bool/int/repeatable-string), so
// every existing command is expressible.
type FlagType int

const (
	StringFlag FlagType = iota
	BoolFlag
	IntFlag
	// StringSliceFlag is a repeatable string flag (e.g. -attach a -attach b),
	// collected in order. Read via Args.StringSlice.
	StringSliceFlag
)

// Flag declares one option a command accepts. Defaults are typed via the *Default
// fields matching Type; the adapter wires the right one.
type Flag struct {
	Name    string   // canonical name, no dash (e.g. "space", "follow")
	Aliases []string // alternate names (e.g. "f" for "follow", "addr" for "server")
	Usage   string   // help text
	Type    FlagType

	// Defaults — only the field matching Type is honoured.
	StringDefault string
	BoolDefault   bool
	IntDefault    int
	// (StringSlice defaults to empty.)

	// EnvFallback, when non-empty, is an env var consulted when the flag is
	// unset (mirrors -profile→SKLP_PROFILE, -project→SKLP_PROJECT, -token→
	// SKLP_TOKEN). The adapter applies it after parsing, before Run.
	EnvFallback string
}

// Positional declares one positional argument.
type Positional struct {
	Name     string // for help/usage (e.g. "service", "issue-id")
	Required bool
	Variadic bool // last-only: soaks up all remaining non-flag tokens

	// Complete names a `sklp __complete <subject>` subject for dynamic value
	// completion at this slot (e.g. "space" → existing space names). Empty means
	// no dynamic candidates. Generated shell scripts shell back into the helper
	// so runtime resolution lives once in Go, never duplicated per shell.
	Complete string
}

// Args is how a Command.Run reads its parsed inputs WITHOUT depending on any
// parser. The adapter implements it. Lookups are by canonical flag name (not
// alias). Missing/unset typed flags return their declared default.
type Args interface {
	// String returns the value of a StringFlag (or its default/env fallback).
	String(name string) string
	// Bool returns the value of a BoolFlag.
	Bool(name string) bool
	// Int returns the value of an IntFlag.
	Int(name string) int
	// StringSlice returns the collected values of a StringSliceFlag, in order.
	StringSlice(name string) []string

	// IsSet reports whether the flag was explicitly provided on the command line
	// (by canonical name or any alias), as opposed to left at its default. It
	// lets a Run distinguish "unset" from "set to the zero value" — needed for
	// PATCH-style verbs where an empty string is a meaningful clear (e.g. `issue
	// edit --body ""`). EnvFallback does NOT count as set.
	IsSet(name string) bool

	// Positionals returns the positional tokens in declaration order. A variadic
	// tail is flattened in. Use len() to detect optional positionals.
	Positionals() []string
	// Passthrough returns the verbatim tokens after a bare `--` (Passthrough
	// commands) or every token (RawArgs commands). Empty otherwise.
	Passthrough() []string
}

// Adapter binds a Command tree to a concrete parsing engine and runs it.
// flagAdapter (cli_flag.go) is the default; a cobra adapter can be dropped in
// later without touching any Command. Execute parses argv (already stripped of
// the program name), dispatches to the selected command, and returns its exit
// code.
type Adapter interface {
	Execute(ctx context.Context, root Command, argv []string) int
}
