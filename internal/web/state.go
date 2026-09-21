package web

import (
	"net/http"
	"sync/atomic"
	"time"

	"migrationplanner/internal/model"
)

var planCounter uint64

func (s *Server) newPlanID() string {
	n := atomic.AddUint64(&planCounter, 1)
	return "plan-" + s.now().Format("20060102T150405") + "-" + itoa(n)
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

type planSummary struct {
	ID          string              `json:"id"`
	RequestedAt time.Time           `json:"requestedAt"`
	Request     model.PlanRequest   `json:"request"`
	WaveCount   int                 `json:"waveCount"`
	Blocked     []model.BlockedNode `json:"blocked"`
}

type stateResponse struct {
	Nodes          map[string]model.Node `json:"nodes"`
	Plans          []planSummary         `json:"plans"`
	SelectedPlanID string                `json:"selectedPlanId"`
	Plan           *model.Plan           `json:"plan"`
	States         map[string]any        `json:"states"`
	Events         []model.Event         `json:"events"`
	IdempotentHit  bool                  `json:"idempotentHit"`
	IdempotencyKey string                `json:"idempotencyKey,omitempty"`
	ServerTime     time.Time             `json:"serverTime"`
}

func (s *Server) buildState(selectedID string, idemKey string, idemHit bool) stateResponse {
	nodes := s.store.Nodes()
	plans := s.store.Plans()
	summaries := make([]planSummary, 0, len(plans))
	for _, p := range plans {
		summaries = append(summaries, planSummary{
			ID: p.ID, RequestedAt: p.RequestedAt, Request: p.Request,
			WaveCount: len(p.Waves), Blocked: p.Blocked,
		})
	}
	if selectedID == "" && len(plans) > 0 {
		selectedID = plans[0].ID
	}
	resp := stateResponse{
		Nodes: nodes, Plans: summaries, SelectedPlanID: selectedID,
		IdempotentHit: idemHit, IdempotencyKey: idemKey, ServerTime: s.now(),
	}
	if selectedID != "" {
		if plan, ok := s.store.Plan(selectedID); ok {
			resp.Plan = plan
			st := derive(plan, s.store.EventsForPlan(selectedID))
			resp.States = map[string]any{}
			for id, ns := range st.Nodes {
				resp.States[id] = ns
			}
			resp.Events = s.store.EventsForPlan(selectedID)
		}
	}
	return resp
}

func (s *Server) respondState(w http.ResponseWriter) {
	writeJSON(w, s.buildState("", "", false))
}

func (s *Server) respondStateSelected(w http.ResponseWriter, planID string) {
	writeJSON(w, s.buildState(planID, "", false))
}

func (s *Server) respondStateSelectedIdem(w http.ResponseWriter, planID, key string, hit bool) {
	writeJSON(w, s.buildState(planID, key, hit))
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.buildState(r.URL.Query().Get("plan"), "", false))
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}
