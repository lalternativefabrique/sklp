//go:build !linux

package host

type Problem struct {
	Name   string
	Detail string
	Fix    string
	Fatal  bool
}

func (p Problem) String() string { return p.Name + ": " + p.Detail }

func Check(string) []Problem { return nil }

func HasFatal([]Problem) bool { return false }
