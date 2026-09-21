package store

import (
	"errors"
	"testing"

	"migplanner/internal/model"
)

func TestAppendOnlyAndIdempotency(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	n := &model.Node{ID: "n1", Name: "v1", Minutes: 5, Output: "o1", Inputs: []string{"s0"}}
	if err := st.SaveNode(n); err != nil {
		t.Fatal(err)
	}
	plan := &model.Plan{ID: "plan-A", StartFP: "s0", Nodes: []*model.PlanNode{
		{Node: *n, Wave: 0},
	}, NodeByID: map[string]*model.PlanNode{}}
	plan.NodeByID["n1"] = plan.Nodes[0]
	if err := st.CreatePlan(plan); err != nil {
		t.Fatal(err)
	}

	e := &model.ExecEvent{PlanID: "plan-A", NodeID: "n1", Type: model.EventStarted, IdemKey: "key-1"}
	if err := st.AppendExec(e); err != nil {
		t.Fatal(err)
	}
	// Identical submission with same idempotency key -> stored once.
	e2 := &model.ExecEvent{PlanID: "plan-A", NodeID: "n1", Type: model.EventSucceeded, IdemKey: "key-1"}
	if err := st.AppendExec(e2); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("expected ErrDuplicate, got %v", err)
	}
	// Same key value on a different plan is an independent submission.
	e3 := &model.ExecEvent{PlanID: "plan-other", NodeID: "n1", Type: model.EventStarted, IdemKey: "key-1"}
	if err := st.AppendExec(e3); err != nil {
		t.Fatalf("idempotency is scoped per plan, got %v", err)
	}
	// Different key records normally.
	e4 := &model.ExecEvent{PlanID: "plan-A", NodeID: "n1", Type: model.EventSucceeded, IdemKey: "key-2", ActualFP: "o1"}
	if err := st.AppendExec(e4); err != nil {
		t.Fatal(err)
	}
	events := st.ExecEvents("plan-A")
	if len(events) != 2 {
		t.Fatalf("plan-A should hold exactly 2 events, got %d", len(events))
	}
	if events[0].Type != model.EventStarted || events[1].Type != model.EventSucceeded {
		t.Fatalf("event order/content wrong: %+v", events)
	}
	st.Close()

	// Reopening replays the append-only log into identical state.
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if len(st2.ListNodes()) != 1 || st2.GetPlan("plan-A") == nil {
		t.Fatal("replay lost nodes/plan")
	}
	if got := len(st2.ExecEvents("plan-A")); got != 2 {
		t.Fatalf("replay should restore 2 events, got %d", got)
	}
}

// Later node edits append new versions and never mutate historical plans.
func TestNodeEditsDoNotRewriteHistory(t *testing.T) {
	dir := t.TempDir()
	st, _ := Open(dir)
	defer st.Close()

	_ = st.SaveNode(&model.Node{ID: "n1", Name: "first", Minutes: 5, Output: "o1", Inputs: []string{"s0"}})
	plan := &model.Plan{ID: "plan-X", StartFP: "s0", Nodes: []*model.PlanNode{
		{Node: model.Node{ID: "n1", Name: "first", Minutes: 5, Output: "o1", Inputs: []string{"s0"}}, Wave: 0},
	}, NodeByID: map[string]*model.PlanNode{}}
	plan.NodeByID["n1"] = plan.Nodes[0]
	if err := st.CreatePlan(plan); err != nil {
		t.Fatal(err)
	}
	_ = st.SaveNode(&model.Node{ID: "n1", Name: "renamed + 99m", Minutes: 99, Output: "o9", Inputs: []string{"s0"}})

	hist := st.GetPlan("plan-X")
	if hist.Nodes[0].Name != "first" || hist.Nodes[0].Minutes != 5 || hist.Nodes[0].Output != "o1" {
		t.Fatalf("historical plan snapshot was rewritten: %+v", hist.Nodes[0])
	}
	if cur := st.ListNodes()[0]; cur.Name != "renamed + 99m" || cur.Output != "o9" {
		t.Fatalf("current node definition should reflect new version, got %+v", cur)
	}
}
