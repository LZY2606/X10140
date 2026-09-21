// Package web 提供迁移依赖规划器的 HTTP API 与页面。
package web

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"migrationplanner/internal/planner"
	"migrationplanner/internal/store"
)

//go:embed index.html
var indexHTML []byte

// Server HTTP 服务。
type Server struct {
	st  *store.Store
	mux *http.ServeMux
}

// New 创建服务并注册路由。
func New(st *store.Store) *Server {
	s := &Server{st: st, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /", s.handleIndex)
	s.mux.HandleFunc("GET /api/nodes", s.handleGetNodes)
	s.mux.HandleFunc("PUT /api/nodes", s.handlePutNodes)
	s.mux.HandleFunc("GET /api/plans", s.handleListPlans)
	s.mux.HandleFunc("POST /api/plans", s.handleCreatePlan)
	s.mux.HandleFunc("GET /api/events", s.handleListEvents)
	s.mux.HandleFunc("POST /api/events", s.handlePostEvent)
	s.mux.HandleFunc("POST /api/recover", s.handleRecover)
	s.mux.HandleFunc("POST /api/rollback", s.handleRollback)
	return s
}

// Handler 返回根 handler。
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func (s *Server) handleGetNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.st.LoadNodes()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"nodes": nodes})
}

func (s *Server) handlePutNodes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Nodes []planner.Node `json:"nodes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, err)
		return
	}
	seen := map[string]bool{}
	for _, n := range body.Nodes {
		if n.ID == "" {
			writeErr(w, errors.New("节点 ID 不能为空"))
			return
		}
		if seen[n.ID] {
			writeErr(w, fmt.Errorf("节点 ID %q 重复", n.ID))
			return
		}
		seen[n.ID] = true
	}
	if cyc := planner.FindCycle(body.Nodes); cyc != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"error": (&planner.CycleError{Path: cyc}).Error(),
			"cycle": cyc,
		})
		return
	}
	if err := s.st.SaveNodes(body.Nodes); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleListPlans(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"plans": s.st.Plans()})
}

func (s *Server) handleCreatePlan(w http.ResponseWriter, r *http.Request) {
	var req planner.PlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, err)
		return
	}
	nodes, err := s.st.LoadNodes()
	if err != nil {
		writeErr(w, err)
		return
	}
	planID := fmt.Sprintf("plan-%s", time.Now().UTC().Format("20060102-150405.000000000"))
	plan, err := planner.BuildPlan(nodes, req, planID, time.Now())
	if err != nil {
		var ce *planner.CycleError
		if errors.As(err, &ce) {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": ce.Error(), "cycle": ce.Path})
			return
		}
		writeErr(w, err)
		return
	}
	if err := s.st.AppendPlan(plan); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, plan)
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.st.Events(r.URL.Query().Get("plan_id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"events": events})
}

func (s *Server) handlePostEvent(w http.ResponseWriter, r *http.Request) {
	var e planner.Event
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		writeErr(w, err)
		return
	}
	if e.PlanID == "" || e.NodeID == "" {
		writeErr(w, errors.New("plan_id 与 node_id 不能为空"))
		return
	}
	switch e.Status {
	case planner.StatusPending, planner.StatusRunning, planner.StatusSuccess,
		planner.StatusFailed, planner.StatusRolledBack, planner.StatusNeedsManual:
	default:
		writeErr(w, fmt.Errorf("未知状态 %q", e.Status))
		return
	}
	if _, ok := s.st.Plan(e.PlanID); !ok {
		writeErr(w, fmt.Errorf("计划 %q 不存在", e.PlanID))
		return
	}
	created, err := s.st.AppendEvent(e)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"created": created})
}

func (s *Server) handleRecover(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PlanID string `json:"plan_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, err)
		return
	}
	plan, ok := s.st.Plan(body.PlanID)
	if !ok {
		writeErr(w, fmt.Errorf("计划 %q 不存在", body.PlanID))
		return
	}
	events, err := s.st.Events(body.PlanID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, planner.Recover(plan, events))
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PlanID string `json:"plan_id"`
		NodeID string `json:"node_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, err)
		return
	}
	plan, ok := s.st.Plan(body.PlanID)
	if !ok {
		writeErr(w, fmt.Errorf("计划 %q 不存在", body.PlanID))
		return
	}
	events, err := s.st.Events(body.PlanID)
	if err != nil {
		writeErr(w, err)
		return
	}
	rb, err := planner.BuildRollback(plan, events, body.NodeID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, rb)
}

func writeJSON(w http.ResponseWriter, v any) {
	writeJSONStatus(w, http.StatusOK, v)
}

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
}
