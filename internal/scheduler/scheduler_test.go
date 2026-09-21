package scheduler

import (
	"strings"
	"testing"

	"migplanner/internal/model"
)

func win60() model.Window {
	return model.Window{Start: "2026-09-22T22:00", End: "2026-09-22T23:00", Zone: "Asia/Tokyo"}
}

func node(id string, mins int, preds, res, ins []string, out string, rb bool) *model.Node {
	return &model.Node{ID: id, Name: id, Prereqs: preds, Resources: res,
		Minutes: mins, Rollback: rb, Inputs: ins, Output: out}
}

func waveOf(r *Result, id string) int {
	for _, n := range r.Nodes {
		if n.ID == id {
			return n.Wave
		}
	}
	return -2
}

// A mutex resource shared by independent nodes forces serial waves even
// though dependency depth would allow parallelism.
func TestMutexResourceSplitsWaves(t *testing.T) {
	nodes := []*model.Node{
		node("a", 10, nil, []string{"R"}, []string{"s0"}, "o1", true),
		node("b", 10, nil, []string{"R"}, []string{"s0"}, "o2", true),
		node("c", 10, nil, nil, []string{"s0"}, "o3", true),
	}
	r, err := Build(nodes, "s0", win60())
	if err != nil {
		t.Fatal(err)
	}
	wa, wb := waveOf(r, "a"), waveOf(r, "b")
	if wa == wb {
		t.Fatalf("nodes sharing mutex R must be in different waves, both %d", wa)
	}
	// c uses no resource; it shares the first wave with whichever of a/b
	// is placed first (stable id order => a first).
	if waveOf(r, "c") != 0 || waveOf(r, "a") != 0 {
		t.Fatalf("a and c expected wave 0, got a=%d c=%d", waveOf(r, "a"), waveOf(r, "c"))
	}
	if waveOf(r, "b") != 1 {
		t.Fatalf("b expected wave 1, got %d", waveOf(r, "b"))
	}
	var note string
	for _, n := range r.Nodes {
		if n.ID == "b" {
			note = n.Note
		}
	}
	if !strings.Contains(note, "互斥资源 R") || !strings.Contains(note, "顺延") {
		t.Fatalf("b should explain mutex postponement, got %q", note)
	}
}

// Window is inclusive at both ends: a chain finishing exactly at the end
// boundary is schedulable.
func TestWindowEndpointsInclusive(t *testing.T) {
	nodes := []*model.Node{
		node("a", 30, nil, nil, []string{"s0"}, "o1", true),
		node("b", 30, []string{"a"}, nil, []string{"o1"}, "o2", true),
	}
	r, err := Build(nodes, "s0", win60())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Blocked) != 0 {
		t.Fatalf("finishing exactly at window end must be allowed, got %+v", r.Blocked)
	}
	bn := r.Nodes[1]
	if bn.FinishMin != 60 || bn.StartMin != 30 {
		t.Fatalf("b expected 30..60, got %d..%d", bn.StartMin, bn.FinishMin)
	}
}

// One minute over the end boundary => blocked with an explicit reason.
func TestWindowOverflowBlocks(t *testing.T) {
	nodes := []*model.Node{
		node("a", 30, nil, nil, []string{"s0"}, "o1", true),
		node("b", 31, []string{"a"}, nil, []string{"o1"}, "o2", true),
	}
	r, err := Build(nodes, "s0", win60())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Blocked) != 1 || r.Blocked[0].ID != "b" {
		t.Fatalf("only b should be blocked, got %+v", r.Blocked)
	}
	if !strings.Contains(r.Blocked[0].Reason, "结束端点恰好完成允许") {
		t.Fatalf("reason should state endpoint semantics: %q", r.Blocked[0].Reason)
	}
}

// Timezone is interpreted via IANA zone; clock strings map to that zone.
func TestWindowTimezone(t *testing.T) {
	w := model.Window{Start: "2026-09-22T22:00", End: "2026-09-22T23:00", Zone: "Asia/Tokyo"}
	pw, err := ParseWindow(w)
	if err != nil {
		t.Fatal(err)
	}
	if off := pw.Start.UTC().Format("15:04"); off != "13:00" {
		t.Fatalf("22:00 JST must be 13:00 UTC, got %s", off)
	}
	if _, err := ParseWindow(model.Window{Start: "2026-09-22T22:00", End: "2026-09-22T22:00", Zone: "Asia/Tokyo"}); err == nil {
		t.Fatal("zero-length window must be rejected")
	}
	if _, err := ParseWindow(model.Window{Start: "t", End: "x", Zone: "Mars/Olympus"}); err == nil ||
		!strings.Contains(err.Error(), "无效时区") {
		t.Fatalf("bad zone error, got %v", err)
	}
}

// Same inputs must always produce identical wave assignment and intra-wave
// node order.
func TestStableScheduling(t *testing.T) {
	mk := func() []*model.Node {
		return []*model.Node{
			node("z", 5, nil, []string{"R"}, []string{"s0"}, "oz", true),
			node("a", 5, nil, []string{"R"}, []string{"s0"}, "oa", true),
			node("m", 5, nil, []string{"R"}, []string{"s0"}, "om", true),
			node("q", 5, []string{"a", "m", "z"}, nil, []string{"oa", "om", "oz"}, "oq", true),
		}
	}
	sig := func(r *Result) string {
		var sb strings.Builder
		for _, wv := range r.Waves {
			sb.WriteString("W" + string(rune('0'+wv.Index)) + ":")
			sb.WriteString(strings.Join(wv.NodeIDs, ",") + ";")
		}
		return sb.String()
	}
	first, err := Build(mk(), "s0", win60())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		r, err := Build(mk(), "s0", win60())
		if err != nil {
			t.Fatal(err)
		}
		if sig(r) != sig(first) {
			t.Fatalf("schedule not stable: %q vs %q", sig(r), sig(first))
		}
	}
	if got := strings.Join(first.Waves[0].NodeIDs, ","); got != "a" {
		t.Fatalf("smallest id must be placed first within same depth, got %q", got)
	}
}

// Fingerprint mismatch at a root and along a chain is reported per node and
// keeps the whole successor chain out of waves.
func TestFingerprintApplicability(t *testing.T) {
	nodes := []*model.Node{
		node("a", 5, nil, nil, []string{"other"}, "oa", true),
		node("b", 5, []string{"a"}, nil, []string{"oa"}, "ob", true),
		node("c", 5, nil, nil, []string{"s0"}, "oc", true),
	}
	r, err := Build(nodes, "s0", win60())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, b := range r.Blocked {
		ids[b.ID] = true
	}
	if !ids["a"] || !ids["b"] {
		t.Fatalf("a and its successor b must be blocked, got %+v", r.Blocked)
	}
	if ids["c"] {
		t.Fatalf("c matching start fingerprint must be schedulable")
	}
	if waveOf(r, "c") != 0 {
		t.Fatalf("c expected wave 0, got %d", waveOf(r, "c"))
	}
}

// Cycle error from scheduler must surface the concrete path.
func TestBuildRejectsCycle(t *testing.T) {
	nodes := []*model.Node{
		node("a", 1, []string{"b"}, nil, []string{"s0"}, "oa", true),
		node("b", 1, []string{"a"}, nil, []string{"s0"}, "ob", true),
	}
	_, err := Build(nodes, "s0", win60())
	if err == nil || !strings.Contains(err.Error(), "环") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}
