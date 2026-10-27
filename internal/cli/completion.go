// completion.go GENERATES shell completion scripts by walking a Command tree —
// the single source of truth. This is the no-cobra equivalent of cobra's
// auto-completion: reflect the declared verbs, sub-verbs, flags, aliases and
// dynamic-value subjects and emit bash/zsh/fish scripts that never drift from
// the tree. Hidden commands are skipped. Framework-free (no flag/cobra import),
// consistent with the package façade invariant.
package cli

import (
	"fmt"
	"sort"
	"strings"
)

// node is the flattened completion view of one Command, keyed by its full token
// path from the root (e.g. ["space", "exec"]). It carries only what a shell
// completer needs: tokens, descriptions, flags, sub-verbs and dynamic subjects.
type node struct {
	path     []string  // token path from root, excluding the program name
	short    string    // one-line description
	subs     []subverb // visible child verbs, sorted by name
	flags    []flagDoc // flags accepted here, sorted by name
	complete []string  // __complete subjects for positional slots, in order
}

// subverb is one child verb reference (name + description) for a parent's list.
type subverb struct {
	name  string
	short string
}

// flagDoc is one flag's completion view: dashed name + description + enum values.
type flagDoc struct {
	name   string   // dashed (e.g. "-repo")
	usage  string   // help text
	values []string // enum candidates, when the flag is a closed set
	isBool bool
}

// walk flattens the visible Command tree into nodes, depth-first, skipping
// Hidden commands. The root node (empty path) carries the top-level verbs.
func walk(root Command) []node {
	var nodes []node
	var rec func(c Command, path []string)
	rec = func(c Command, path []string) {
		n := node{path: path, short: c.Short}
		for _, f := range c.Flags {
			n.flags = append(n.flags, flagDocOf(f))
		}
		sort.Slice(n.flags, func(i, j int) bool { return n.flags[i].name < n.flags[j].name })
		for _, p := range c.Positionals {
			if p.Complete != "" {
				n.complete = append(n.complete, p.Complete)
			}
		}
		for _, sub := range c.Sub {
			if sub.Hidden {
				continue
			}
			n.subs = append(n.subs, subverb{name: sub.Name, short: sub.Short})
		}
		sort.Slice(n.subs, func(i, j int) bool { return n.subs[i].name < n.subs[j].name })
		nodes = append(nodes, n)
		for _, sub := range c.Sub {
			if sub.Hidden {
				continue
			}
			rec(sub, append(append([]string{}, path...), sub.Name))
		}
	}
	for _, sub := range root.Sub {
		if sub.Hidden {
			continue
		}
		rec(sub, []string{sub.Name})
	}
	// Synthesize the root node holding the top-level verb list.
	rootNode := node{path: nil, short: root.Short}
	for _, sub := range root.Sub {
		if sub.Hidden {
			continue
		}
		rootNode.subs = append(rootNode.subs, subverb{name: sub.Name, short: sub.Short})
	}
	sort.Slice(rootNode.subs, func(i, j int) bool { return rootNode.subs[i].name < rootNode.subs[j].name })
	return append([]node{rootNode}, nodes...)
}

// flagDocOf renders one Flag into its completion view. Bool flags take no value;
// enum candidates are mined from the usage text when it spells a closed set.
func flagDocOf(f Flag) flagDoc {
	return flagDoc{
		name:   "-" + f.Name,
		usage:  oneLine(f.Usage),
		values: enumValues(f),
		isBool: f.Type == BoolFlag,
	}
}

// oneLine collapses a usage string to a single line and trims it, so multi-line
// help never breaks the generated shell quoting.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// enumValues returns the closed candidate set for a flag, or nil. We key off the
// flag name (the few enum flags in the tree) rather than parsing prose, keeping
// the candidate lists explicit and stable.
func enumValues(f Flag) []string {
	switch f.Name {
	case "output":
		return []string{"text", "json"}
	case "provider":
		return []string{"ovh", "scaleway", "mistral", "deepseek", "anthropic", "openai", "openai-compat"}
	case "agent":
		return []string{"claude", "cursor", "codex", "vibe", "zed", "cline", "none"}
	case "status":
		return []string{"backlog", "in_progress", "done", "wontfix"}
	case "type":
		return []string{"bug", "feature", "task", "other"}
	}
	return nil
}

// pathKey joins a token path with spaces for use as a shell case label.
func pathKey(path []string) string { return strings.Join(path, " ") }

// GenerateBash emits a bash completion script for the tree rooted at root,
// registered for the program prog (typically "sklp").
func GenerateBash(root Command, prog string) string {
	nodes := walk(root)
	var b strings.Builder
	fmt.Fprintf(&b, "# bash completion for %s — GENERATED from the command tree.\n", prog)
	fmt.Fprintf(&b, "# Do not edit: run `%s completion bash` to regenerate.\n", prog)
	fmt.Fprintf(&b, "_%s() {\n", prog)
	b.WriteString("    local cur prev words cword\n")
	b.WriteString("    _get_comp_words_by_ref -n : cur prev words cword 2>/dev/null || {\n")
	b.WriteString("        cur=\"${COMP_WORDS[COMP_CWORD]}\"; words=(\"${COMP_WORDS[@]}\"); cword=$COMP_CWORD\n")
	b.WriteString("    }\n")
	// Build the verb path: every word after prog that is not a flag, until cword.
	b.WriteString("    local path=\"\" w\n")
	b.WriteString("    local i=1\n")
	b.WriteString("    while [ $i -lt $cword ]; do\n")
	b.WriteString("        w=\"${words[$i]}\"\n")
	b.WriteString("        case \"$w\" in -*) ;; *) path=\"${path:+$path }$w\" ;; esac\n")
	b.WriteString("        i=$((i+1))\n")
	b.WriteString("    done\n")
	b.WriteString("    local opts=\"\" subs=\"\" dyn=\"\"\n")
	b.WriteString("    case \"$path\" in\n")
	for _, n := range nodes {
		fmt.Fprintf(&b, "        %q)\n", pathKey(n.path))
		if subs := subNames(n.subs); subs != "" {
			fmt.Fprintf(&b, "            subs=%q\n", subs)
		}
		if opts := flagNames(n.flags); opts != "" {
			fmt.Fprintf(&b, "            opts=%q\n", opts)
		}
		for _, subj := range n.complete {
			fmt.Fprintf(&b, "            dyn=%q\n", subj)
		}
		b.WriteString("            ;;\n")
	}
	b.WriteString("    esac\n")
	// Flag value enums: complete after an enum flag.
	b.WriteString("    case \"$prev\" in\n")
	for _, e := range bashEnumCases(nodes) {
		b.WriteString(e)
	}
	b.WriteString("    esac\n")
	b.WriteString("    if [[ \"$cur\" == -* ]]; then\n")
	b.WriteString("        COMPREPLY=( $(compgen -W \"$opts\" -- \"$cur\") ); return 0\n")
	b.WriteString("    fi\n")
	b.WriteString("    local cand=\"$subs\"\n")
	fmt.Fprintf(&b, "    if [ -n \"$dyn\" ]; then cand=\"$cand $(%s __complete \"$dyn\" 2>/dev/null)\"; fi\n", prog)
	b.WriteString("    COMPREPLY=( $(compgen -W \"$cand\" -- \"$cur\") )\n")
	b.WriteString("    return 0\n")
	b.WriteString("}\n")
	fmt.Fprintf(&b, "complete -F _%s %s\n", prog, prog)
	return b.String()
}

// bashEnumCases emits `prev` cases that complete a flag's enum values. The flags
// are gathered tree-wide and de-duplicated by name (same enum everywhere).
func bashEnumCases(nodes []node) []string {
	seen := map[string]string{}
	for _, n := range nodes {
		for _, f := range n.flags {
			if len(f.values) > 0 {
				seen[f.name] = strings.Join(f.values, " ")
			}
		}
	}
	names := make([]string, 0, len(seen))
	for k := range seen {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, fmt.Sprintf("        %s) COMPREPLY=( $(compgen -W %q -- \"$cur\") ); return 0 ;;\n", name, seen[name]))
	}
	return out
}

// GenerateZsh emits a zsh completion script (compdef-style) for the tree.
func GenerateZsh(root Command, prog string) string {
	nodes := walk(root)
	var b strings.Builder
	fmt.Fprintf(&b, "#compdef %s\n", prog)
	fmt.Fprintf(&b, "# zsh completion for %s — GENERATED from the command tree.\n", prog)
	fmt.Fprintf(&b, "# Do not edit: run `%s completion zsh` to regenerate.\n", prog)
	fmt.Fprintf(&b, "_%s() {\n", prog)
	b.WriteString("    local curcontext=\"$curcontext\" state line\n")
	b.WriteString("    local -a path\n")
	b.WriteString("    local w\n")
	b.WriteString("    for w in ${words[2,CURRENT-1]}; do\n")
	b.WriteString("        case $w in -*) ;; *) path+=$w ;; esac\n")
	b.WriteString("    done\n")
	b.WriteString("    local key=\"${(j: :)path}\"\n")
	b.WriteString("    local -a subs opts dyn\n")
	b.WriteString("    case \"$key\" in\n")
	for _, n := range nodes {
		fmt.Fprintf(&b, "        %s)\n", zshCaseLabel(pathKey(n.path)))
		if descs := zshSubDescs(n.subs); len(descs) > 0 {
			fmt.Fprintf(&b, "            subs=(%s)\n", strings.Join(descs, " "))
		}
		if descs := zshFlagDescs(n.flags); len(descs) > 0 {
			fmt.Fprintf(&b, "            opts=(%s)\n", strings.Join(descs, " "))
		}
		for _, subj := range n.complete {
			fmt.Fprintf(&b, "            dyn=(${(f)\"$(%s __complete %s 2>/dev/null)\"})\n", prog, subj)
		}
		b.WriteString("            ;;\n")
	}
	b.WriteString("    esac\n")
	b.WriteString("    if [[ \"$words[CURRENT]\" == -* ]]; then\n")
	b.WriteString("        _describe -t opts 'option' opts && return\n")
	b.WriteString("    fi\n")
	b.WriteString("    _describe -t subs 'command' subs\n")
	b.WriteString("    (( ${#dyn} )) && _describe -t dyn 'value' dyn\n")
	b.WriteString("}\n")
	fmt.Fprintf(&b, "compdef _%s %s\n", prog, prog)
	return b.String()
}

// zshCaseLabel quotes a case label for zsh (empty label → the root case).
func zshCaseLabel(s string) string {
	if s == "" {
		return `""`
	}
	return fmt.Sprintf("%q", s)
}

// zshSubDescs renders sub-verbs as zsh `name:description` describe entries.
func zshSubDescs(subs []subverb) []string {
	out := make([]string, 0, len(subs))
	for _, s := range subs {
		out = append(out, fmt.Sprintf("%q", s.name+":"+zshDesc(s.short)))
	}
	return out
}

// zshFlagDescs renders flags as zsh `-flag:description` describe entries.
func zshFlagDescs(flags []flagDoc) []string {
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		out = append(out, fmt.Sprintf("%q", f.name+":"+zshDesc(f.usage)))
	}
	return out
}

// zshDesc sanitizes a description for a zsh double-quoted `_describe` entry: zsh
// expands `$` and backticks inside double quotes and treats `:` as the value
// separator, so neutralise them (one line, no `$`/backtick, `:`→`-`).
func zshDesc(s string) string {
	s = oneLine(s)
	s = strings.ReplaceAll(s, "`", "'")
	s = strings.ReplaceAll(s, "$", "")
	s = strings.ReplaceAll(s, ":", " -")
	return s
}

// GenerateFish emits a fish completion script for the tree.
func GenerateFish(root Command, prog string) string {
	nodes := walk(root)
	var b strings.Builder
	fmt.Fprintf(&b, "# fish completion for %s — GENERATED from the command tree.\n", prog)
	fmt.Fprintf(&b, "# Do not edit: run `%s completion fish` to regenerate.\n", prog)
	// A helper that returns the verb path (non-flag tokens after prog).
	fmt.Fprintf(&b, "function __%s_path\n", prog)
	b.WriteString("    set -l toks (commandline -opc)\n")
	b.WriteString("    set -l p\n")
	b.WriteString("    for t in $toks[2..-1]\n")
	b.WriteString("        switch $t\n")
	b.WriteString("            case '-*'\n")
	b.WriteString("            case '*'; set -a p $t\n")
	b.WriteString("        end\n")
	b.WriteString("    end\n")
	b.WriteString("    string join ' ' $p\n")
	b.WriteString("end\n")
	for _, n := range nodes {
		cond := fmt.Sprintf("test (__%s_path) = %q", prog, pathKey(n.path))
		for _, s := range n.subs {
			fmt.Fprintf(&b, "complete -c %s -n %q -f -a %q -d %q\n", prog, cond, s.name, fishDesc(s.short))
		}
		for _, f := range n.flags {
			line := fmt.Sprintf("complete -c %s -n %q -l %q -d %q", prog, cond, strings.TrimPrefix(f.name, "-"), fishDesc(f.usage))
			if len(f.values) > 0 {
				line += fmt.Sprintf(" -x -a %q", strings.Join(f.values, " "))
			}
			b.WriteString(line + "\n")
		}
		for _, subj := range n.complete {
			fmt.Fprintf(&b, "complete -c %s -n %q -f -a \"(%s __complete %s 2>/dev/null)\"\n", prog, cond, prog, subj)
		}
	}
	return b.String()
}

// fishDesc sanitizes a description for a fish double-quoted `-d` string: fish
// expands backticks and `$` inside double quotes, so neutralise them (and
// collapse to one line) to keep the generated script literal and safe.
func fishDesc(s string) string {
	s = oneLine(s)
	s = strings.ReplaceAll(s, "`", "'")
	s = strings.ReplaceAll(s, "$", "")
	return s
}

// subNames joins sub-verb names with spaces (bash candidate list).
func subNames(subs []subverb) string {
	names := make([]string, 0, len(subs))
	for _, s := range subs {
		names = append(names, s.name)
	}
	return strings.Join(names, " ")
}

// flagNames joins dashed flag names with spaces (bash option list).
func flagNames(flags []flagDoc) string {
	names := make([]string, 0, len(flags))
	for _, f := range flags {
		names = append(names, f.name)
	}
	return strings.Join(names, " ")
}
