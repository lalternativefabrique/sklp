package spacemanager

import (
	"fmt"
	"strings"
)

// detailText returns the event detail, or a short fallback when empty.
func detailText(e StateEvent) string {
	if e.Detail == "" {
		return e.Service
	}
	return e.Detail
}

// detailSuffix renders " (<detail>)" for a sub-line, or "" when no detail.
func detailSuffix(e StateEvent) string {
	if e.Detail == "" {
		return ""
	}
	return " (" + e.Detail + ")"
}

// pluralReady formats a Ready count: "1 service ready" / "3 services ready".
func pluralReady(n int) string {
	if n == 1 {
		return "1 service ready"
	}
	return fmt.Sprintf("%d services ready", n)
}

// joinStrings joins names with ", " for the summary line.
func joinStrings(s []string) string {
	return strings.Join(s, ", ")
}

// joinLines joins log lines with newlines for a FAILURES message body.
func joinLines(s []string) string {
	return strings.Join(s, "\n")
}
