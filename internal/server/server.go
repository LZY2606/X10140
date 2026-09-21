// Package server 提供迁移依赖规划器的 HTTP API 与页面。
package server

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"migrationplanner/internal/planner"
	"migrationplanner/internal/store"
)

//go:embed static
var staticFS embed.FS

// Server 持有存储与路由。
type Server struct {
	st  *store.Store
	mux *http.ServeMux
}

// New 创建服务。
func New(st *store.Store) *Server {
	s := &Server{st: st, mux: http.NewServeMux()}
	static, _ := fs.Sub(staticFS, "static")
	s.mux.Handle("GET /", http.FileServer(http.FS(static)))
	s.mux.HandleFunc("GET /api/state", s.handleState)
	s.mux.HandleFunc("PUT /api/graph", s.handleSaveGraph)
	s.mux.HandleFunc("POST /api/plans", s.handleCreatePlan)
	s.mux.HandleFunc("POST /api/runs", s.handleCreateRun)
	s.mux.HandleFunc("POST /api/runs/{id}/events", s.handleAddEvent)
	s.mux.HandleFunc("GET /api/runs/{id}/rollback", s.handleRollback)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

type runView struct {
	*store.Run
	Recovery *planner.Recovery `json:"recovery"`
}

func (s *Server) runViews() []runView {
	views := []runView{}
	for _, run := range s.st.Runs {
		v := runView{Run: run}
		if p := s.st.PlanByID(run.PlanID); p != nil {
			v.Recovery = planner.Recover(p, run.Events)
		}
		views = append(views, v)
	}
	return views
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes": s.st.Graph,
		"plans": s.st.Plans,
		"runs":  s.runViews(),
	})
}

func (s *Server) handleSaveGraph(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Nodes []planner.Node `json:"nodes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if err := s.st.SaveGraph(body.Nodes); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(body.Nodes)})
}

func (s *Server) handleCreatePlan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		StartSchema string           `json:"start_schema"`
		Timezone    string           `json:"timezone"`
		Windows     []planner.Window `json:"windows"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if body.Timezone == "" {
		body.Timezone = "Asia/Shanghai"
	}
	plan, err := planner.Build(s.st.Graph, body.StartSchema, body.Timezone, body.Windows, time.Now())
	if err != nil {
		var cyc *planner.CycleError
		if errors.As(err, &cyc) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": cyc.Error(), "cycle": cyc.Path})
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := s.st.AddPlan(plan)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, planWithID(plan, id))
}

func planWithID(p *planner.Plan, id string) *planner.Plan {
	p.ID = id
	return p
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PlanID string `json:"plan_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if s.st.PlanByID(body.PlanID) == nil {
		writeErr(w, http.StatusNotFound, "计划 "+body.PlanID+" 不存在")
		return
	}
	run, err := s.st.AddRun(body.PlanID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleAddEvent(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	var ev planner.Event
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	added, err := s.st.AddEvent(runID, ev)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	run := s.st.RunByID(runID)
	var rec *planner.Recovery
	if p := s.st.PlanByID(run.PlanID); p != nil {
		rec = planner.Recover(p, run.Events)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"added":     added,
		"duplicate": !added,
		"recovery":  rec,
	})
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	nodeID := r.URL.Query().Get("node")
	run := s.st.RunByID(runID)
	if run == nil {
		writeErr(w, http.StatusNotFound, "执行 "+runID+" 不存在")
		return
	}
	plan := s.st.PlanByID(run.PlanID)
	if plan == nil {
		writeErr(w, http.StatusNotFound, "计划 "+run.PlanID+" 不存在")
		return
	}
	rec := planner.Recover(plan, run.Events)
	res := planner.RollbackPlan(plan, nodeID, rec)
	writeJSON(w, http.StatusOK, res)
}

// 静态页面路径兜底：/index.html
var _ = strings.TrimSpace
