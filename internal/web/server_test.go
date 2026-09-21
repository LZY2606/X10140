package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"migplanner/internal/model"
	"migplanner/internal/store"
)

func newTestServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(st).Handler())
	return ts, func() { ts.Close(); st.Close() }
}

func do(t *testing.T, ts *httptest.Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, ts.URL+path, &buf)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func seedNode(t *testing.T, ts *httptest.Server, n model.Node) {
	t.Helper()
	if code, out := do(t, ts, "POST", "/api/nodes", n); code != 200 {
		t.Fatalf("save node %s: %d %v", n.ID, code, out)
	}
}

func TestHTTPCycleError(t *testing.T) {
	ts, cl := newTestServer(t)
	defer cl()
	seedNode(t, ts, model.Node{ID: "a", Prereqs: []string{"b"}, Minutes: 1, Inputs: []string{"s0"}, Output: "oa"})
	seedNode(t, ts, model.Node{ID: "b", Prereqs: []string{"a"}, Minutes: 1, Inputs: []string{"oa"}, Output: "ob"})
	code, out := do(t, ts, "POST", "/api/plans", map[string]any{
		"startFingerprint": "s0",
		"window":           map[string]string{"start": "2026-09-22T22:00", "end": "2026-09-23T01:00", "zone": "Asia/Tokyo"},
	})
	if code != 400 {
		t.Fatalf("want 400 for cycle, got %d: %v", code, out)
	}
	if !strings.Contains(asString(out["error"]), "环") {
		t.Fatalf("error must describe concrete cycle: %v", out["error"])
	}
}

func TestHTTPEndToEnd(t *testing.T) {
	ts, cl := newTestServer(t)
	defer cl()
	seedNode(t, ts, model.Node{ID: "a", Name: "A", Minutes: 10, Rollback: true, Inputs: []string{"s0"}, Output: "oa"})
	seedNode(t, ts, model.Node{ID: "b", Name: "B", Minutes: 10, Rollback: true, Prereqs: []string{"a"}, Inputs: []string{"oa"}, Output: "ob"})

	code, out := do(t, ts, "POST", "/api/plans", map[string]any{
		"startFingerprint": "s0",
		"window":           map[string]string{"start": "2026-09-22T22:00", "end": "2026-09-23T00:00", "zone": "Asia/Tokyo"},
	})
	if code != 201 {
		t.Fatalf("create plan: %d %v", code, out)
	}
	planID := asString(out["id"])
	if planID == "" {
		t.Fatal("missing plan id")
	}

	// Start a, then simulate duplicate submission of the same result.
	if code, _ := do(t, ts, "POST", "/api/plans/"+planID+"/events", map[string]any{
		"nodeId": "a", "type": "started", "idemKey": "a-start-1",
	}); code != 201 {
		t.Fatalf("start a: %d", code)
	}
	if code, _ := do(t, ts, "POST", "/api/plans/"+planID+"/events", map[string]any{
		"nodeId": "a", "type": "started", "idemKey": "a-start-1",
	}); code != 409 {
		t.Fatalf("duplicate idempotency key must be 409, got %d", code)
	}
	// b cannot start before a succeeds.
	if code, out := do(t, ts, "POST", "/api/plans/"+planID+"/events", map[string]any{
		"nodeId": "b", "type": "started", "idemKey": "b-start-bad",
	}); code != 409 || !strings.Contains(asString(out["error"]), "等待前置完成") {
		t.Fatalf("b must not start while a is running: %d %v", code, out)
	}

	// a succeeds with matching fingerprint -> b becomes runnable.
	if code, _ := do(t, ts, "POST", "/api/plans/"+planID+"/events", map[string]any{
		"nodeId": "a", "type": "succeeded", "idemKey": "a-ok", "actualFingerprint": "oa",
	}); code != 201 {
		t.Fatalf("succeed a: %d", code)
	}
	view := getView(t, ts, planID, false)
	if !view["nodes"].(map[string]any)["b"].(map[string]any)["runnable"].(bool) {
		t.Fatal("b should be runnable after a succeeds with matching fp")
	}

	// Recovery projection: a is skipped (success+matching), not runnable.
	rec := getView(t, ts, planID, true)
	na := rec["nodes"].(map[string]any)["a"].(map[string]any)
	if na["runnable"].(bool) || na["blocked"].(bool) {
		t.Fatal("successful matching a must be skipped, not runnable/blocked")
	}
	if skip := rec["recovery"].(map[string]any)["skipped"].([]any); len(skip) != 1 || skip[0] != "a" {
		t.Fatalf("a must appear in skipped list, got %v", skip)
	}
}

func TestHTTPFingerprintDrift(t *testing.T) {
	ts, cl := newTestServer(t)
	defer cl()
	seedNode(t, ts, model.Node{ID: "a", Minutes: 5, Rollback: true, Inputs: []string{"s0"}, Output: "oa"})
	seedNode(t, ts, model.Node{ID: "b", Minutes: 5, Rollback: true, Prereqs: []string{"a"}, Inputs: []string{"oa"}, Output: "ob"})
	_, out := do(t, ts, "POST", "/api/plans", map[string]any{
		"startFingerprint": "s0",
		"window":           map[string]string{"start": "2026-09-22T22:00", "end": "2026-09-23T00:00", "zone": "Asia/Tokyo"},
	})
	planID := asString(out["id"])
	do(t, ts, "POST", "/api/plans/"+planID+"/events", map[string]any{
		"nodeId": "a", "type": "started", "idemKey": "k1"})
	do(t, ts, "POST", "/api/plans/"+planID+"/events", map[string]any{
		"nodeId": "a", "type": "succeeded", "idemKey": "k2", "actualFingerprint": "WRONG"})
	view := getView(t, ts, planID, true)
	nodes := view["nodes"].(map[string]any)
	if !nodes["a"].(map[string]any)["blocked"].(bool) {
		t.Fatal("drifted a must be blocked")
	}
	if !nodes["b"].(map[string]any)["blocked"].(bool) {
		t.Fatal("successor b must be blocked after drift")
	}
	drifted := view["recovery"].(map[string]any)["drifted"].([]any)
	if len(drifted) != 1 || drifted[0] != "a" {
		t.Fatalf("drifted report wrong: %v", drifted)
	}

	// Rollback on the drifted (manual) node is refused directly.
	code, rb := do(t, ts, "GET", "/api/plans/"+planID+"/rollback?node=a", nil)
	if code == 200 {
		t.Fatalf("manual node rollback must not produce normal plan: %v", rb)
	}
}

func TestHomepageServesPlanner(t *testing.T) {
	ts, cl := newTestServer(t)
	defer cl()
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "迁移依赖规划器") {
		t.Fatalf("homepage must contain title, got: %q", string(body)[:200])
	}
}

func getView(t *testing.T, ts *httptest.Server, id string, recovered bool) map[string]any {
	t.Helper()
	suffix := ""
	if recovered {
		suffix = "?recovered=1"
	}
	res, err := http.Get(ts.URL + "/api/plans/" + id + "/view" + suffix)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
