package plan

import (
	"fmt"
	"sort"
)

// DAGNode is one node in the step graph. Edges are the names of nodes
// the current node depends on (i.e. predecessors that must run first).
type DAGNode struct {
	Name      string
	DependsOn []string
}

// CycleError is returned by TopoSort when the input graph contains a
// cycle. It names both endpoints of one offending edge so the user can
// locate the conflict in the YAML without parsing the message.
type CycleError struct {
	From string
	To   string
	Path []string
}

func (e *CycleError) Error() string {
	if len(e.Path) > 0 {
		return fmt.Sprintf("cycle detected: %s -> %s (path: %v)", e.From, e.To, e.Path)
	}
	return fmt.Sprintf("cycle detected: %s depends on %s and vice versa", e.From, e.To)
}

// MissingDepError is returned when a node's DependsOn references a name
// that is not part of the input set.
type MissingDepError struct {
	From    string
	Missing string
}

func (e *MissingDepError) Error() string {
	return fmt.Sprintf("step %q depends on unknown step %q", e.From, e.Missing)
}

// TopoSort returns the nodes in a deterministic topological order.
//
// Determinism: among nodes ready to be emitted at any given step (i.e.
// nodes whose remaining in-degree is zero), the lexicographically
// smallest name wins. Two runs over the same input therefore produce
// the byte-identical order.
//
// Errors:
//   - *MissingDepError when a DependsOn entry is not a node name.
//   - *CycleError when the graph contains a cycle.
func TopoSort(nodes []DAGNode) ([]string, error) {
	known := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		known[n.Name] = struct{}{}
	}

	// adjacency: dep -> dependents. inDegree counts unmet predecessors.
	adj := make(map[string][]string, len(nodes))
	inDegree := make(map[string]int, len(nodes))
	for _, n := range nodes {
		if _, ok := inDegree[n.Name]; !ok {
			inDegree[n.Name] = 0
		}
		for _, d := range n.DependsOn {
			if _, ok := known[d]; !ok {
				return nil, &MissingDepError{From: n.Name, Missing: d}
			}
			if d == n.Name {
				return nil, &CycleError{From: n.Name, To: n.Name}
			}
			adj[d] = append(adj[d], n.Name)
			inDegree[n.Name]++
		}
	}

	// Sort dependent lists so the order we discover them is stable.
	for k := range adj {
		sort.Strings(adj[k])
	}

	// Initial ready set: nodes with no predecessors, lex-sorted.
	ready := make([]string, 0)
	for name, deg := range inDegree {
		if deg == 0 {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)

	out := make([]string, 0, len(nodes))
	for len(ready) > 0 {
		// Pop the lex-smallest ready node.
		name := ready[0]
		ready = ready[1:]
		out = append(out, name)

		for _, dep := range adj[name] {
			inDegree[dep]--
			if inDegree[dep] == 0 {
				// Insert in sorted position to keep determinism.
				idx := sort.SearchStrings(ready, dep)
				ready = append(ready, "")
				copy(ready[idx+1:], ready[idx:])
				ready[idx] = dep
			}
		}
	}

	if len(out) != len(inDegree) {
		// Remaining nodes form one or more cycles. Find a witness edge.
		from, to, path := findCycle(nodes, known)
		return nil, &CycleError{From: from, To: to, Path: path}
	}

	return out, nil
}

// findCycle locates one edge participating in a cycle. The result is a
// best-effort witness used to build a human-readable CycleError.
func findCycle(nodes []DAGNode, known map[string]struct{}) (string, string, []string) {
	deps := make(map[string][]string, len(nodes))
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		filtered := make([]string, 0, len(n.DependsOn))
		for _, d := range n.DependsOn {
			if _, ok := known[d]; ok {
				filtered = append(filtered, d)
			}
		}
		sort.Strings(filtered)
		deps[n.Name] = filtered
		names = append(names, n.Name)
	}
	sort.Strings(names)

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(nodes))
	stack := make([]string, 0)

	var (
		foundFrom, foundTo string
		foundPath          []string
		found              bool
	)
	var dfs func(string)
	dfs = func(u string) {
		if found {
			return
		}
		color[u] = gray
		stack = append(stack, u)
		for _, v := range deps[u] {
			if found {
				return
			}
			switch color[v] {
			case gray:
				foundFrom = u
				foundTo = v
				foundPath = append([]string(nil), stack...)
				foundPath = append(foundPath, v)
				found = true
				return
			case white:
				dfs(v)
			}
		}
		color[u] = black
		stack = stack[:len(stack)-1]
	}

	for _, n := range names {
		if found {
			break
		}
		if color[n] == white {
			dfs(n)
		}
	}
	return foundFrom, foundTo, foundPath
}
