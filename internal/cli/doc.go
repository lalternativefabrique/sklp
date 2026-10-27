// doc.go emits the Command tree as JSON — the single source of truth for the
// sklp-web documentation site. Like the completion generator, it walks
// rootCommand() so the docs can never drift from the declared verbs/flags.
//
// The shape here IS the doc-site contract (apps/sklp-web consumes it). Keep it
// stable; additive fields only.
package cli

import (
	"encoding/json"
	"io"
	"sort"
)

// DocFlag is one flag in the JSON doc contract.
type DocFlag struct {
	Name        string   `json:"name"`
	Aliases     []string `json:"aliases,omitempty"`
	Usage       string   `json:"usage"`
	Type        string   `json:"type"` // string | bool | int | stringSlice
	Default     string   `json:"default,omitempty"`
	EnvFallback string   `json:"envFallback,omitempty"`
}

// DocPositional is one positional argument in the JSON doc contract.
type DocPositional struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Variadic bool   `json:"variadic,omitempty"`
	Complete string `json:"complete,omitempty"` // __complete subject for dynamic values
}

// DocCommand is one node of the documented command tree.
type DocCommand struct {
	Path        []string        `json:"path"` // e.g. ["sklp","space","up"]
	Slug        string          `json:"slug"` // e.g. "space-up" ("" for root)
	Name        string          `json:"name"` // leaf token
	Aliases     []string        `json:"aliases,omitempty"`
	Short       string          `json:"short"`
	Long        string          `json:"long,omitempty"`
	Synopsis    string          `json:"synopsis"` // derived: "sklp space up [flags] <name>"
	Flags       []DocFlag       `json:"flags,omitempty"`
	Positionals []DocPositional `json:"positionals,omitempty"`
	Passthrough bool            `json:"passthrough,omitempty"`
	RawArgs     bool            `json:"rawArgs,omitempty"`
	Subcommands []DocCommand    `json:"subcommands,omitempty"`
}

// flagTypeName maps the enum to the stable JSON token the doc site expects.
func flagTypeName(t FlagType) string {
	switch t {
	case BoolFlag:
		return "bool"
	case IntFlag:
		return "int"
	case StringSliceFlag:
		return "stringSlice"
	default:
		return "string"
	}
}

// defaultString renders the type-appropriate default as a display string.
func defaultString(f Flag) string {
	switch f.Type {
	case BoolFlag:
		if f.BoolDefault {
			return "true"
		}
		return ""
	case IntFlag:
		if f.IntDefault != 0 {
			return itoa(f.IntDefault)
		}
		return ""
	default:
		return f.StringDefault
	}
}

// itoa avoids pulling strconv into this small surface.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// synopsis builds a one-line usage string from the command's own surface.
// RawArgs commands soak their tail verbatim, so we append "[args…]".
func synopsis(path []string, c Command) string {
	s := joinSpace(path)
	if len(c.Flags) > 0 {
		s += " [flags]"
	}
	for _, p := range c.Positionals {
		tok := "<" + p.Name + ">"
		if !p.Required {
			tok = "[" + p.Name + "]"
		}
		if p.Variadic {
			tok += "…"
		}
		s += " " + tok
	}
	if c.RawArgs && len(c.Positionals) == 0 {
		s += " [args…]"
	}
	if c.Passthrough {
		s += " [-- cmd…]"
	}
	return s
}

func joinSpace(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}

// slugify flattens a command path (minus the root program name) into a URL
// slug: ["sklp","space","up"] → "space-up". The root itself is "".
func slugify(path []string) string {
	if len(path) <= 1 {
		return ""
	}
	out := ""
	for i, p := range path[1:] {
		if i > 0 {
			out += "-"
		}
		out += p
	}
	return out
}

// toDoc converts one Command (and its visible children) into a DocCommand.
// Hidden commands are skipped entirely.
func toDoc(parent []string, c Command) DocCommand {
	path := append(append([]string{}, parent...), c.Name)

	d := DocCommand{
		Path:        path,
		Slug:        slugify(path),
		Name:        c.Name,
		Aliases:     c.Aliases,
		Short:       c.Short,
		Long:        c.Long,
		Synopsis:    synopsis(path, c),
		Passthrough: c.Passthrough,
		RawArgs:     c.RawArgs,
	}
	for _, f := range c.Flags {
		d.Flags = append(d.Flags, DocFlag{
			Name:        f.Name,
			Aliases:     f.Aliases,
			Usage:       f.Usage,
			Type:        flagTypeName(f.Type),
			Default:     defaultString(f),
			EnvFallback: f.EnvFallback,
		})
	}
	for _, p := range c.Positionals {
		d.Positionals = append(d.Positionals, DocPositional{
			Name:     p.Name,
			Required: p.Required,
			Variadic: p.Variadic,
			Complete: p.Complete,
		})
	}
	for _, sub := range c.Sub {
		if sub.Hidden {
			continue
		}
		d.Subcommands = append(d.Subcommands, toDoc(path, sub))
	}
	sort.SliceStable(d.Subcommands, func(i, j int) bool {
		return d.Subcommands[i].Name < d.Subcommands[j].Name
	})
	return d
}

// BuildDoc walks a root Command into the DocCommand tree. Hidden subcommands
// (and a hidden root, which never happens) are pruned.
func BuildDoc(root Command) DocCommand {
	return toDoc(nil, root)
}

// WriteDoc emits the doc tree of root as indented JSON to w.
func WriteDoc(w io.Writer, root Command) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(BuildDoc(root))
}
