package drill

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

func (r Report) Summary() string {
	if r.Status == StatusOK {
		return fmt.Sprintf("%s %s restored %s in %s: %d collection(s), %s rows/messages, %s, data up to %s",
			r.Engine, r.Target, r.Backup.Label, r.RTO, len(r.Inventory.Collections),
			groupDigits(r.totalCount()), humanBytes(r.Inventory.Bytes), r.Inventory.RecoveryPoint.Format(time.RFC3339))
	}
	var reasons []string
	for _, c := range r.Failures() {
		reasons = append(reasons, c.Name+": "+c.Detail)
	}
	return fmt.Sprintf("%s %s restore FAILED — %s", r.Engine, r.Target, strings.Join(reasons, "; "))
}

func (r Report) totalCount() int64 {
	var n int64
	for _, c := range r.Inventory.Collections {
		n += c.Count
	}
	return n
}

func (r Report) Render(w io.Writer) {
	fmt.Fprintf(w, "restore drill · %s · %s · %s\n", r.Space, r.Engine, r.Target)
	fmt.Fprintf(w, "status    %s\n", strings.ToUpper(string(r.Status)))
	fmt.Fprintf(w, "backup    %s (%s) taken %s, %s\n", r.Backup.Label, r.Backup.Type, r.Backup.TakenAt.Format(time.RFC3339), humanBytes(r.Backup.Bytes))
	fmt.Fprintf(w, "rto       %s   rpo %s   total %s\n", r.RTO, r.RPO, r.Duration)
	if r.Inventory.Position != "" {
		fmt.Fprintf(w, "position  %s\n", r.Inventory.Position)
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "phase\tduration\terror")
	for _, p := range r.Phases {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, p.Duration, p.Error)
	}
	tw.Flush()

	cols := append([]Collection(nil), r.Inventory.Collections...)
	sort.Slice(cols, func(i, j int) bool {
		if cols[i].Group != cols[j].Group {
			return cols[i].Group < cols[j].Group
		}
		return cols[i].Name < cols[j].Name
	})
	if len(cols) > 0 {
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "group\tcollection\tcount\tsize\tnewest")
		for _, c := range cols {
			newest := ""
			if c.Newest != nil {
				newest = c.Newest.Format(time.RFC3339)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.Group, c.Name, groupDigits(c.Count), humanBytes(c.Bytes), newest)
		}
		fmt.Fprintf(tw, "total\t%d\t%s\t%s\t\n", len(cols), groupDigits(r.totalCount()), humanBytes(r.Inventory.Bytes))
		tw.Flush()
	}

	for _, c := range r.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "[%s] %s — %s\n", mark, c.Name, c.Detail)
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func groupDigits(n int64) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		return s
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + " " + s[i:]
	}
	return s
}
