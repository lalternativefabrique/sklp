package plan

import "github.com/lalternativefabrique/sklp/internal/rolloutgate"

func orderPublishSteps(steps []CIStepInput, g *rolloutgate.Graph) []CIStepInput {
	if g == nil {
		return steps
	}
	var slots []int
	names := make([]string, 0, len(steps))
	for i, s := range steps {
		if s.Kind == string(KindCIStepPublish) {
			slots = append(slots, i)
			names = append(names, s.Image)
		}
	}
	out := append([]CIStepInput(nil), steps...)
	for slot, idx := range g.Order(names) {
		out[slots[slot]] = steps[slots[idx]]
	}
	return out
}

// markRolloutWaits flags each published image another image of this run
// depends on. Compile-skipped steps ship nothing, so they neither wait nor make
// another step wait.
func markRolloutWaits(steps []CIStep, g *rolloutgate.Graph) {
	for i := range steps {
		first := steps[i].Publish
		if first == nil || !g.Watched(first.Image) {
			continue
		}
		for j := range steps {
			if second := steps[j].Publish; i != j && second != nil && g.Before(first.Image, second.Image) {
				first.WaitRollout = true
				break
			}
		}
	}
}
