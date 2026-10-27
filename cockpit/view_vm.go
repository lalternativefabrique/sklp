package cockpit

import (
	"fmt"
	"strings"
	"time"
)

// renderVM paints the machine the cockpit itself runs on. On macOS that is a
// VM the user never sees otherwise, so this is the only place they learn what
// it is made of and what is running there. The summary line always shows; the
// process list is behind [V] because it ranks the WHOLE machine by memory,
// which on a workstation is the browser, not anything the cockpit manages.
// Returns "" when nothing could be read, so a host that reports nothing shows
// no empty box.
func renderVM(m *Model, r Renderer, width int) string {
	v := m.VM
	if !v.Known() {
		return ""
	}

	var b strings.Builder
	b.WriteString(r.Text(vmSummary(v), StyleMuted))

	if n := len(v.Processes); n > 0 {
		b.WriteString("\n")
		if !m.VMProcsOpen {
			b.WriteString(note(r, itoa(n)+" processes — [V] show", StyleMuted, width-boxChrome))
		} else {
			rows := make([]Row, 0, n)
			for _, p := range v.Processes {
				rows = append(rows, Row{Cells: []string{
					fmt.Sprint(p.PID),
					p.Name,
					humanBytes(p.RSS),
				}})
			}
			cols := []Column{
				{Header: "PID", Width: 7},
				{Header: "PROCESS", Flex: true},
				{Header: "RSS", Width: 10},
			}
			b.WriteString(r.Table(cols, rows, width-boxChrome))
			b.WriteString("\n")
			b.WriteString(note(r, "[V] hide", StyleMuted, width-boxChrome))
		}
	}
	return r.Box(vmTitle(v), b.String(), width)
}

func vmTitle(v VMState) string {
	if v.Backend != "" {
		return "vm · " + v.Backend
	}
	return "vm"
}

// vmSummary is the one-line description of the box: what it is, how loaded it
// is, and how to reach it.
func vmSummary(v VMState) string {
	var parts []string
	if v.Kernel != "" {
		parts = append(parts, "linux "+v.Kernel)
	}
	if v.CPUs > 0 {
		parts = append(parts, fmt.Sprintf("%d cpu · load %.2f", v.CPUs, v.LoadAvg[0]))
	}
	if used := v.MemUsed(); used > 0 {
		parts = append(parts, fmt.Sprintf("mem %s / %s", humanBytes(used), humanBytes(v.MemTotal)))
	}
	if v.Uptime > 0 {
		parts = append(parts, "up "+humanDuration(v.Uptime))
	}
	if len(v.Addrs) > 0 {
		parts = append(parts, strings.Join(v.Addrs, "  "))
	}
	return strings.Join(parts, "   ")
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGT"[exp])
}

// humanDuration renders an uptime the way `uptime` does: the largest two
// units, because "3d 4h" answers the question and "3d4h12m9s" does not.
func humanDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}
