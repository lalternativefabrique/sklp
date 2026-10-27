// cli_usage.go renders help text from the declared Command tree, so `-h` output
// stays in sync with the declarations instead of a hand-maintained string.
package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
)

// printGroupUsage lists a group's sub-commands (hidden ones excluded).
func (a *flagAdapter) printGroupUsage(cmd *Command, path string) {
	w := os.Stderr
	if cmd.Long != "" {
		fmt.Fprintln(w, cmd.Long)
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "Usage: %s <subcommand> [flags]\n\n", path)
	fmt.Fprintln(w, "Subcommands:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for i := range cmd.Sub {
		c := &cmd.Sub[i]
		if c.Hidden {
			continue
		}
		name := c.Name
		if len(c.Aliases) > 0 {
			name += " (" + strings.Join(c.Aliases, ", ") + ")"
		}
		fmt.Fprintf(tw, "  %s\t%s\n", name, c.Short)
	}
	_ = tw.Flush()
}

// printLeafUsage prints a leaf command's flags + positionals.
func (a *flagAdapter) printLeafUsage(cmd *Command, path string) {
	w := os.Stderr
	if cmd.Long != "" {
		fmt.Fprintln(w, cmd.Long)
		fmt.Fprintln(w)
	}
	var b strings.Builder
	b.WriteString("Usage: ")
	b.WriteString(path)
	if len(cmd.Flags) > 0 {
		b.WriteString(" [flags]")
	}
	for _, p := range cmd.Positionals {
		switch {
		case p.Variadic:
			b.WriteString(" [" + p.Name + "...]")
		case p.Required:
			b.WriteString(" <" + p.Name + ">")
		default:
			b.WriteString(" [" + p.Name + "]")
		}
	}
	fmt.Fprintln(w, b.String())
	if len(cmd.Flags) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Flags:")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for i := range cmd.Flags {
			f := &cmd.Flags[i]
			name := "-" + f.Name
			if len(f.Aliases) > 0 {
				alias := make([]string, len(f.Aliases))
				for j, al := range f.Aliases {
					alias[j] = "-" + al
				}
				name += " (" + strings.Join(alias, ", ") + ")"
			}
			fmt.Fprintf(tw, "  %s\t%s\n", name, f.Usage)
		}
		_ = tw.Flush()
	}
}
