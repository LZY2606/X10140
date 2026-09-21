package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"migrationplanner/internal/model"
	"migrationplanner/internal/store"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(s)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return srv, ts
}

func postJSON(t *testing.T, ts *httptest.Server, path string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	res, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

func upsertNode(t *testing.T, ts *httptest.Server, d map[string]any) {
	t.Helper()
	status, body := postJSON(t, ts, "/api/nodes", d)
	if status != 200 {
		t.Fatalf("upsert node failed: %d %v", status, body)
	}
}

func nodeBody(id string, dur float64, deps []string, rollback bool, in, out string) map[string]any {
	return map[string]any{
		"id": id, "name": id, "dependsOn": deps, "mutexResources": []string{},
		"durationMin": dur, "rollbackable": rollback,
		"inputFingerprint": in, "outputFingerprint": out,
	}
}

func TestIndexPage_ShowsTitle(t *testing.T) {
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "迁移依赖规划器") {
		t.Fatal("index page must display 迁移依赖规划器")
	}
}

func createPlan(t *testing.T, ts *httptest.Server, start string, mins float64) (int, map[string]any) {
	t.Helper()
	loc, _ := time.LoadLocation("Asia/Tokyo")
	ws := time.Date(2026, 9, 22, 2, 0, 0, 0, loc)
	we := ws.Add(time.Duration(mins * float64(time.Minute)))
	return postJSON(t, ts, "/api/plans", map[string]any{
		"startFingerprint": start,
		"windowStart":      ws.Format("2006-01-02T15:04"),
		"windowEnd":        we.Format("2006-01-02T15:04"),
		"timezone":         "Asia/Tokyo",
	})
}

func planIDFromState(body map[string]any) string {
	if p, ok := body["plan"].(map[string]any); ok && p != nil {
		return p["id"].(string)
	}
	return ""
}

func TestAPI_CycleRejectedWithConcretePath(t *testing.T) {
	_, ts := newTestServer(t)
	upsertNode(t, ts, nodeBody("a", 5, []string{"c"}, true, "", "fa"))
	upsertNode(t, ts, nodeBody("b", 5, []string{"a"}, true, "fa", "fb"))
	upsertNode(t, ts, nodeBody("c", 5, []string{"b"}, true, "fb", "fc"))

	status, body := createPlan(t, ts, "", 120)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	cyc, ok := body["cycle"].([]any)
	if !ok || len(cyc) < 4 {
		t.Fatalf("expected concrete cycle path, got %v", body)
	}
	if cyc[0] != cyc[len(cyc)-1] {
		t.Fatalf("cycle must close: %v", cyc)
	}
}

func execEvent(t *testing.T, ts *httptest.Server, body map[string]any) (int, map[string]any) {
	return postJSON(t, ts, "/api/execute", body)
}

func TestAPI_DriftBlocksSuccessorChain(t *testing.T) {
	_, ts := newTestServer(t)
	upsertNode(t, ts, nodeBody("a", 5, nil, true, "", "fp-A"))
	upsertNode(t, ts, nodeBody("b", 5, []string{"a"}, true, "fp-A", "fp-B"))
	upsertNode(t, ts, nodeBody("c", 5, []string{"b"}, true, "fp-B", "fp-C"))

	_, body := createPlan(t, ts, "", 120)
	pid := planIDFromState(body)
	if pid == "" {
		t.Fatal("no plan id")
	}

	if st, _ := execEvent(t, ts, map[string]any{"planId": pid, "nodeId": "a", "action": "start"}); st != 200 {
		t.Fatal("start a failed")
	}
	if st, b := execEvent(t, ts, map[string]any{"planId": pid, "nodeId": "a", "action": "succeed", "outputFingerprint": "fp-DRIFT"}); st != 200 {
		t.Fatalf("succeed a: %d %v", st, b)
	}

	// b must now refuse to start.
	st, b := execEvent(t, ts, map[string]any{"planId": pid, "nodeId": "b", "action": "start"})
	if st != 409 {
		t.Fatalf("expected 409 chain block, got %d %v", st, b)
	}
	if !strings.Contains(b["error"].(string), "人工接管") {
		t.Fatalf("block reason should mention manual takeover, got %v", b)
	}

	// A state fetch must show a as 需要人工接管 due to drift.
	res, err := http.Get(ts.URL + "/api/state?plan=" + pid)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var state map[string]any
	json.NewDecoder(res.Body).Decode(&state)
	states := state["states"].(map[string]any)
	a := states["a"].(map[string]any)
	if a["status"] != model.StatusManual || a["drift"] != true {
		t.Fatalf("a should be manual+drift, got %v", a)
	}
	c := states["c"].(map[string]any)
	if c["status"] != model.StatusManual {
		t.Fatalf("transitive successor c should be blocked/manual, got %v", c["status"])
	}
}

func TestAPI_ExecuteIdempotentDuplicateRecordedOnce(t *testing.T) {
	_, ts := newTestServer(t)
	upsertNode(t, ts, nodeBody("a", 5, nil, true, "", "fp-A"))
	_, body := createPlan(t, ts, "", 120)
	pid := planIDFromState(body)

	payload := map[string]any{"planId": pid, "nodeId": "a", "action": "start", "idempotencyKey": "same-key"}
	if st, b := execEvent(t, ts, payload); st != 200 {
		t.Fatalf("first start: %d %v", st, b)
	}
	st, b := execEvent(t, ts, payload)
	if st != 200 {
		t.Fatalf("duplicate start: %d", st)
	}
	if b["idempotentHit"] != true {
		t.Fatalf("duplicate must report idempotentHit, got %v", b)
	}

	res, err := http.Get(ts.URL + "/api/state?plan=" + pid)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var state map[string]any
	json.NewDecoder(res.Body).Decode(&state)
	evs := state["events"].([]any)
	var starts int
	for _, e := range evs {
		if e.(map[string]any)["type"] == model.EvNodeStarted {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("duplicate idempotent submission must record only one start event, got %d", starts)
	}
}

func TestAPI_CrashRecoverySkipsMatchingSuccess(t *testing.T) {
	_, ts := newTestServer(t)
	upsertNode(t, ts, nodeBody("a", 5, nil, true, "", "fp-A"))
	upsertNode(t, ts, nodeBody("b", 5, []string{"a"}, true, "fp-A", "fp-B"))
	_, body := createPlan(t, ts, "", 120)
	pid := planIDFromState(body)

	execEvent(t, ts, map[string]any{"planId": pid, "nodeId": "a", "action": "start"})
	execEvent(t, ts, map[string]any{"planId": pid, "nodeId": "a", "action": "succeed", "outputFingerprint": "fp-A"})
	execEvent(t, ts, map[string]any{"planId": pid, "nodeId": "b", "action": "start"})
	postJSON(t, ts, "/api/crash", map[string]any{"planId": pid, "idempotencyKey": "crash-1"})

	res, err := http.Get(ts.URL + "/api/state?plan=" + pid)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var state map[string]any
	json.NewDecoder(res.Body).Decode(&state)
	states := state["states"].(map[string]any)
	a := states["a"].(map[string]any)
	b := states["b"].(map[string]any)
	if a["status"] != model.StatusSucceeded {
		t.Fatal("a stays succeeded after recovery")
	}
	if b["interrupted"] != true || b["status"] != model.StatusRunning {
		t.Fatalf("b must be interrupted running, got %v", b)
	}

	// Restarting b after crash is allowed; a cannot be re-run.
	if st, b2 := execEvent(t, ts, map[string]any{"planId": pid, "nodeId": "b", "action": "start"}); st != 200 {
		t.Fatalf("restart interrupted b should succeed: %d %v", st, b2)
	}
	if st, _ := execEvent(t, ts, map[string]any{"planId": pid, "nodeId": "a", "action": "start"}); st != 409 {
		t.Fatalf("matched-success a must not be re-run, got status %d", st)
	}
}

func TestAPI_RollbackBoundaryStopsPlan(t *testing.T) {
	_, ts := newTestServer(t)
	upsertNode(t, ts, nodeBody("a", 5, nil, true, "", "fp-A"))
	upsertNode(t, ts, nodeBody("b", 5, []string{"a"}, false, "fp-A", "fp-B")) // not rollbackable
	upsertNode(t, ts, nodeBody("c", 5, []string{"b"}, true, "fp-B", "fp-C"))
	_, body := createPlan(t, ts, "", 120)
	pid := planIDFromState(body)

	for _, step := range [][2]string{{"a", "fp-A"}, {"b", "fp-B"}} {
		execEvent(t, ts, map[string]any{"planId": pid, "nodeId": step[0], "action": "start"})
		execEvent(t, ts, map[string]any{"planId": pid, "nodeId": step[0], "action": "succeed", "outputFingerprint": step[1]})
	}
	execEvent(t, ts, map[string]any{"planId": pid, "nodeId": "c", "action": "start"})
	execEvent(t, ts, map[string]any{"planId": pid, "nodeId": "c", "action": "fail", "detail": "boom"})

	st, b := postJSON(t, ts, "/api/rollback/plan", map[string]any{"planId": pid, "nodeId": "c"})
	if st != 200 {
		t.Fatalf("rollback plan: %d %v", st, b)
	}
	if b["complete"] != false {
		t.Fatalf("plan must be incomplete at b boundary, got %v", b["complete"])
	}
	steps := b["steps"].([]any)
	if len(steps) != 0 {
		t.Fatalf("no steps may be emitted before the non-rollbackable boundary b, got %v", steps)
	}
	if !strings.Contains(b["boundary"].(string), "b") {
		t.Fatalf("boundary must name node b, got %v", b["boundary"])
	}
}

func TestAPI_MutexResourceSerializedAcrossWaves(t *testing.T) {
	_, ts := newTestServer(t)
	upsertNode(t, ts, map[string]any{"id": "x", "name": "x", "dependsOn": []string{}, "mutexResources": []string{"T"}, "durationMin": 10, "rollbackable": true})
	upsertNode(t, ts, map[string]any{"id": "y", "name": "y", "dependsOn": []string{}, "mutexResources": []string{"T"}, "durationMin": 10, "rollbackable": true})
	_, body := createPlan(t, ts, "", 120)
	plan := body["plan"].(map[string]any)
	waves := plan["waves"].([]any)
	if len(waves) != 2 {
		t.Fatalf("two mutex users must occupy two waves, got %d", len(waves))
	}
	w0 := waves[0].(map[string]any)["nodeIds"].([]any)
	w1 := waves[1].(map[string]any)["nodeIds"].([]any)
	if len(w0) != 1 || w0[0] != "x" || len(w1) != 1 || w1[0] != "y" {
		t.Fatalf("stable ordering should be x then y, got %v / %v", w0, w1)
	}
}
