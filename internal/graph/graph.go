package graph

import (
	"fmt"
	"sort"
	"strings"

	"migplanner/internal/model"
)

// CycleError carries one concrete closed path such as
// a -> b -> c -> a.
type CycleError struct {
	Cycle []string
}

func (e *CycleError) Error() string {
	return fmt.Sprintf("依赖图存在环: %s", strings.Join(e.Cycle, " -> "))
}

// Adjacency returns prerequisite edges and a sorted node id list.
func Adjacency(nodes []*model.Node) map[string][]string {
	preds := make(map[string][]string)
	seen := map[string]struct{}{}
	for _, n := range nodes {
		seen[n.ID] = struct{}{}
		preds[n.ID] = append(preds[n.ID], n.Prereqs...)
	}
	for id := range preds {
		sort.Strings(preds[id])
	}
	return preds
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// DetectCycle returns a CycleError with a concrete loop when the directed
// prereq graph (edge prereq -> node) contains one.
func DetectCycle(nodes []*model.Node) error {
	preds := Adjacency(nodes)
	known := idSet(nodes)
	ids := sortedKeys(known)

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	onStack := map[string]bool{}

	var dfs func(id string) error
	dfs = func(id string) error {
		color[id] = gray
		stack = append(stack, id)
		onStack[id] = true
		for _, p := range preds[id] {
			if _, ok := known[p]; !ok {
				continue
			}
			switch color[p] {
			case gray:
				// Close the concrete cycle: walk stack back to p.
				idx := 0
				for i, s := range stack {
					if s == p {
						idx = i
						break
					}
				}
				cyc := append([]string{}, stack[idx:]...)
				cyc = append(cyc, p)
				return &CycleError{Cycle: cyc}
			case white:
				if err := dfs(p); err != nil {
					return err
				}
			}
		}
		stack = stack[:len(stack)-1]
		onStack[id] = false
		color[id] = black
		return nil
	}

	for _, id := range ids {
		if color[id] == white {
			if err := dfs(id); err != nil {
				return err
			}
		}
	}
	return nil
}

func idSet(nodes []*model.Node) map[string]struct{} {
	s := map[string]struct{}{}
	for _, n := range nodes {
		s[n.ID] = struct{}{}
	}
	return s
}

// UnknownPrereqs returns prerequisites referencing nodes that do not exist.
func UnknownPrereqs(nodes []*model.Node) []string {
	ids := idSet(nodes)
	var bad []string
	seen := map[string]struct{}{}
	for _, n := range nodes {
		for _, p := range n.Prereqs {
			if _, ok := ids[p]; !ok {
				if _, dup := seen[p]; !dup {
					bad = append(bad, p)
					seen[p] = struct{}{}
				}
			}
		}
	}
	sort.Strings(bad)
	return bad
}
