package store

import (
	"testing"
	"time"

	"migrationplanner/internal/model"
)

func TestAppend_IdempotencyKeyRecordedOnce(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ev := model.Event{Type: model.EvNodeStarted, PlanID: "p1", NodeID: "a", Time: time.Now()}
	first, dup, err := s.Append(ev, "key-123")
	if err != nil || dup {
		t.Fatalf("first append: dup=%v err=%v", dup, err)
	}
	again := ev
	again.Detail = "duplicate payload must be ignored"
	second, dup2, err := s.Append(again, "key-123")
	if err != nil {
		t.Fatal(err)
	}
	if !dup2 {
		t.Fatal("second append with same idempotency key must be deduplicated")
	}
	if second.Seq != first.Seq || second.Detail != first.Detail {
		t.Fatalf("dedup must return the original event, got %+v vs %+v", first, second)
	}
	if len(s.Events()) != 1 {
		t.Fatalf("exactly one event must be stored, got %d", len(s.Events()))
	}
}

func TestIdempotencySurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	_, _, _ = s.Append(model.Event{Type: model.EvNodeSucceeded, PlanID: "p", NodeID: "a"}, "persist-key")

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.Events()) != 1 {
		t.Fatalf("event log must reload, got %d", len(s2.Events()))
	}
	_, dup, err := s2.Append(model.Event{Type: model.EvNodeSucceeded, PlanID: "p", NodeID: "a", Detail: "x"}, "persist-key")
	if err != nil {
		t.Fatal(err)
	}
	if !dup {
		t.Fatal("idempotency keys must survive restart via the append-only log")
	}
}

func TestNodeEditsDoNotRewriteHistoricalPlan(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	n1 := model.Node{ID: "a", Name: "old", Duration: time.Minute}
	_, _, _ = s.Append(model.Event{Type: model.EvNodeUpserted, Node: &n1}, "")

	// Snapshot node into a plan event.
	plan := &model.Plan{ID: "plan-old", NodeSnapshot: map[string]model.Node{"a": n1}, RequestedAt: time.Now()}
	_, _, _ = s.Append(model.Event{Type: model.EvPlanCreated, PlanID: plan.ID, Plan: plan}, "")

	// Edit the node afterwards.
	n2 := n1
	n2.Name = "new"
	n2.Duration = 5 * time.Minute
	_, _, _ = s.Append(model.Event{Type: model.EvNodeUpserted, Node: &n2}, "")

	got, ok := s.Plan("plan-old")
	if !ok {
		t.Fatal("historical plan missing")
	}
	if got.NodeSnapshot["a"].Name != "old" || got.NodeSnapshot["a"].Duration != time.Minute {
		t.Fatalf("historical plan snapshot was rewritten: %+v", got.NodeSnapshot["a"])
	}
	if cur := s.Nodes()["a"]; cur.Name != "new" {
		t.Fatalf("current node def should be new, got %+v", cur)
	}
}
