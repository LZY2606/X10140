package runtime

import (
	"strings"
	"testing"

	"migplanner/internal/model"
	"migplanner/internal/scheduler"
)

func mkPlan(t *testing.T, nodes []*model.Node) *model.Plan {
	t.Helper()
	w := model.Window{Start: "2026-09-22T22:00", End: "2026-09-23T01:00", Zone: "Asia/Tokyo"}
	r, err := scheduler.Build(nodes, "s0", w)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	p := &model.Plan{ID: "p1", StartFP: "s0", Window: w, Nodes: r.Nodes, Waves: r.Waves,
		Blocked: r.Blocked, NodeByID: map[string]*model.PlanNode{}}
	for _, n := range p.Nodes {
		p.NodeByID[n.ID] = n
	}
	return p
}

func chainNodes(nonRollbackable bool) []*model.Node {
	mk := func(id string, mins int, preds []string, rb bool, in, out string) *model.Node {
		return &model.Node{ID: id, Name: id, Prereqs: preds, Minutes: mins, Rollback: rb,
			Inputs: []string{in}, Output: out}
	}
	return []*model.Node{
		mk("a", 10, nil, true, "s0", "oa"),
		mk("b", 10, []string{"a"}, true, "oa", "ob"),
		mk("c", 10, []string{"b"}, !nonRollbackable, "ob", "oc"),
		mk("d", 10, []string{"c"}, true, "oc", "od"),
	}
}

func ev(node string, et model.ExecEventType, key string, fp string) *model.ExecEvent {
	return &model.ExecEvent{PlanID: "p1", NodeID: node, Type: et, IdemKey: key,
		ActualFP: fp, At: "2026-09-22T13:00:00Z", Seq: 1}
}

// After partial execution + crash restart:
//   - succeeded nodes whose fingerprint matches are skipped (never re-run);
//   - a node still running becomes manual takeover;
//   - everything downstream is blocked.
func TestPartialRecoveryInterrupted(t *testing.T) {
	plan := mkPlan(t, chainNodes(false))
	events := []*model.ExecEvent{
		ev("a", model.EventStarted, "k1", ""),
		ev("a", model.EventSucceeded, "k2", "oa"),
		ev("b", model.EventStarted, "k3", ""),
	}
	v := Project(plan, events, true)
	if got := v.Nodes["a"].Status; got != model.StatusSuccess {
		t.Fatalf("a stays success, got %s", got)
	}
	if v.Nodes["a"].Blocked || v.Nodes["a"].Runnable {
		t.Fatal("successful matching node must be skipped, not runnable again")
	}
	if got := v.Nodes["b"].Status; got != model.StatusManual {
		t.Fatalf("interrupted running node must become manual, got %s", got)
	}
	if !v.Nodes["b"].Handled || !v.Nodes["b"].Blocked {
		t.Fatal("interrupted node must be handled + blocked")
	}
	for _, id := range []string{"c", "d"} {
		if !v.Nodes[id].Blocked {
			t.Fatalf("successor %s must be blocked by interrupted b", id)
		}
	}
	if !contains(v.Recovery.Interrupted, "b") || !contains(v.Recovery.Skipped, "a") {
		t.Fatalf("report wrong: %+v", v.Recovery)
	}
	if !contains(v.Recovery.ChainBlocked, "c") || !contains(v.Recovery.ChainBlocked, "d") {
		t.Fatalf("chain blocked should include c,d: %+v", v.Recovery.ChainBlocked)
	}
}

// Fingerprint drift: do not re-run; the drifted node and the whole successor
// chain are blocked/manual.
func TestFingerprintDriftBlocksChain(t *testing.T) {
	plan := mkPlan(t, chainNodes(false))
	events := []*model.ExecEvent{
		ev("a", model.EventSucceeded, "k1", "oa"),
		ev("b", model.EventSucceeded, "k2", "UNEXPECTED-FP"),
	}
	v := Project(plan, events, true)
	b := v.Nodes["b"]
	if b.Status != model.StatusManual || !b.Handled || !b.Blocked {
		t.Fatalf("drifted b must become manual/blocked, got %s handled=%v", b.Status, b.Handled)
	}
	if !strings.Contains(b.Reason, "漂移") || !strings.Contains(b.Reason, "UNEXPECTED-FP") {
		t.Fatalf("drift reason missing detail: %q", b.Reason)
	}
	if !contains(v.Recovery.Drifted, "b") {
		t.Fatalf("drift report: %+v", v.Recovery.Drifted)
	}
	for _, id := range []string{"c", "d"} {
		if !v.Nodes[id].Blocked {
			t.Fatalf("successor %s must be blocked by drift", id)
		}
	}
	if v.Nodes["a"].Blocked {
		t.Fatal("matching sibling a must not be blocked")
	}
}

// Rollback of a failed node only includes executed rollback-able ancestors in
// reverse order; a non-rollbackable ancestor stops the plan explicitly.
func TestRollbackBoundaryNonRollbackable(t *testing.T) {
	plan := mkPlan(t, chainNodes(true)) // c is non-rollbackable
	events := []*model.ExecEvent{
		ev("a", model.EventSucceeded, "k1", "oa"),
		ev("b", model.EventSucceeded, "k2", "ob"),
		ev("c", model.EventSucceeded, "k3", "oc"),
		ev("d", model.EventStarted, "k4", ""),
		ev("d", model.EventFailed, "k5", ""),
	}
	v := Project(plan, events, false)
	rp, err := PlanRollback(v, "d")
	if err != nil {
		t.Fatal(err)
	}
	if rp.Complete {
		t.Fatal("plan must be incomplete due to non-rollbackable c")
	}
	if rp.StoppedAt != "c" {
		t.Fatalf("stop boundary must be c, got %q", rp.StoppedAt)
	}
	if !strings.Contains(rp.BoundaryReason, "不可回滚") {
		t.Fatalf("boundary reason must say non-rollbackable: %q", rp.BoundaryReason)
	}
	got := stepIDs(rp)
	want := []string{"d"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("only d (the target) is safely rollback-able; got %v, boundary: %s", got, rp.BoundaryReason)
	}
}

func TestRollbackCompleteWhenAllRollbackable(t *testing.T) {
	plan := mkPlan(t, chainNodes(false))
	events := []*model.ExecEvent{
		ev("a", model.EventSucceeded, "k1", "oa"),
		ev("b", model.EventSucceeded, "k2", "ob"),
		ev("c", model.EventSucceeded, "k3", "oc"),
		ev("d", model.EventFailed, "k4", ""),
	}
	v := Project(plan, events, false)
	rp, err := PlanRollback(v, "d")
	if err != nil {
		t.Fatal(err)
	}
	if !rp.Complete || rp.StoppedAt != "" {
		t.Fatalf("expected complete plan, got %+v %q", rp.Complete, rp.BoundaryReason)
	}
	if got := strings.Join(stepIDs(rp), ","); got != "d,c,b,a" {
		t.Fatalf("reverse execution order d,c,b,a expected, got %s", got)
	}
}

// Manual takeover on an ancestor is itself a hard rollback boundary.
func TestRollbackBoundaryManual(t *testing.T) {
	plan := mkPlan(t, chainNodes(false))
	events := []*model.ExecEvent{
		ev("a", model.EventSucceeded, "k1", "oa"),
		ev("b", model.EventManual, "k2", ""),
		ev("c", model.EventFailed, "k3", ""),
	}
	v := Project(plan, events, false)
	rp, err := PlanRollback(v, "c")
	if err != nil {
		t.Fatal(err)
	}
	if rp.Complete || rp.StoppedAt != "b" {
		t.Fatalf("manual b must stop rollback, got %+v stop=%s", rp.Complete, rp.StoppedAt)
	}
}

// Target itself non-rollbackable: zero steps, immediate stop.
func TestRollbackTargetNonRollbackable(t *testing.T) {
	plan := mkPlan(t, chainNodes(true))
	events := []*model.ExecEvent{
		ev("a", model.EventSucceeded, "k1", "oa"),
		ev("b", model.EventSucceeded, "k2", "ob"),
		ev("c", model.EventFailed, "k3", ""),
	}
	v := Project(plan, events, false)
	rp, err := PlanRollback(v, "c")
	if err != nil {
		t.Fatal(err)
	}
	if rp.Complete || len(rp.Steps) != 0 || rp.StoppedAt != "c" {
		t.Fatalf("expected empty plan stopped at c, got %+v", rp)
	}
}

func TestTransitionValidation(t *testing.T) {
	if err := ValidateTransition(model.StatusPending, model.EventSucceeded); err == nil {
		t.Fatal("pending -> succeeded must be illegal")
	}
	if err := ValidateTransition(model.StatusRunning, model.EventSucceeded); err != nil {
		t.Fatalf("running -> succeeded must be legal: %v", err)
	}
	if err := ValidateTransition(model.StatusManual, model.EventStarted); err == nil {
		t.Fatal("manual takeover must be terminal")
	}
}

func stepIDs(rp *RollbackPlan) []string {
	var ids []string
	for _, s := range rp.Steps {
		ids = append(ids, s.NodeID)
	}
	return ids
}

// Rolled-back or failed prerequisites also block the successor chain during
// recovery reporting.
func TestRecoveryChainBlockedByRollback(t *testing.T) {
	plan := mkPlan(t, chainNodes(false))
	events := []*model.ExecEvent{
		ev("a", model.EventSucceeded, "k1", "oa"),
		ev("a", model.EventRolled, "k2", ""),
	}
	v := Project(plan, events, true)
	if !v.Nodes["b"].Blocked {
		t.Fatal("b must be blocked after a was rolled back")
	}
	if !contains(v.Recovery.ChainBlocked, "b") || !contains(v.Recovery.ChainBlocked, "d") {
		t.Fatalf("chain b,c,d should be reported blocked: %+v", v.Recovery.ChainBlocked)
	}
}
