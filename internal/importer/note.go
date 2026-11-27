package importer

import "fmt"

type Severity int

const (
	Info Severity = iota
	Manual
)

// Note is one line of the import report. A Manual note is an action the
// developer must take before the imported contract behaves like the source;
// an Info note records a translation choice worth a glance.
type Note struct {
	Severity Severity
	Service  string
	Field    string
	Message  string
}

func (n Note) String() string {
	tag := "info"
	if n.Severity == Manual {
		tag = "TODO"
	}
	switch {
	case n.Service != "" && n.Field != "":
		return fmt.Sprintf("%s %s.%s: %s", tag, n.Service, n.Field, n.Message)
	case n.Service != "":
		return fmt.Sprintf("%s %s: %s", tag, n.Service, n.Message)
	case n.Field != "":
		return fmt.Sprintf("%s %s: %s", tag, n.Field, n.Message)
	}
	return tag + " " + n.Message
}

func (n Note) comment() string {
	prefix := "import: "
	if n.Severity == Manual {
		prefix = "TODO(import): "
	}
	if n.Field != "" {
		return prefix + n.Field + ": " + n.Message
	}
	return prefix + n.Message
}

func ManualCount(notes []Note) int {
	n := 0
	for _, note := range notes {
		if note.Severity == Manual {
			n++
		}
	}
	return n
}

type noter interface {
	note(sev Severity, field, format string, args ...any)
}
