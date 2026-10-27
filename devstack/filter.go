package devstack

import "fmt"

// filterServices restricts stack.Services to the named services plus every
// service they transitively depend on (depends_on closure). The existing topo
// order is preserved. Unknown names in only are an error.
func filterServices(stack *Stack, only []string) error {
	// index name → NamedService for quick dep lookup
	byName := make(map[string]NamedService, len(stack.Services))
	for _, svc := range stack.Services {
		byName[svc.Name] = svc
	}

	for _, name := range only {
		if _, ok := byName[name]; !ok {
			return fmt.Errorf("unknown service %q", name)
		}
	}

	// BFS: collect the closure of requested services + their deps
	keep := make(map[string]bool)
	queue := append([]string{}, only...)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if keep[name] {
			continue
		}
		keep[name] = true
		for _, dep := range byName[name].Def.DependsOn {
			if !keep[dep] {
				queue = append(queue, dep)
			}
		}
	}

	// Preserve original topo order, keep only the selected set
	filtered := stack.Services[:0:0]
	for _, svc := range stack.Services {
		if keep[svc.Name] {
			filtered = append(filtered, svc)
		}
	}
	stack.Services = filtered
	return nil
}
