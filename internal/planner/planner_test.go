package planner

import (
	"testing"
	"time"

	"migrationplanner/internal/model"
)

func mkNode(id string, dur time.Duration, deps, res []string) model.Node {
	return model.Node{ID: id, DependsOn: deps, MutexResources: res, Duration: dur, Rollbackable: true}
}

func baseReq() model.PlanRequest {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	ws := time.Date(2026, 9, 22, 2, 0, 0, 0, loc)
	return model.PlanRequest{WindowStart: ws, WindowEnd: ws.Add(4 * time.Hour)}
}

func TestFindCycle_ReturnsConcretePath(t *testing.T) {
	nodes := map[string]model.Node{
		"a": mkNode("a", time.Minute, []string{"c"}, nil),
		"b": mkNode("b", time.Minute, []string{"a"}, nil),
		"c": mkNode("c", time.Minute, []string{"b"}, nil),
		"d": mkNode("d", time.Minute, nil, nil),
	}
	cyc := FindCycle(nodes)
	if cyc == nil {
		t.Fatal("expected a cycle")
	}
	if cyc[0] != cyc[len(cyc)-1] {
		t.Fatalf("cycle path must close on itself, got %v", cyc)
	}
	_, err := Plan("p", baseReq(), nodes, time.Now())
	if err == nil {
		t.Fatal("Plan must reject cyclic graph")
	}
	if ce, ok := err.(*CycleError); !ok {
		t.Fatalf("want *CycleError, got %T %v", err, err)
	} else if len(ce.Cycle) < 4 {
		t.Fatalf("concrete cycle too short: %v", ce.Cycle)
	}
}

func TestPlan_NoCycleWhenAcyclic(t *testing.T) {
	nodes := map[string]model.Node{
		"a": mkNode("a", time.Minute, nil, nil),
		"b": mkNode("b", time.Minute, []string{"a"}, nil),
	}
	if cyc := FindCycle(nodes); cyc != nil {
		t.Fatalf("unexpected cycle: %v", cyc)
	}
}

func waveOf(p *model.Plan, id string) int {
	for _, w := range p.Waves {
		for _, x := range w.NodeIDs {
			if x == id {
				return w.Index
			}
		}
	}
	return -1
}

func TestMutexResource_NoConcurrency(t *testing.T) {
	// a and b are independent but share a resource: they must end in
	// different waves even though both have zero prerequisites.
	nodes := map[string]model.Node{
		"a": mkNode("a", 10*time.Minute, nil, []string{"R"}),
		"b": mkNode("b", 10*time.Minute, nil, []string{"R"}),
		"c": mkNode("c", 10*time.Minute, nil, []string{"R"}),
	}
	p, err := Plan("p", baseReq(), nodes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	wa, wb, wc := waveOf(p, "a"), waveOf(p, "b"), waveOf(p, "c")
	if wa == wb || wb == wc || wa == wc {
		t.Fatalf("shared mutex resource scheduled concurrently: a=%d b=%d c=%d", wa, wb, wc)
	}
	// Stability: lexicographically smallest gets the earliest wave.
	if !(wa < wb && wb < wc) {
		t.Fatalf("expected stable ordering a<b<c, got a=%d b=%d c=%d", wa, wb, wc)
	}
}

func TestStableScheduling_SameInputSameOutput(t *testing.T) {
	nodes := map[string]model.Node{
		"zeta":  mkNode("zeta", 5*time.Minute, []string{"root"}, nil),
		"alpha": mkNode("alpha", 5*time.Minute, []string{"root"}, nil),
		"mid":   mkNode("mid", 5*time.Minute, []string{"root"}, []string{"X"}),
		"other": mkNode("other", 5*time.Minute, nil, []string{"X"}),
		"root":  mkNode("root", 5*time.Minute, nil, nil),
	}
	p1, err := Plan("p1", baseReq(), nodes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	p2, err := Plan("p2", baseReq(), nodes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Waves) != len(p2.Waves) {
		t.Fatalf("wave count not stable: %d vs %d", len(p1.Waves), len(p2.Waves))
	}
	for i := range p1.Waves {
		if len(p1.Waves[i].NodeIDs) != len(p2.Waves[i].NodeIDs) {
			t.Fatalf("wave %d size differs", i)
		}
		for j := range p1.Waves[i].NodeIDs {
			if p1.Waves[i].NodeIDs[j] != p2.Waves[i].NodeIDs[j] {
				t.Fatalf("wave %d order differs: %v vs %v", i, p1.Waves[i].NodeIDs, p2.Waves[i].NodeIDs)
			}
		}
	}
	// Intra-wave order sorted by ID.
	for _, w := range p1.Waves {
		for j := 1; j < len(w.NodeIDs); j++ {
			if w.NodeIDs[j-1] > w.NodeIDs[j] {
				t.Fatalf("wave %d not sorted: %v", w.Index, w.NodeIDs)
			}
		}
	}
}

func TestWindowEndpoints_ExactFitScheduled(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	ws := time.Date(2026, 9, 22, 2, 0, 0, 0, loc)
	req := model.PlanRequest{WindowStart: ws, WindowEnd: ws.Add(30 * time.Minute)}
	nodes := map[string]model.Node{
		"a": mkNode("a", 10*time.Minute, nil, nil),
		"b": mkNode("b", 10*time.Minute, []string{"a"}, nil),
		"c": mkNode("c", 10*time.Minute, []string{"b"}, nil),
	}
	p, err := Plan("p", req, nodes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Waves) != 3 {
		t.Fatalf("three 10m waves exactly fill a 30m window, got %d waves", len(p.Waves))
	}
	if len(p.Blocked) != 0 {
		t.Fatalf("exact-fit nodes must not be blocked: %+v", p.Blocked)
	}
	if !p.Waves[2].End.Equal(req.WindowEnd) {
		t.Fatalf("last wave end %v must equal window end %v (endpoint inclusive)", p.Waves[2].End, req.WindowEnd)
	}
}

func TestWindowOverflow_BlocksChain(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Tokyo")
	ws := time.Date(2026, 9, 22, 2, 0, 0, 0, loc)
	req := model.PlanRequest{WindowStart: ws, WindowEnd: ws.Add(25 * time.Minute)}
	nodes := map[string]model.Node{
		"a": mkNode("a", 10*time.Minute, nil, nil),
		"b": mkNode("b", 10*time.Minute, []string{"a"}, nil),
		"c": mkNode("c", 10*time.Minute, []string{"b"}, nil), // third wave ends at 30m > 25m
	}
	p, err := Plan("p", req, nodes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Waves) != 2 {
		t.Fatalf("only two 10m waves fit 25m, got %d", len(p.Waves))
	}
	blocked := map[string]bool{}
	for _, b := range p.Blocked {
		blocked[b.NodeID] = true
	}
	if !blocked["c"] {
		t.Fatalf("overflow node c must be blocked, got %+v", p.Blocked)
	}
}

func TestWindowInterpretedInTimezone(t *testing.T) {
	// "02:00 to 05:00" in Tokyo is UTC 17:00 previous day; check the wave
	// timestamps carry the requested zone, not local server time.
	loc, _ := time.LoadLocation("Asia/Tokyo")
	req := baseReq()
	nodes := map[string]model.Node{"a": mkNode("a", 30*time.Minute, nil, nil)}
	p, err := Plan("p", req, nodes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if p.Waves[0].Start.Location().String() != "Asia/Tokyo" {
		t.Fatalf("window not interpreted in requested timezone: %v", p.Waves[0].Start.Location())
	}
	if p.Waves[0].Start.Hour() != 2 {
		t.Fatalf("expected 02:00 Tokyo start, got %v", p.Waves[0].Start)
	}
	_ = loc
}

func TestFingerprintMismatch_BlocksSuccessorChain(t *testing.T) {
	req := baseReq()
	nodes := map[string]model.Node{
		"a": {ID: "a", Duration: 10 * time.Minute, Rollbackable: true, OutputFingerprint: "fp-A"},
		"b": {ID: "b", Duration: 10 * time.Minute, Rollbackable: true, DependsOn: []string{"a"}, InputFingerprint: "fp-WRONG", OutputFingerprint: "fp-B"},
		"c": {ID: "c", Duration: 10 * time.Minute, Rollbackable: true, DependsOn: []string{"b"}, OutputFingerprint: "fp-C"},
	}
	p, err := Plan("p", req, nodes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	blocked := map[string][]string{}
	for _, b := range p.Blocked {
		blocked[b.NodeID] = b.Reasons
	}
	if _, ok := blocked["b"]; !ok {
		t.Fatal("b must be blocked on fingerprint mismatch")
	}
	if _, ok := blocked["c"]; !ok {
		t.Fatal("whole successor chain (c) must block when b is blocked")
	}
	if waveOf(p, "a") < 0 {
		t.Fatal("a should still be schedulable")
	}
}

func TestMissingDependency_Rejected(t *testing.T) {
	nodes := map[string]model.Node{
		"a": mkNode("a", time.Minute, []string{"ghost"}, nil),
	}
	if _, err := Plan("p", baseReq(), nodes, time.Now()); err == nil {
		t.Fatal("expected validation error for unknown dependency")
	}
}
