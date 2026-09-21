package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"migplanner/internal/model"
	"migplanner/internal/runtime"
	"migplanner/internal/scheduler"
	"migplanner/internal/store"
)

//go:embed static/*
var staticFS embed.FS

type Server struct {
	store *store.Store
	mu    sync.Mutex
}

func New(st *store.Store) *Server { return &Server{store: st} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/nodes", s.handleSaveNode)
	mux.HandleFunc("DELETE /api/nodes", s.handleDeleteNode)
	mux.HandleFunc("POST /api/plans", s.handleCreatePlan)
	mux.HandleFunc("GET /api/plans/{id}/view", s.handleView)
	mux.HandleFunc("POST /api/plans/{id}/events", s.handleEvent)
	mux.HandleFunc("GET /api/plans/{id}/rollback", s.handleRollback)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	return logRequests(mux)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	data, err := s.store.RawLines()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	_, _ = w.Write(data)
}

type stateResp struct {
	Nodes    []*model.Node `json:"nodes"`
	Plans    []*model.Plan `json:"plans"`
	DataFile string        `json:"dataFile"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, stateResp{
		Nodes:    s.store.ListNodes(),
		Plans:    s.store.ListPlans(),
		DataFile: s.store.DataFile(),
	})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (s *Server) handleSaveNode(w http.ResponseWriter, r *http.Request) {
	var n model.Node
	if err := decode(r, &n); err != nil {
		writeErr(w, 400, "节点 JSON 无效: "+err.Error())
		return
	}
	n.ID = strings.TrimSpace(n.ID)
	if n.ID == "" {
		writeErr(w, 400, "节点 ID 必填")
		return
	}
	if n.Minutes < 0 {
		writeErr(w, 400, "预计时长不能为负")
		return
	}
	n.Prereqs = cleanList(n.Prereqs)
	n.Resources = cleanList(n.Resources)
	n.Inputs = cleanList(n.Inputs)
	if err := s.store.SaveNode(&n); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, n)
}

func cleanList(xs []string) []string {
	out := xs[:0]
	seen := map[string]bool{}
	for _, x := range xs {
		x = strings.TrimSpace(x)
		if x == "" || seen[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	return out
}

func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, 400, "缺少 id")
		return
	}
	if err := s.store.DeleteNode(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"deleted": id})
}

type createPlanReq struct {
	StartFingerprint string       `json:"startFingerprint"`
	Window           model.Window `json:"window"`
}

func (s *Server) handleCreatePlan(w http.ResponseWriter, r *http.Request) {
	var req createPlanReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "请求 JSON 无效: "+err.Error())
		return
	}
	req.StartFingerprint = strings.TrimSpace(req.StartFingerprint)
	if req.StartFingerprint == "" {
		writeErr(w, 400, "必须选择起始 schema 指纹")
		return
	}
	nodes := s.store.ListNodes()
	res, err := scheduler.Build(nodes, req.StartFingerprint, req.Window)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	plan := &model.Plan{
		ID:       "plan-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Created:  time.Now().UTC().Format(time.RFC3339Nano),
		StartFP:  req.StartFingerprint,
		Window:   req.Window,
		Nodes:    res.Nodes,
		Waves:    res.Waves,
		Blocked:  res.Blocked,
		NodeByID: map[string]*model.PlanNode{},
	}
	for _, pn := range plan.Nodes {
		plan.NodeByID[pn.ID] = pn
	}
	if err := s.store.CreatePlan(plan); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, plan)
}

func (s *Server) currentView(planID string, recovered bool) (*runtime.View, int, string) {
	plan := s.store.GetPlan(planID)
	if plan == nil {
		return nil, 404, "计划不存在: " + planID
	}
	return runtime.Project(plan, s.store.ExecEvents(planID), recovered), 0, ""
}

func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	recovered := r.URL.Query().Get("recovered") == "1"
	v, code, msg := s.currentView(id, recovered)
	if v == nil {
		writeErr(w, code, msg)
		return
	}
	writeJSON(w, 200, v)
}

type eventReq struct {
	NodeID            string              `json:"nodeId"`
	Type              model.ExecEventType `json:"type"`
	IdemKey           string              `json:"idemKey"`
	ActualFingerprint string              `json:"actualFingerprint"`
	Detail            string              `json:"detail"`
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req eventReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, "请求 JSON 无效: "+err.Error())
		return
	}
	req.NodeID = strings.TrimSpace(req.NodeID)
	req.IdemKey = strings.TrimSpace(req.IdemKey)
	if req.NodeID == "" || req.IdemKey == "" {
		writeErr(w, 400, "nodeId 与 idemKey 必填")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	plan := s.store.GetPlan(id)
	if plan == nil {
		writeErr(w, 404, "计划不存在")
		return
	}
	if plan.NodeByID[req.NodeID] == nil {
		writeErr(w, 400, "计划快照中不存在节点 "+req.NodeID)
		return
	}
	view := runtime.Project(plan, s.store.ExecEvents(id), false)
	nv := view.Nodes[req.NodeID]
	if err := runtime.ValidateTransition(nv.Status, req.Type); err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	if req.Type == model.EventStarted && nv.Status == model.StatusPending && !nv.Runnable {
		writeErr(w, 409, "节点尚不可运行："+nv.Reason)
		return
	}
	if nv.Blocked || nv.Handled {
		writeErr(w, 409, "节点处于阻塞/人工接管状态，除提交人工接管外不能记录其它执行事件："+nv.Reason)
		return
	}
	if req.Type == model.EventSucceeded && req.ActualFingerprint == "" {
		req.ActualFingerprint = plan.NodeByID[req.NodeID].Output
	}
	ev := &model.ExecEvent{
		PlanID:   id,
		NodeID:   req.NodeID,
		Type:     req.Type,
		IdemKey:  req.IdemKey,
		ActualFP: req.ActualFingerprint,
		Detail:   req.Detail,
	}
	if err := s.store.AppendExec(ev); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			writeErr(w, 409, err.Error())
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"stored": true, "event": ev})
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	node := r.URL.Query().Get("node")
	v, code, msg := s.currentView(id, false)
	if v == nil {
		writeErr(w, code, msg)
		return
	}
	rp, err := runtime.PlanRollback(v, node)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, rp)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			fmt.Printf("%s %s\n", r.Method, r.URL.Path)
		}
		h.ServeHTTP(w, r)
	})
}
