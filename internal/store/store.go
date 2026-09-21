package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"migplanner/internal/model"
)

var ErrDuplicate = errors.New("幂等键已存在：该执行事件此前已提交过，仅记录一次")

// Store is an append-only JSONL event store. Every planning and execution
// event is appended; plans snapshot node definitions, so later edits never
// rewrite historical plans.
type Store struct {
	mu    sync.Mutex
	dir   string
	f     *os.File
	seq   int64
	nodes map[string]*model.Node
	plans map[string]*model.Plan
	execs map[string][]*model.ExecEvent
	idem  map[string]int64 // planID + "|" + key -> seq
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录: %w", err)
	}
	path := filepath.Join(dir, "events.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s := &Store{
		dir:   dir,
		f:     f,
		nodes: map[string]*model.Node{},
		plans: map[string]*model.Plan{},
		execs: map[string][]*model.ExecEvent{},
		idem:  map[string]int64{},
	}
	if err := s.replay(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}

func (s *Store) replay() error {
	if _, err := s.f.Seek(0, 0); err != nil {
		return err
	}
	sc := bufio.NewScanner(s.f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev model.NodeEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return fmt.Errorf("事件日志损坏: %w", err)
		}
		s.apply(&ev)
		if ev.Seq > s.seq {
			s.seq = ev.Seq
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if _, err := s.f.Seek(0, os.SEEK_END); err != nil {
		return err
	}
	return nil
}

func (s *Store) apply(ev *model.NodeEvent) {
	switch ev.Type {
	case "node_saved":
		if ev.Node != nil {
			cp := *ev.Node
			cp.Prereqs = append([]string{}, ev.Node.Prereqs...)
			cp.Resources = append([]string{}, ev.Node.Resources...)
			cp.Inputs = append([]string{}, ev.Node.Inputs...)
			s.nodes[cp.ID] = &cp
		}
	case "node_deleted":
		delete(s.nodes, ev.NodeID)
	case "plan_created":
		if ev.Plan != nil {
			s.plans[ev.Plan.ID] = ev.Plan
		}
	case "exec_event":
		if ev.Exec != nil {
			e := ev.Exec
			key := e.PlanID + "|" + e.IdemKey
			if _, exists := s.idem[key]; exists {
				return
			}
			s.idem[key] = e.Seq
			s.execs[e.PlanID] = append(s.execs[e.PlanID], e)
		}
	}
}

// append must be called with s.mu held.
func (s *Store) append(ev *model.NodeEvent) error {
	s.seq++
	ev.Seq = s.seq
	if ev.At == "" {
		ev.At = time.Now().UTC().Format(time.RFC3339Nano)
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := s.f.Write(data); err != nil {
		return err
	}
	if err := s.f.Sync(); err != nil {
		return err
	}
	s.apply(ev)
	return nil
}

func (s *Store) ListNodes() []*model.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*model.Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		cp := *n
		cp.Prereqs = append([]string{}, n.Prereqs...)
		cp.Resources = append([]string{}, n.Resources...)
		cp.Inputs = append([]string{}, n.Inputs...)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) SaveNode(n *model.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *n
	cp.Prereqs = append([]string{}, n.Prereqs...)
	cp.Resources = append([]string{}, n.Resources...)
	cp.Inputs = append([]string{}, n.Inputs...)
	return s.append(&model.NodeEvent{Type: "node_saved", Node: &cp})
}

func (s *Store) DeleteNode(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.append(&model.NodeEvent{Type: "node_deleted", NodeID: id})
}

func (s *Store) CreatePlan(p *model.Plan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.plans[p.ID]; exists {
		return fmt.Errorf("计划 %s 已存在", p.ID)
	}
	if p.Created == "" {
		p.Created = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return s.append(&model.NodeEvent{Type: "plan_created", Plan: p})
}

func (s *Store) ListPlans() []*model.Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*model.Plan, 0, len(s.plans))
	for _, p := range s.plans {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created < out[j].Created })
	return out
}

func (s *Store) GetPlan(id string) *model.Plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.plans[id]
}

// AppendExec appends an execution event. A repeated submission carrying the
// same (planId, idempotency key) is stored exactly once and returns
// ErrDuplicate.
func (s *Store) AppendExec(e *model.ExecEvent) error {
	if e.IdemKey == "" {
		return fmt.Errorf("执行事件必须携带幂等键")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := e.PlanID + "|" + e.IdemKey
	if _, exists := s.idem[key]; exists {
		return ErrDuplicate
	}
	if e.At == "" {
		e.At = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return s.append(&model.NodeEvent{Type: "exec_event", Exec: e})
}

func (s *Store) ExecEvents(planID string) []*model.ExecEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]*model.ExecEvent{}, s.execs[planID]...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// DataFile reports the event log path (used by the UI).
func (s *Store) DataFile() string {
	return filepath.Join(s.dir, "events.jsonl")
}

// RawLines returns the full append-only event log (newline-terminated).
func (s *Store) RawLines() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.ReadFile(filepath.Join(s.dir, "events.jsonl"))
}

// Exists reports whether the data directory already holds an event log.
func Exists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "events.jsonl"))
	return !errors.Is(err, fs.ErrNotExist)
}
