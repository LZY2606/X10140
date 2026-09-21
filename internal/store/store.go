// Package store 提供追加式 JSONL 持久化：每次规划与执行事件都追加保存，
// 历史记录不会被后续修改改写；执行事件按幂等键去重。
package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"migrationplanner/internal/planner"
)

// Run 是一次执行，引用某份历史计划。
type Run struct {
	ID        string          `json:"id"`
	PlanID    string          `json:"plan_id"`
	CreatedAt time.Time       `json:"created_at"`
	Events    []planner.Event `json:"events"`
}

type record struct {
	Kind  string         `json:"kind"` // graph | plan | run | event
	Nodes []planner.Node `json:"nodes,omitempty"`
	Plan  *planner.Plan  `json:"plan,omitempty"`
	Run   *Run           `json:"run,omitempty"`
	RunID string         `json:"run_id,omitempty"`
	Event *planner.Event `json:"event,omitempty"`
}

// Store 是追加式存储。
type Store struct {
	mu     sync.Mutex
	path   string
	Graph  []planner.Node
	Plans  []*planner.Plan
	Runs   []*Run
	runIdx map[string]*Run
	keys   map[string]bool // runID + "/" + event.Key
}

// Open 打开（或创建）数据目录并重放全部历史记录。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		path:   filepath.Join(dir, "store.jsonl"),
		runIdx: map[string]*Run{},
		keys:   map[string]bool{},
	}
	f, err := os.Open(s.path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("历史记录损坏: %w", err)
		}
		s.apply(rec)
	}
	return s, sc.Err()
}

func (s *Store) apply(rec record) {
	switch rec.Kind {
	case "graph":
		s.Graph = rec.Nodes
	case "plan":
		s.Plans = append(s.Plans, rec.Plan)
	case "run":
		s.Runs = append(s.Runs, rec.Run)
		s.runIdx[rec.Run.ID] = rec.Run
		for _, ev := range rec.Run.Events {
			s.keys[rec.Run.ID+"/"+ev.Key] = true
		}
	case "event":
		if run, ok := s.runIdx[rec.RunID]; ok {
			run.Events = append(run.Events, *rec.Event)
			s.keys[rec.RunID+"/"+rec.Event.Key] = true
		}
	}
}

func (s *Store) append(rec record) error {
	rec.Kind = rec.Kind // no-op, clarity
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	s.apply(rec)
	return nil
}

// SaveGraph 保存当前节点定义（追加新记录，不改写历史计划中的快照）。
func (s *Store) SaveGraph(nodes []planner.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(record{Kind: "graph", Nodes: nodes})
}

// AddPlan 追加一份计划，返回分配的计划 ID。
func (s *Store) AddPlan(p *planner.Plan) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.ID = fmt.Sprintf("plan-%04d", len(s.Plans)+1)
	if err := s.append(record{Kind: "plan", Plan: p}); err != nil {
		return "", err
	}
	return p.ID, nil
}

// PlanByID 查找历史计划。
func (s *Store) PlanByID(id string) *planner.Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.Plans {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// AddRun 基于某份计划创建执行。
func (s *Store) AddRun(planID string) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := &Run{
		ID:        fmt.Sprintf("run-%04d", len(s.Runs)+1),
		PlanID:    planID,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.append(record{Kind: "run", Run: run}); err != nil {
		return nil, err
	}
	return run, nil
}

// RunByID 查找执行。
func (s *Store) RunByID(id string) *Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runIdx[id]
}

// AddEvent 追加执行事件；幂等键重复时只记一次，返回 added=false。
func (s *Store) AddEvent(runID string, ev planner.Event) (added bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runIdx[runID]
	if !ok {
		return false, fmt.Errorf("执行 %s 不存在", runID)
	}
	if ev.Key == "" {
		return false, fmt.Errorf("缺少幂等键")
	}
	if s.keys[runID+"/"+ev.Key] {
		return false, nil
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	if err := s.append(record{Kind: "event", RunID: runID, Event: &ev}); err != nil {
		return false, err
	}
	_ = run
	return true, nil
}
