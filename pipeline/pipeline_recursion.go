// Package runner — compile-time scope/output recursion check.
//
// Universal rule: a pipeline that writes a file matching its own
// trigger scope creates an infinite trigger loop the moment CI
// automation auto-commits the result (push -> regen -> auto-commit ->
// push -> ...). The dormant trap exists even on pipelines that don't
// auto-commit today, so we warn preventively. We warn instead of
// failing because legitimate "regen + commit by hand" workflows exist;
// the user keeps full control and only sees a one-line hint per pair.
package pipeline

import (
	"fmt"
	"io"
	"sort"

	"github.com/bmatcuk/doublestar/v4"
)

// checkScopeOutputRecursion walks every declared step output and
// reports each (step, output, scope, glob) pair where the output would
// re-trigger the pipeline's own scope. Detection is limited to
// declared `outputs:` from engine 2.4; shell `run:` strings are NOT
// parsed (out of scope, per issue #429).
//
// Pairs are emitted in deterministic order: sorted by step name, then
// output path. Compile is never failed by this check — the function
// returns nothing and only writes to w.
func checkScopeOutputRecursion(p Pipeline, taskPath string, w io.Writer) {
	if w == nil || len(p.Scopes) == 0 || len(p.Steps) == 0 {
		return
	}

	type hit struct {
		step   string
		output string
		scope  string
		glob   string
	}
	var hits []hit

	// Sort scope names so a given output reports against scopes in a
	// stable order when it matches more than one.
	scopeNames := make([]string, 0, len(p.Scopes))
	for name := range p.Scopes {
		scopeNames = append(scopeNames, name)
	}
	sort.Strings(scopeNames)

	for _, s := range p.Steps {
		for _, out := range s.Outputs {
			if out == "" {
				continue
			}
			for _, scope := range scopeNames {
				for _, glob := range p.Scopes[scope] {
					if glob == "" {
						continue
					}
					ok, err := doublestar.PathMatch(glob, out)
					if err != nil || !ok {
						continue
					}
					hits = append(hits, hit{
						step:   s.Name,
						output: out,
						scope:  scope,
						glob:   glob,
					})
				}
			}
		}
	}

	if len(hits) == 0 {
		return
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].step != hits[j].step {
			return hits[i].step < hits[j].step
		}
		return hits[i].output < hits[j].output
	})

	for _, h := range hits {
		fmt.Fprintf(w, "warning: scope-output recursion risk in %s\n", taskPath)
		fmt.Fprintf(w, "  step %q writes output %q\n", h.step, h.output)
		fmt.Fprintf(w, "  which matches scope %q glob %q\n", h.scope, h.glob)
		fmt.Fprintln(w, "  -> automating this pipeline would create an infinite trigger loop")
		fmt.Fprintln(w, "  -> remove the generated file from the scope, or use a separate output dir")
	}
}
