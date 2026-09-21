// Package web exposes the planner as an HTTP server with a single-page UI.
package web

import (
	"embed"
	"encoding/json"
	"net/http"
	"time"

	"migrationplanner/internal/execute"
	"migrationplanner/internal/model"
	"migrationplanner/internal/planner"
	"migrationplanner/internal/store"
)

//go:embed all:assets
var assets embed.FS

// Server holds dependencies and the currently selected plan.
type Server struct {
	store *store.Store
	now   func() time.Time
}

func NewServer(s *store.Store) *Server {
	return &Server{store: s, now: time.Now}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/nodes", s.handleUpsertNode)
	mux.HandleFunc("POST /api/nodes/delete", s.handleDeleteNode)
	mux.HandleFunc("POST /api/plans", s.handleCreatePlan)
	mux.HandleFunc("POST /api/execute", s.handleExecute)
	mux.HandleFunc("POST /api/crash", s.handleCrash)
	mux.HandleFunc("POST /api/rollback/plan", s.handleRollbackPlan)
	mux.HandleFunc("POST /api/rollback/step", s.handleRollbackStep)
	return logging(mux)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := assets.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

type nodeDTO struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	DependsOn         []string `json:"dependsOn"`
	MutexResources    []string `json:"mutexResources"`
	DurationMin       float64  `json:"durationMin"`
	Rollbackable      bool     `json:"rollbackable"`
	InputFingerprint  string   `json:"inputFingerprint"`
	OutputFingerprint string   `json:"outputFingerprint"`
}

func (s *Server) handleUpsertNode(w http.ResponseWriter, r *http.Request) {
	var d nodeDTO
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if d.ID == "" {
		writeErr(w, 400, "节点 ID 不能为空")
		return
	}
	if d.DurationMin < 0 {
		writeErr(w, 400, "预计时长不能为负")
		return
	}
	n := model.Node{
		ID:                d.ID,
		Name:              d.Name,
		DependsOn:         nonNil(d.DependsOn),
		MutexResources:    nonNil(d.MutexResources),
		Duration:          time.Duration(d.DurationMin * float64(time.Minute)),
		Rollbackable:      d.Rollbackable,
		InputFingerprint:  d.InputFingerprint,
		OutputFingerprint: d.OutputFingerprint,
		UpdatedAt:         s.now(),
	}
	if _, _, err := s.store.Append(model.Event{Type: model.EvNodeUpserted, Node: &n}, ""); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.respondState(w)
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	out := xs[:0]
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if _, _, err := s.store.Append(model.Event{Type: model.EvNodeDeleted, DeletedNodeID: req.ID}, ""); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.respondState(w)
}

type planDTO struct {
	StartFingerprint string `json:"startFingerprint"`
	WindowStart      string `json:"windowStart"` // "2006-01-02T15:04"
	WindowEnd        string `json:"windowEnd"`
	Timezone         string `json:"timezone"` // IANA name, e.g. Asia/Tokyo
}

func (s *Server) handleCreatePlan(w http.ResponseWriter, r *http.Request) {
	var d planDTO
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	loc, err := time.LoadLocation(d.Timezone)
	if err != nil {
		writeErr(w, 400, "无法识别的时区: "+d.Timezone)
		return
	}
	ws, err := time.ParseInLocation("2006-01-02T15:04", d.WindowStart, loc)
	if err != nil {
		writeErr(w, 400, "窗口开始时间格式应为 YYYY-MM-DDTHH:MM")
		return
	}
	we, err := time.ParseInLocation("2006-01-02T15:04", d.WindowEnd, loc)
	if err != nil {
		writeErr(w, 400, "窗口结束时间格式应为 YYYY-MM-DDTHH:MM")
		return
	}
	req := model.PlanRequest{StartFingerprint: d.StartFingerprint, WindowStart: ws, WindowEnd: we}
	nodes := s.store.Nodes()
	plan, err := planner.Plan(s.newPlanID(), req, nodes, s.now())
	if err != nil {
		if ce, ok := err.(*planner.CycleError); ok {
			writeJSON(w, map[string]any{"cycle": ce.Cycle, "error": ce.Error()})
			return
		}
		writeErr(w, 400, err.Error())
		return
	}
	if _, _, err := s.store.Append(model.Event{Type: model.EvPlanCreated, PlanID: plan.ID, Plan: plan}, ""); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.respondState(w)
}

func (s *Server) handleExecute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlanID            string `json:"planId"`
		NodeID            string `json:"nodeId"`
		Action            string `json:"action"` // start | succeed | fail
		OutputFingerprint string `json:"outputFingerprint"`
		Detail            string `json:"detail"`
		IdempotencyKey    string `json:"idempotencyKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if ev, hit := s.store.LookupIdempotent(req.IdempotencyKey); hit && req.IdempotencyKey != "" && ev.PlanID == req.PlanID {
		s.respondStateSelectedIdem(w, req.PlanID, ev.IdempotencyKey, true)
		return
	}
	plan, ok := s.store.Plan(req.PlanID)
	if !ok {
		writeErr(w, 404, "计划不存在")
		return
	}
	n, ok := plan.NodeSnapshot[req.NodeID]
	if !ok {
		writeErr(w, 400, "节点不在计划快照中")
		return
	}
	states := derive(plan, s.store.EventsForPlan(req.PlanID))
	st := states.Nodes[req.NodeID]

	evType := ""
	switch req.Action {
	case "start":
		if can, reason := execute.CanStart(plan, states, req.NodeID); !can {
			writeErr(w, 409, reason)
			return
		}
		evType = model.EvNodeStarted
	case "succeed":
		if st.Status != model.StatusRunning {
			writeErr(w, 409, "节点当前不是运行中，无法记录成功（崩溃后请先重新启动）")
			return
		}
		fp := req.OutputFingerprint
		if fp == "" {
			fp = n.OutputFingerprint // default to declared fingerprint
		}
		req.OutputFingerprint = fp
		evType = model.EvNodeSucceeded
	case "fail":
		if st.Status != model.StatusRunning {
			writeErr(w, 409, "节点当前不是运行中，无法记录失败")
			return
		}
		evType = model.EvNodeFailed
	default:
		writeErr(w, 400, "未知 action: "+req.Action)
		return
	}

	ev := model.Event{
		Type:              evType,
		PlanID:            req.PlanID,
		NodeID:            req.NodeID,
		OutputFingerprint: req.OutputFingerprint,
		Detail:            req.Detail,
		Duration:          n.Duration,
	}
	existing, dup, err := s.store.Append(ev, req.IdempotencyKey)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if dup {
		s.respondStateSelectedIdem(w, req.PlanID, existing.IdempotencyKey, true)
		return
	}
	s.respondStateSelected(w, req.PlanID)
}

func (s *Server) handleCrash(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlanID         string `json:"planId"`
		IdempotencyKey string `json:"idempotencyKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if ev, hit := s.store.LookupIdempotent(req.IdempotencyKey); hit && req.IdempotencyKey != "" && ev.PlanID == req.PlanID {
		s.respondStateSelectedIdem(w, req.PlanID, ev.IdempotencyKey, true)
		return
	}
	if _, ok := s.store.Plan(req.PlanID); !ok {
		writeErr(w, 404, "计划不存在")
		return
	}
	existing, dup, err := s.store.Append(model.Event{Type: model.EvCrashSimulated, PlanID: req.PlanID, Detail: "模拟服务进程崩溃"}, req.IdempotencyKey)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if dup {
		s.respondStateSelectedIdem(w, req.PlanID, existing.IdempotencyKey, true)
		return
	}
	s.respondStateSelected(w, req.PlanID)
}

func (s *Server) handleRollbackPlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlanID string `json:"planId"`
		NodeID string `json:"nodeId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	plan, ok := s.store.Plan(req.PlanID)
	if !ok {
		writeErr(w, 404, "计划不存在")
		return
	}
	states := derive(plan, s.store.EventsForPlan(req.PlanID))
	rp, err := execute.BuildRollbackPlan(plan, states, req.NodeID)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, rp)
}

func (s *Server) handleRollbackStep(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlanID         string `json:"planId"`
		FailedNodeID   string `json:"failedNodeId"`
		NodeID         string `json:"nodeId"`
		IdempotencyKey string `json:"idempotencyKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if ev, hit := s.store.LookupIdempotent(req.IdempotencyKey); hit && req.IdempotencyKey != "" && ev.PlanID == req.PlanID {
		s.respondStateSelectedIdem(w, req.PlanID, ev.IdempotencyKey, true)
		return
	}
	plan, ok := s.store.Plan(req.PlanID)
	if !ok {
		writeErr(w, 404, "计划不存在")
		return
	}
	states := derive(plan, s.store.EventsForPlan(req.PlanID))
	rp, err := execute.BuildRollbackPlan(plan, states, req.FailedNodeID)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// The next executable step must be the first step still in 成功 state.
	next := ""
	for _, step := range rp.Steps {
		if states.Nodes[step.NodeID] != nil && states.Nodes[step.NodeID].Status == model.StatusSucceeded {
			next = step.NodeID
			break
		}
	}
	if next == "" {
		writeErr(w, 409, "回滚计划中没有待执行步骤（可能已全部回滚，或在不可回滚边界处停止）")
		return
	}
	if next != req.NodeID {
		writeErr(w, 409, "必须按逆序回滚，下一个应回滚的节点是 "+next)
		return
	}
	ev := model.Event{Type: model.EvNodeRolledBack, PlanID: req.PlanID, NodeID: req.NodeID, Detail: "按回滚计划逆序回滚（失败节点 " + req.FailedNodeID + "）"}
	existing, dup, err := s.store.Append(ev, req.IdempotencyKey)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if dup {
		s.respondStateSelectedIdem(w, req.PlanID, existing.IdempotencyKey, true)
		return
	}
	s.respondStateSelected(w, req.PlanID)
}

func derive(plan *model.Plan, evs []model.Event) *execute.States {
	st := execute.Replay(plan.ID, evs)
	execute.Derive(plan, st)
	return st
}
