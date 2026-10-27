package main

import (
	"strconv"
	"strings"
)

type listenOptions struct {
	Addr           string
	Advertise      string
	WorkRoot       string
	Concurrency    int
	AllowHostSteps bool
}

// parseListenArgs extracts the run-API flags from a `sklp run` tail. It
// reports ok=false when --listen is absent, leaving the normal one-shot flow
// untouched. Both `--flag value` and `--flag=value` are accepted, as is the
// single-dash spelling the rest of the CLI tolerates.
//
// concurrency defaults to 1 (the historical one-run-at-a-time behaviour) and
// is clamped to that floor on a non-positive or unparsable value, so a typo'd
// flag degrades to the safe default instead of silently unlimiting the node.
func parseListenArgs(args []string) (o listenOptions, install, ok bool) {
	o.Concurrency = 1
	for i := 0; i < len(args); i++ {
		name, value, hasValue := splitListenFlag(args[i])
		// --install is a boolean: consuming the next token as its value would
		// swallow a following flag's argument.
		if !hasValue && name != "install" && name != "allow-host-steps" && i+1 < len(args) && !isFlag(args[i+1]) {
			value, i = args[i+1], i+1
		}
		switch name {
		case "listen":
			o.Addr, ok = value, true
		case "advertise":
			o.Advertise = value
		case "work-root":
			o.WorkRoot = value
		case "concurrency":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				o.Concurrency = n
			}
		case "install":
			install = true
		case "allow-host-steps":
			o.AllowHostSteps = true
		}
	}
	if ok && o.Addr == "" {
		o.Addr = ":6464"
	}
	return o, install, ok
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }

// splitListenFlag parses one argument into a flag name and, for the `=` form,
// its inline value. Local to this file so the run API stays buildable on every
// platform: the CLI's shared splitFlag lives in a Linux-only file.
func splitListenFlag(a string) (name, value string, hasInline bool) {
	if !strings.HasPrefix(a, "-") {
		return "", "", false
	}
	a = strings.TrimLeft(a, "-")
	if i := strings.IndexByte(a, '='); i >= 0 {
		return a[:i], a[i+1:], true
	}
	return a, "", false
}
