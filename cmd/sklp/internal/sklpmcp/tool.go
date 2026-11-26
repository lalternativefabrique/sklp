package sklpmcp

import (
	"context"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type kind int

const (
	read kind = iota
	write
	destructive
)

type toolDef struct {
	kind kind
	add  func(*mcp.Server, []string)
}

func define[In, Out any](t *mcp.Tool, k kind, fn func(context.Context, In, *redactor) (Out, error)) toolDef {
	if t.Annotations == nil {
		t.Annotations = &mcp.ToolAnnotations{}
	}
	switch k {
	case read:
		t.Annotations.ReadOnlyHint = true
	case write:
		no := false
		t.Annotations.DestructiveHint = &no
	case destructive:
		yes := true
		t.Annotations.DestructiveHint = &yes
	}
	return toolDef{kind: k, add: func(s *mcp.Server, secrets []string) {
		mcp.AddTool(s, t, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
			var zero Out
			r := &redactor{}
			r.add(secrets...)
			out, err := fn(ctx, in, r)
			if err != nil {
				return nil, zero, r.err(err)
			}
			if out, err = scrub(out, r); err != nil {
				return nil, zero, r.err(err)
			}
			return nil, out, nil
		})
	}}
}

func head[T any](s []T, n int) []T {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func tail[T any](s []T, n int) []T {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

type lineRing struct {
	mu      sync.Mutex
	max     int
	lines   []string
	partial strings.Builder
	dropped bool
}

func (l *lineRing) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			l.push()
			continue
		}
		if l.partial.Len() < maxLineBytes {
			l.partial.WriteByte(b)
		}
	}
	return len(p), nil
}

func (l *lineRing) push() {
	l.lines = append(l.lines, l.partial.String())
	l.partial.Reset()
	if len(l.lines) > l.max {
		l.lines = l.lines[1:]
		l.dropped = true
	}
}

func nonNil(lines []string) []string {
	if lines == nil {
		return []string{}
	}
	return lines
}

func (l *lineRing) snapshot() (lines []string, dropped bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	lines = append([]string(nil), l.lines...)
	if l.partial.Len() > 0 {
		lines = append(lines, l.partial.String())
	}
	return lines, l.dropped
}
