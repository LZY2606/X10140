// Package store 提供基于项目目录文件的持久化：
// 节点定义（整体覆盖）、计划快照与执行事件（追加式，不回写历史）。
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"migrationplanner/internal/planner"
)

// Store 文件存储。
type Store struct {
	dir       string
	mu        sync.Mutex
	eventKeys map[string]bool
	plans     map[string]*planner.Plan
	planOrder []string
}

// Open 打开（必要时创建）数据目录并加载索引。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, eventKeys: map[string]bool{}, plans: map[string]*planner.Plan{}}
	if err := s.loadPlans(); err != nil {
		return nil, err
	}
	if err := s.loadEventKeys(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) path(name string) string { return filepath.Join(s.dir, name) }

// SaveNodes 覆盖保存当前节点定义。
func (s *Store) SaveNodes(nodes []planner.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(nodes, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path("nodes.json"), data, 0o644)
}

// LoadNodes 读取当前节点定义。
func (s *Store) LoadNodes() ([]planner.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path("nodes.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var nodes []planner.Node
	if err := json.Unmarshal(data, &nodes); err != nil {
		return nil, err
	}
	return nodes, nil
}

// AppendPlan 追加保存一份计划快照，之后不再修改。
func (s *Store) AppendPlan(p *planner.Plan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := appendJSONLine(s.path("plans.jsonl"), p); err != nil {
		return err
	}
	s.plans[p.ID] = p
	s.planOrder = append(s.planOrder, p.ID)
	return nil
}

// Plans 按创建顺序返回全部历史计划。
func (s *Store) Plans() []*planner.Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*planner.Plan, 0, len(s.planOrder))
	for _, id := range s.planOrder {
		out = append(out, s.plans[id])
	}
	return out
}

// Plan 按 ID 返回计划快照。
func (s *Store) Plan(id string) (*planner.Plan, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[id]
	return p, ok
}

// AppendEvent 追加一条执行事件。幂等键重复时不重复记录，返回 created=false。
func (s *Store) AppendEvent(e planner.Event) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Key != "" && s.eventKeys[e.Key] {
		return false, nil
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if err := appendJSONLine(s.path("events.jsonl"), e); err != nil {
		return false, err
	}
	if e.Key != "" {
		s.eventKeys[e.Key] = true
	}
	return true, nil
}

// Events 返回某个计划的全部事件（按追加顺序）。
func (s *Store) Events(planID string) ([]planner.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []planner.Event
	err := readJSONLines(s.path("events.jsonl"), func(e planner.Event) {
		if planID == "" || e.PlanID == planID {
			out = append(out, e)
		}
	})
	return out, err
}

func (s *Store) loadPlans() error {
	return readJSONLines(s.path("plans.jsonl"), func(p *planner.Plan) {
		s.plans[p.ID] = p
		s.planOrder = append(s.planOrder, p.ID)
	})
}

func (s *Store) loadEventKeys() error {
	return readJSONLines(s.path("events.jsonl"), func(e planner.Event) {
		if e.Key != "" {
			s.eventKeys[e.Key] = true
		}
	})
}

func appendJSONLine(path string, v any) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	return err
}

func readJSONLines[T any](path string, fn func(T)) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(line, &v); err != nil {
			return err
		}
		fn(v)
	}
	return sc.Err()
}
