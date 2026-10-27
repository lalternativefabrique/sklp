package cockpit

import (
	"fmt"
	"strings"
)

// plainRenderer is a TUI-library-free Renderer: it draws the SAME view.go
// composition as plain ASCII for the non-TTY one-shot snapshot (piped/CI). Its
// existence proves the Renderer port is real — view.go compiles and renders
// against a second backend with zero changes.
type plainRenderer struct{}

// NewPlainRenderer returns the plain-text Renderer used for the snapshot path.
func NewPlainRenderer() Renderer { return plainRenderer{} }

func (plainRenderer) Box(title, body string, _ int) string {
	return title + ":\n" + indent(body, "  ")
}

// Table sizes columns to their content rather than the caller's widths: the
// snapshot goes to a pipe or a file, where nothing needs to fit a terminal and
// truncating would lose data. Selection is marked with a leading '>' since the
// plain backend has no background colors.
func (plainRenderer) Table(cols []Column, rows []Row, _ int) string {
	widths := make([]int, len(cols))
	headers := make([]string, len(cols))
	for i, c := range cols {
		headers[i] = c.Header
		widths[i] = len(c.Header)
	}
	for _, row := range rows {
		for i, c := range row.Cells {
			if i < len(widths) && len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	var b strings.Builder
	b.WriteString("  ")
	writeRow(&b, headers, widths)
	for _, row := range rows {
		if row.Selected {
			b.WriteString("> ")
		} else {
			b.WriteString("  ")
		}
		writeRow(&b, row.Cells, widths)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (plainRenderer) Header(fields [][2]string, _ [][2]string, _ int) string {
	var b strings.Builder
	for _, f := range fields {
		fmt.Fprintf(&b, "%s %s\n", f[0], f[1])
	}
	return strings.TrimRight(b.String(), "\n")
}

func (plainRenderer) Crumb(path, right string, _ int) string {
	if right == "" {
		return path
	}
	return path + "  " + right
}

func writeRow(b *strings.Builder, cells []string, widths []int) {
	for i, c := range cells {
		w := 0
		if i < len(widths) {
			w = widths[i]
		}
		fmt.Fprintf(b, "%-*s  ", w, c)
	}
	b.WriteString("\n")
}

func (plainRenderer) StatusGlyph(state State) string {
	switch state {
	case StateReady:
		return "[ok]"
	case StateBusy:
		return "[..]"
	case StateIdle:
		return "[--]"
	case StateError:
		return "[!!]"
	default:
		return "[??]"
	}
}

// Text ignores styling in the plain backend (the style is an enum, not ANSI).
func (plainRenderer) Text(s string, _ Style) string { return s }

func (plainRenderer) HRule(width int) string { return strings.Repeat("-", width) }

// SideBySide joins two blocks horizontally: left lines padded to leftWidth, a
// " | " gutter, then the right lines. Deterministic, ANSI-free.
func (plainRenderer) SideBySide(left, right string, leftWidth int) string {
	ls := strings.Split(left, "\n")
	rs := strings.Split(right, "\n")
	n := len(ls)
	if len(rs) > n {
		n = len(rs)
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		l := ""
		if i < len(ls) {
			l = ls[i]
		}
		r := ""
		if i < len(rs) {
			r = rs[i]
		}
		fmt.Fprintf(&b, "%-*s │ %s\n", leftWidth, l, r)
	}
	return strings.TrimRight(b.String(), "\n")
}

func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}
