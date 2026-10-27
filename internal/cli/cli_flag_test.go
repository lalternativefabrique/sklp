package cli

import (
	"context"
	"reflect"
	"testing"
)

// capture records what a command's Run observed, so tests assert on parsed args.
type capture struct {
	ran         bool
	str         map[string]string
	boolv       map[string]bool
	intv        map[string]int
	slice       map[string][]string
	positionals []string
	passthrough []string
	exit        int
}

func runCmd(t *testing.T, root Command, argv ...string) (*capture, int) {
	t.Helper()
	a := NewFlagAdapter()
	code := a.Execute(context.Background(), root, argv)
	return testCap, code
}

var testCap *capture

func leaf(name string, flags []Flag, pos []Positional, exit int) Command {
	return Command{
		Name:        name,
		Flags:       flags,
		Positionals: pos,
		Run: func(ctx context.Context, args Args) int {
			testCap = &capture{
				ran:         true,
				str:         map[string]string{},
				boolv:       map[string]bool{},
				intv:        map[string]int{},
				slice:       map[string][]string{},
				positionals: args.Positionals(),
				passthrough: args.Passthrough(),
				exit:        exit,
			}
			for _, f := range flags {
				switch f.Type {
				case StringFlag:
					testCap.str[f.Name] = args.String(f.Name)
				case BoolFlag:
					testCap.boolv[f.Name] = args.Bool(f.Name)
				case IntFlag:
					testCap.intv[f.Name] = args.Int(f.Name)
				case StringSliceFlag:
					testCap.slice[f.Name] = args.StringSlice(f.Name)
				}
			}
			return exit
		},
	}
}

func TestStringFlagAndDefault(t *testing.T) {
	testCap = nil
	root := leaf("c", []Flag{{Name: "repo", Type: StringFlag, StringDefault: "."}}, nil, 0)
	cap, code := runCmd(t, root, "-repo", "/tmp")
	if code != 0 || !cap.ran {
		t.Fatalf("ran=%v code=%d", cap.ran, code)
	}
	if cap.str["repo"] != "/tmp" {
		t.Fatalf("repo=%q", cap.str["repo"])
	}

	testCap = nil
	cap, _ = runCmd(t, root)
	if cap.str["repo"] != "." {
		t.Fatalf("default repo=%q", cap.str["repo"])
	}
}

func TestFlagAlias(t *testing.T) {
	testCap = nil
	root := leaf("c", []Flag{{Name: "follow", Aliases: []string{"f"}, Type: BoolFlag}}, nil, 0)
	cap, _ := runCmd(t, root, "-f")
	if !cap.boolv["follow"] {
		t.Fatalf("alias -f did not set follow: %+v", cap.boolv)
	}
	testCap = nil
	cap, _ = runCmd(t, root, "-follow")
	if !cap.boolv["follow"] {
		t.Fatalf("-follow did not set follow")
	}
}

func TestPositionalsAmongFlags(t *testing.T) {
	testCap = nil
	root := leaf("c",
		[]Flag{{Name: "space", Type: StringFlag}, {Name: "down", Type: BoolFlag}},
		[]Positional{{Name: "stack"}}, 0)
	// stack positional sits BEFORE -space (legacy parseWithStack tolerance).
	cap, _ := runCmd(t, root, "mystack", "-space", "prod", "-down")
	if got := cap.positionals; !reflect.DeepEqual(got, []string{"mystack"}) {
		t.Fatalf("positionals=%v", got)
	}
	if cap.str["space"] != "prod" || !cap.boolv["down"] {
		t.Fatalf("flags not parsed across positional: %+v %+v", cap.str, cap.boolv)
	}
	// And the value form: -space VALUE must not be captured as the positional.
	testCap = nil
	cap, _ = runCmd(t, root, "-space", "prod", "mystack")
	if !reflect.DeepEqual(cap.positionals, []string{"mystack"}) {
		t.Fatalf("positionals(value-first)=%v", cap.positionals)
	}
}

func TestRepeatableSlice(t *testing.T) {
	testCap = nil
	root := leaf("c", []Flag{{Name: "attach", Type: StringSliceFlag}}, nil, 0)
	cap, _ := runCmd(t, root, "-attach", "a", "-attach", "b")
	if !reflect.DeepEqual(cap.slice["attach"], []string{"a", "b"}) {
		t.Fatalf("attach=%v", cap.slice["attach"])
	}
}

func TestPassthroughDoubleDash(t *testing.T) {
	testCap = nil
	root := Command{
		Name:        "space",
		Passthrough: true,
		Flags:       []Flag{{Name: "repo", Type: StringFlag, StringDefault: "."}},
		Positionals: []Positional{{Name: "name"}},
		Run: func(ctx context.Context, args Args) int {
			testCap = &capture{ran: true, positionals: args.Positionals(), passthrough: args.Passthrough(), str: map[string]string{"repo": args.String("repo")}}
			return 0
		},
	}
	cap, _ := runCmd(t, root, "web", "-repo", "/r", "--", "go", "test", "./...")
	if !reflect.DeepEqual(cap.positionals, []string{"web"}) {
		t.Fatalf("positionals=%v", cap.positionals)
	}
	if cap.str["repo"] != "/r" {
		t.Fatalf("repo=%q", cap.str["repo"])
	}
	if !reflect.DeepEqual(cap.passthrough, []string{"go", "test", "./..."}) {
		t.Fatalf("passthrough=%v", cap.passthrough)
	}
}

func TestRawArgsVerbatim(t *testing.T) {
	testCap = nil
	root := Command{
		Name:    "run",
		RawArgs: true,
		Run: func(ctx context.Context, args Args) int {
			testCap = &capture{ran: true, passthrough: args.Passthrough()}
			return 0
		},
	}
	cap, _ := runCmd(t, root, "ci", "-repo", ".", "-output", "json")
	if !reflect.DeepEqual(cap.passthrough, []string{"ci", "-repo", ".", "-output", "json"}) {
		t.Fatalf("rawargs passthrough=%v", cap.passthrough)
	}
}

func TestEnvFallback(t *testing.T) {
	testCap = nil
	t.Setenv("SKLP_PROFILE", "ci")
	root := leaf("c", []Flag{{Name: "profile", Type: StringFlag, EnvFallback: "SKLP_PROFILE"}}, nil, 0)
	cap, _ := runCmd(t, root)
	if cap.str["profile"] != "ci" {
		t.Fatalf("env fallback profile=%q", cap.str["profile"])
	}
	// Explicit flag overrides the env.
	testCap = nil
	cap, _ = runCmd(t, root, "-profile", "explicit")
	if cap.str["profile"] != "explicit" {
		t.Fatalf("explicit profile=%q", cap.str["profile"])
	}
}

func TestSubDispatch(t *testing.T) {
	testCap = nil
	root := Command{
		Name: "deploy",
		Sub: []Command{
			leaf("ls", nil, nil, 0),
			leaf("get", nil, []Positional{{Name: "name", Required: true}}, 0),
		},
	}
	cap, code := runCmd(t, root, "get", "web")
	if code != 0 || !cap.ran {
		t.Fatalf("get not dispatched: code=%d ran=%v", code, cap.ran)
	}
	if !reflect.DeepEqual(cap.positionals, []string{"web"}) {
		t.Fatalf("positionals=%v", cap.positionals)
	}
}

func TestSubAlias(t *testing.T) {
	testCap = nil
	root := Command{
		Name: "image",
		Sub:  []Command{leaf("push", nil, nil, 0)},
	}
	// alias at the group level: select via the alias on the root token is handled
	// by main's dispatch; here we test child alias resolution.
	root.Sub = []Command{{Name: "ls", Aliases: []string{"list"}, Run: func(ctx context.Context, a Args) int { testCap = &capture{ran: true}; return 0 }}}
	cap, code := runCmd(t, root, "list")
	if code != 0 || !cap.ran {
		t.Fatalf("alias child not dispatched")
	}
}

func TestExitCodePropagates(t *testing.T) {
	testCap = nil
	root := leaf("c", nil, nil, 7)
	_, code := runCmd(t, root)
	if code != 7 {
		t.Fatalf("exit code=%d want 7", code)
	}
}

func TestUnknownSubcommand(t *testing.T) {
	testCap = nil
	root := Command{Name: "cache", Sub: []Command{leaf("status", nil, nil, 0)}}
	_, code := runCmd(t, root, "bogus")
	if code != 2 {
		t.Fatalf("unknown sub code=%d want 2", code)
	}
}

func TestUnknownFlagIsUsageError(t *testing.T) {
	testCap = nil
	root := leaf("c", []Flag{{Name: "repo", Type: StringFlag}}, nil, 0)
	_, code := runCmd(t, root, "-nope")
	if code != 2 {
		t.Fatalf("unknown flag code=%d want 2", code)
	}
}

func TestIntFlag(t *testing.T) {
	testCap = nil
	root := leaf("c", []Flag{{Name: "limit", Type: IntFlag, IntDefault: 0}}, nil, 0)
	cap, _ := runCmd(t, root, "-limit", "5")
	if cap.intv["limit"] != 5 {
		t.Fatalf("limit=%d", cap.intv["limit"])
	}
}

func TestIsSetDistinguishesUnsetFromZero(t *testing.T) {
	cmd := Command{
		Name:  "edit",
		Flags: []Flag{{Name: "body", Type: StringFlag}, {Name: "title", Type: StringFlag}},
	}
	// --body "" is SET (empty clears); --title is unset.
	a, err := ParseLeaf(cmd, []string{"-body", ""})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !a.IsSet("body") {
		t.Fatalf("body should be set")
	}
	if a.IsSet("title") {
		t.Fatalf("title should be unset")
	}
}

func TestIsSetIgnoresEnvFallback(t *testing.T) {
	t.Setenv("SKLP_PROJECT", "envval")
	cmd := Command{Name: "c", Flags: []Flag{{Name: "project", Type: StringFlag, EnvFallback: "SKLP_PROJECT"}}}
	a, err := ParseLeaf(cmd, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.String("project") != "envval" {
		t.Fatalf("env fallback project=%q", a.String("project"))
	}
	// Env fallback must NOT count as explicitly set.
	if a.IsSet("project") {
		t.Fatalf("env fallback should not mark project as set")
	}
}

func TestVariadicPositionals(t *testing.T) {
	testCap = nil
	root := leaf("c", nil, []Positional{{Name: "ids", Variadic: true}}, 0)
	cap, _ := runCmd(t, root, "a", "b", "c")
	if !reflect.DeepEqual(cap.positionals, []string{"a", "b", "c"}) {
		t.Fatalf("variadic positionals=%v", cap.positionals)
	}
}
