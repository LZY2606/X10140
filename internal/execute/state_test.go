package execute

import (
	"testing"
	"time"

	"migrationplanner/internal/model"
	"migrationplanner/internal/planner"
)

func chainPlan(t *testing.T) model.PlanRequest {
	t.Helper()
	loc, _ := time.LoadLocation("Asia/Tokyo")
	ws := time.Date(2026, 9, 22, 2, 0, 0, 0, loc)
	return model.PlanRequest{WindowStart: ws, WindowEnd: ws.Add(4 * time.Hour)}
}

func chainNodes() map[string]model.Node {
	return map[string]model.Node{
		"a": {ID: "a", Duration: 10 * time.Minute, Rollbackable: true, OutputFingerprint: "fp-A"},
		"b": {ID: "b", Duration: 10 * time.Minute, Rollbackable: true, DependsOn: []string{"a"}, InputFingerprint: "fp-A", OutputFingerprint: "fp-B"},
		"c": {ID: "c", Duration: 10 * time.Minute, Rollbackable: true, DependsOn: []string{"b"}, InputFingerprint: "fp-B", OutputFingerprint: "fp-C"},
	}
}

func mustPlan(t *testing.T, nodes map[string]model.Node) *model.Plan {
	t.Helper()
	p, err := planner.Plan("plan-1", chainPlan(t), nodes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRecovery_SucceededMatchingFingerprintNotRerun(t *testing.T) {
	p := mustPlan(t, chainNodes())
	now := time.Now()
	evs := []model.Event{
		{Seq: 1, Time: now, Type: model.EvNodeStarted, PlanID: p.ID, NodeID: "a"},
		{Seq: 2, Time: now.Add(time.Minute), Type: model.EvNodeSucceeded, PlanID: p.ID, NodeID: "a", OutputFingerprint: "fp-A"},
	}
	st := Replay(p.ID, evs)
	Derive(p, st)

	if st.Nodes["a"].Status != model.StatusSucceeded {
		t.Fatalf("a should be succeeded, got %s", st.Nodes["a"].Status)
	}
	can, _ := CanStart(p, st, "a")
	if can {
		t.Fatal("succeeded node with matching fingerprint must not be re-runnable")
	}
	// b is ready because a succeeded with matching fingerprint.
	can, reason := CanStart(p, st, "b")
	if !can {
		t.Fatalf("b should be startable after a succeeds: %s", reason)
	}
}

func TestRecovery_PartialExecutionInterruptedNodeCanRerun(t *testing.T) {
	p := mustPlan(t, chainNodes())
	now := time.Now()
	evs := []model.Event{
		{Seq: 1, Time: now, Type: model.EvNodeStarted, PlanID: p.ID, NodeID: "a"},
		{Seq: 2, Time: now.Add(time.Minute), Type: model.EvNodeSucceeded, PlanID: p.ID, NodeID: "a", OutputFingerprint: "fp-A"},
		{Seq: 3, Time: now.Add(2 * time.Minute), Type: model.EvNodeStarted, PlanID: p.ID, NodeID: "b"},
		{Seq: 4, Time: now.Add(3 * time.Minute), Type: model.EvCrashSimulated, PlanID: p.ID},
	}
	st := Replay(p.ID, evs)
	Derive(p, st)

	if !st.Nodes["b"].Interrupted || st.Nodes["b"].Status != model.StatusRunning {
		t.Fatalf("b must remain running+interrupted after crash, got status=%s interrupted=%v", st.Nodes["b"].Status, st.Nodes["b"].Interrupted)
	}
	if st.Nodes["a"].Status != model.StatusSucceeded {
		t.Fatal("a must stay succeeded and be skipped on recovery")
	}
	// b can be re-started after a crash (interrupted running nodes are safe to rerun).
	can, _ := CanStart(p, st, "b")
	if !can {
		t.Fatal("interrupted running node b should be safe to restart after recovery")
	}
}

func TestFingerprintDrift_BlocksWholeSuccessorChain(t *testing.T) {
	p := mustPlan(t, chainNodes())
	now := time.Now()
	evs := []model.Event{
		{Seq: 1, Time: now, Type: model.EvNodeStarted, PlanID: p.ID, NodeID: "a"},
		{Seq: 2, Time: now.Add(time.Minute), Type: model.EvNodeSucceeded, PlanID: p.ID, NodeID: "a", OutputFingerprint: "fp-UNEXPECTED"},
	}
	st := Replay(p.ID, evs)
	Derive(p, st)

	a := st.Nodes["a"]
	if !a.Drift || a.Status != model.StatusManual {
		t.Fatalf("drifted node must be 需要人工接管, got status=%s drift=%v reasons=%v", a.Status, a.Drift, a.Reasons)
	}
	// b and c are in the successor chain and must block.
	for _, id := range []string{"b", "c"} {
		can, reason := CanStart(p, st, id)
		if can {
			t.Fatalf("%s must not start after fingerprint drift upstream", id)
		}
		if reason == "" {
			t.Fatalf("%s needs an explicit block reason", id)
		}
	}
	if got := st.Nodes["b"].Status; got != model.StatusManual {
		t.Fatalf("direct successor b should be marked manual/blocked, got %s (%v)", got, st.Nodes["b"].Reasons)
	}
	if got := st.Nodes["c"].Status; got != model.StatusManual {
		t.Fatalf("transitive successor c should be marked manual/blocked, got %s (%v)", got, st.Nodes["c"].Reasons)
	}
}

func TestRollbackPlan_OnlyExecutedRollbackableAncestorsReverse(t *testing.T) {
	nodes := chainNodes()
	p := mustPlan(t, nodes)
	now := time.Now()
	// a succeeded, b started but crashed, c failed is impossible directly;
	// simulate a started b then failure on c requires b succeeded. Use:
	// a succeeded, b succeeded, c running -> failed.
	evs := []model.Event{
		{Seq: 1, Type: model.EvNodeStarted, PlanID: p.ID, NodeID: "a", Time: now},
		{Seq: 2, Type: model.EvNodeSucceeded, PlanID: p.ID, NodeID: "a", OutputFingerprint: "fp-A", Time: now.Add(time.Minute)},
		{Seq: 3, Type: model.EvNodeStarted, PlanID: p.ID, NodeID: "b", Time: now.Add(2 * time.Minute)},
		{Seq: 4, Type: model.EvNodeSucceeded, PlanID: p.ID, NodeID: "b", OutputFingerprint: "fp-B", Time: now.Add(3 * time.Minute)},
		{Seq: 5, Type: model.EvNodeStarted, PlanID: p.ID, NodeID: "c", Time: now.Add(4 * time.Minute)},
		{Seq: 6, Type: model.EvNodeFailed, PlanID: p.ID, NodeID: "c", Time: now.Add(5 * time.Minute)},
	}
	st := Replay(p.ID, evs)
	Derive(p, st)

	rp, err := BuildRollbackPlan(p, st, "c")
	if err != nil {
		t.Fatal(err)
	}
	if !rp.Complete {
		t.Fatalf("all ancestors rollbackable, plan must be complete: %s", rp.Boundary)
	}
	if len(rp.Steps) != 2 || rp.Steps[0].NodeID != "b" || rp.Steps[1].NodeID != "a" {
		t.Fatalf("steps must be reverse order [b a], got %+v", rp.Steps)
	}
}

func TestRollbackPlan_StopsAtNonRollbackableBoundary(t *testing.T) {
	nodes := chainNodes()
	// Make b non-rollbackable. b is an executed ancestor of failed c:
	// plan must stop at b and not include a (which is behind it).
	nb := nodes["b"]
	nb.Rollbackable = false
	nodes["b"] = nb
	p := mustPlan(t, nodes)
	now := time.Now()
	evs := []model.Event{
		{Seq: 1, Type: model.EvNodeStarted, PlanID: p.ID, NodeID: "a", Time: now},
		{Seq: 2, Type: model.EvNodeSucceeded, PlanID: p.ID, NodeID: "a", OutputFingerprint: "fp-A", Time: now.Add(time.Minute)},
		{Seq: 3, Type: model.EvNodeStarted, PlanID: p.ID, NodeID: "b", Time: now.Add(2 * time.Minute)},
		{Seq: 4, Type: model.EvNodeSucceeded, PlanID: p.ID, NodeID: "b", OutputFingerprint: "fp-B", Time: now.Add(3 * time.Minute)},
		{Seq: 5, Type: model.EvNodeStarted, PlanID: p.ID, NodeID: "c", Time: now.Add(4 * time.Minute)},
		{Seq: 6, Type: model.EvNodeFailed, PlanID: p.ID, NodeID: "c", Time: now.Add(5 * time.Minute)},
	}
	st := Replay(p.ID, evs)
	Derive(p, st)

	rp, err := BuildRollbackPlan(p, st, "c")
	if err != nil {
		t.Fatal(err)
	}
	if rp.Complete {
		t.Fatal("plan must be marked incomplete at non-rollbackable boundary")
	}
	if rp.Boundary == "" {
		t.Fatal("boundary explanation is mandatory when stopping")
	}
	if len(rp.Steps) != 0 {
		t.Fatalf("nothing can precede the b boundary; b itself and a must be omitted, got %+v", rp.Steps)
	}
}

func TestRollbackPlan_MiddleBoundaryKeepsOnlyLaterSteps(t *testing.T) {
	// chain a -> b -> c -> d(fails); c non-rollbackable; only d... wait d is
	// failed. Build a -> b -> c -> d -> e(fails), c non-rollbackable:
	// reverse ancestors of e are d,c,b,a; d is rollbackable then stop at c.
	nodes := map[string]model.Node{
		"a": {ID: "a", Duration: time.Minute, Rollbackable: true, OutputFingerprint: "a"},
		"b": {ID: "b", Duration: time.Minute, Rollbackable: true, DependsOn: []string{"a"}, InputFingerprint: "a", OutputFingerprint: "b"},
		"c": {ID: "c", Duration: time.Minute, Rollbackable: false, DependsOn: []string{"b"}, InputFingerprint: "b", OutputFingerprint: "c"},
		"d": {ID: "d", Duration: time.Minute, Rollbackable: true, DependsOn: []string{"c"}, InputFingerprint: "c", OutputFingerprint: "d"},
		"e": {ID: "e", Duration: time.Minute, Rollbackable: true, DependsOn: []string{"d"}, InputFingerprint: "d", OutputFingerprint: "e"},
	}
	p := mustPlan(t, nodes)
	var evs []model.Event
	now := time.Now()
	for i, id := range []string{"a", "b", "c", "d"} {
		evs = append(evs, model.Event{Seq: i*2 + 1, Type: model.EvNodeStarted, PlanID: p.ID, NodeID: id, Time: now})
		evs = append(evs, model.Event{Seq: i*2 + 2, Type: model.EvNodeSucceeded, PlanID: p.ID, NodeID: id, OutputFingerprint: id, Time: now.Add(time.Minute)})
	}
	evs = append(evs, model.Event{Seq: 20, Type: model.EvNodeStarted, PlanID: p.ID, NodeID: "e", Time: now})
	evs = append(evs, model.Event{Seq: 21, Type: model.EvNodeFailed, PlanID: p.ID, NodeID: "e", Time: now.Add(time.Minute)})

	st := Replay(p.ID, evs)
	Derive(p, st)
	rp, err := BuildRollbackPlan(p, st, "e")
	if err != nil {
		t.Fatal(err)
	}
	if rp.Complete {
		t.Fatal("must be incomplete due to non-rollbackable c")
	}
	if len(rp.Steps) != 1 || rp.Steps[0].NodeID != "d" {
		t.Fatalf("only d (later than boundary c) may be planned, got %+v", rp.Steps)
	}
}

func TestRollbackPlan_RejectsNonFailedNode(t *testing.T) {
	p := mustPlan(t, chainNodes())
	st := Replay(p.ID, nil)
	Derive(p, st)
	if _, err := BuildRollbackPlan(p, st, "a"); err == nil {
		t.Fatal("only failed nodes can have a rollback plan")
	}
}
