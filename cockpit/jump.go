package cockpit

import "strings"

// The Ctrl+K jump palette: a fuzzy filter over every space and, inside it,
// every container/service — so the operator reaches one directly by name
// across however many nodes are grouped in the table, instead of paging
// through it. Confirming a hit selects its space and, when the hit was a
// container, drills straight into the service list on it.

// jumpHit is one candidate the palette can jump to.
type jumpHit struct {
	// spaceIndex is the row's index into m.Spaces — the selection target.
	spaceIndex int
	space      *Space
	// service is set when the hit is a container/service inside the space,
	// nil when the hit is the space itself.
	service *Service
	// label is what the palette lists: "space" or "space › container".
	label string
	// node annotates a remote hit so a same-named space/container on another
	// node is not ambiguous in the results list.
	node string
}

// OpenJump opens the palette. Closed elsewhere first: the palette and the logs
// panel both want the printable-key stream, and only one prompt owns it at a
// time.
func (m *Model) OpenJump() bool {
	if m.LogsOpen {
		return false
	}
	m.JumpOpen = true
	m.JumpQuery = ""
	m.JumpIndex = 0
	m.Note = ""
	return true
}

// CloseJump closes the palette without acting, leaving the current selection
// untouched.
func (m *Model) CloseJump() bool {
	if !m.JumpOpen {
		return false
	}
	m.JumpOpen = false
	m.JumpQuery = ""
	m.JumpIndex = 0
	return true
}

// JumpInput appends one typed rune to the query and re-clamps the highlighted
// result, so the palette tracks the query as it is typed.
func (m *Model) JumpInput(r rune) bool {
	if !m.JumpOpen {
		return false
	}
	m.JumpQuery += string(r)
	m.JumpIndex = 0
	return true
}

// JumpBackspace deletes the last rune of the query.
func (m *Model) JumpBackspace() bool {
	if !m.JumpOpen {
		return false
	}
	if m.JumpQuery == "" {
		return false
	}
	r := []rune(m.JumpQuery)
	m.JumpQuery = string(r[:len(r)-1])
	m.JumpIndex = 0
	return true
}

// moveJump shifts the highlighted result, clamped to the current match list.
func (m *Model) moveJump(delta int) bool {
	hits := m.jumpHits()
	if len(hits) == 0 {
		return false
	}
	next := m.JumpIndex + delta
	if next < 0 {
		next = 0
	}
	if next > len(hits)-1 {
		next = len(hits) - 1
	}
	if next == m.JumpIndex {
		return false
	}
	m.JumpIndex = next
	return true
}

// jumpHits lists every space and, inside it, every container/service that
// matches the query (substring, case-insensitive on either the space name or
// the container/service name). An empty query matches nothing: the palette is
// for jumping to a KNOWN name, not for browsing — that is what the table is for.
func (m *Model) jumpHits() []jumpHit {
	if m.JumpQuery == "" {
		return nil
	}
	needle := strings.ToLower(m.JumpQuery)
	var out []jumpHit
	for i := range m.Spaces {
		sp := &m.Spaces[i]
		node := sp.Node
		if node == "" {
			node = "local"
		}
		if strings.Contains(strings.ToLower(sp.Name), needle) {
			out = append(out, jumpHit{spaceIndex: i, space: sp, label: sp.Name, node: node})
		}
		for j := range sp.Services {
			svc := &sp.Services[j]
			if strings.Contains(strings.ToLower(svc.Name), needle) {
				out = append(out, jumpHit{
					spaceIndex: i, space: sp, service: svc,
					label: sp.Name + " › " + svc.Name, node: node,
				})
			}
		}
	}
	return out
}

// ConfirmJump selects the highlighted hit's space and, when the hit is a
// container/service, drills straight into it — the same end state EnterSpace
// followed by picking that service would reach, in one step. Closes the
// palette either way.
func (m *Model) ConfirmJump() bool {
	if !m.JumpOpen {
		return false
	}
	hits := m.jumpHits()
	if m.JumpIndex < 0 || m.JumpIndex >= len(hits) {
		m.CloseJump()
		return true
	}
	hit := hits[m.JumpIndex]
	m.Selected = hit.spaceIndex
	m.Level = LevelSpaces
	m.SvcIndex = 0
	if hit.service != nil && len(hit.space.Services) > 0 {
		m.Level = LevelServices
		for i := range hit.space.Services {
			if &hit.space.Services[i] == hit.service {
				m.SvcIndex = i
				break
			}
		}
	}
	m.CloseJump()
	m.Note = ""
	return true
}
