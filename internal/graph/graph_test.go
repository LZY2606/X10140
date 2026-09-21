package graph

import (
	"strings"
	"testing"

	"migplanner/internal/model"
)

func TestDetectCycleConcretePath(t *testing.T) {
	nodes := []*model.Node{
		{ID: "a", Prereqs: []string{"c"}},
		{ID: "b", Prereqs: []string{"a"}},
		{ID: "c", Prereqs: []string{"b"}},
		{ID: "d", Prereqs: []string{"a"}},
	}
	err := DetectCycle(nodes)
	if err == nil {
		t.Fatal("expected cycle error")
	}
	ce, ok := err.(*CycleError)
	if !ok {
		t.Fatalf("want *CycleError, got %T: %v", err, err)
	}
	// Concrete closed loop must repeat its first node at the end.
	if len(ce.Cycle) < 3 || ce.Cycle[0] != ce.Cycle[len(ce.Cycle)-1] {
		t.Fatalf("cycle path not a closed loop: %v", ce.Cycle)
	}
	// Every consecutive edge must be a real prereq edge.
	pred := Adjacency(nodes)
	for i := 0; i+1 < len(ce.Cycle); i++ {
		found := false
		// Direction in the reported path: node -> its prerequisite.
		for _, p := range pred[ce.Cycle[i]] {
			if p == ce.Cycle[i+1] {
				found = true
			}
		}
		if !found {
			t.Fatalf("cycle edge %s -> %s is not a prereq edge (full: %v)",
				ce.Cycle[i+1], ce.Cycle[i], ce.Cycle)
		}
	}
	if !strings.Contains(err.Error(), "a -> b -> c -> a") &&
		!strings.Contains(err.Error(), "c -> b -> a -> c") &&
		!strings.Contains(err.Error(), "b -> a -> c -> b") &&
		!strings.Contains(err.Error(), "a -> c -> b -> a") {
		t.Fatalf("error should print concrete cycle, got %q", err.Error())
	}
}

func TestDetectSelfCycle(t *testing.T) {
	err := DetectCycle([]*model.Node{{ID: "x", Prereqs: []string{"x"}}})
	if err == nil || !strings.Contains(err.Error(), "x -> x") {
		t.Fatalf("want self cycle message, got %v", err)
	}
}

func TestNoCycle(t *testing.T) {
	nodes := []*model.Node{
		{ID: "a"}, {ID: "b", Prereqs: []string{"a"}},
		{ID: "c", Prereqs: []string{"a", "b"}},
	}
	if err := DetectCycle(nodes); err != nil {
		t.Fatalf("unexpected cycle: %v", err)
	}
	if bad := UnknownPrereqs(nodes); len(bad) != 0 {
		t.Fatalf("unexpected unknown prereqs: %v", bad)
	}
}

func TestUnknownPrereq(t *testing.T) {
	bad := UnknownPrereqs([]*model.Node{{ID: "a", Prereqs: []string{"ghost"}}})
	if len(bad) != 1 || bad[0] != "ghost" {
		t.Fatalf("got %v", bad)
	}
}
