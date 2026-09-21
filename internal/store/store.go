// Package store persists planning and execution events as an append-only
// JSON-lines log. Node definitions and execution state are derived by replay.
package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"migrationplanner/internal/model"
)

// Store is a file-backed append-only event store.
type Store struct {
	mu       sync.Mutex
	path     string
	events   []model.Event
	idemSeen map[string]int // idempotency key -> sequence of first event
	seq      int
}

// Open loads (or creates) the event log at dir/events.jsonl.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "events.jsonl")
	s := &Store{path: path, idemSeen: map[string]int{}}
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev model.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			f.Close()
			return nil, fmt.Errorf("事件日志损坏: %w", err)
		}
		s.appendInMemory(ev)
	}
	f.Close()
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return s, nil
}

// appendInMemory indexes an event without writing it to disk.
func (s *Store) appendInMemory(ev model.Event) {
	if ev.Seq > s.seq {
		s.seq = ev.Seq
	}
	if ev.IdempotencyKey != "" {
		if _, ok := s.idemSeen[ev.IdempotencyKey]; !ok {
			s.idemSeen[ev.IdempotencyKey] = ev.Seq
		}
	}
	s.events = append(s.events, ev)
}

// Append writes a new event. If idempotencyKey was already seen it returns the
// first event with that key and found=true instead of recording anything.
func (s *Store) Append(ev model.Event, idempotencyKey string) (model.Event, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idempotencyKey != "" {
		if seq, ok := s.idemSeen[idempotencyKey]; ok {
			for _, e := range s.events {
				if e.Seq == seq {
					return e, true, nil
				}
			}
		}
	}
	s.seq++
	ev.Seq = s.seq
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	ev.IdempotencyKey = idempotencyKey
	line, err := json.Marshal(ev)
	if err != nil {
		return model.Event{}, false, err
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return model.Event{}, false, err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return model.Event{}, false, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return model.Event{}, false, err
	}
	f.Close()
	s.appendInMemory(ev)
	return ev, false, nil
}

// LookupIdempotent returns the first event recorded with key, if any.
func (s *Store) LookupIdempotent(key string) (model.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == "" {
		return model.Event{}, false
	}
	seq, ok := s.idemSeen[key]
	if !ok {
		return model.Event{}, false
	}
	for _, e := range s.events {
		if e.Seq == seq {
			return e, true
		}
	}
	return model.Event{}, false
}

// Events returns a copy of all events in sequence order.
func (s *Store) Events() []model.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.Event, len(s.events))
	copy(out, s.events)
	return out
}

// Nodes derives current node definitions by replaying definition events.
func (s *Store) Nodes() map[string]model.Node {
	nodes := map[string]model.Node{}
	for _, ev := range s.Events() {
		switch ev.Type {
		case model.EvNodeUpserted:
			if ev.Node != nil {
				nodes[ev.Node.ID] = *ev.Node
			}
		case model.EvNodeDeleted:
			delete(nodes, ev.DeletedNodeID)
		}
	}
	return nodes
}

// Plan finds the most recent plan by ID, or the latest plan if id is "".
// Plans are embedded whole in plan_created events, so history is immutable.
func (s *Store) Plan(id string) (*model.Plan, bool) {
	var found *model.Plan
	for _, ev := range s.Events() {
		if ev.Type != model.EvPlanCreated || ev.Plan == nil {
			continue
		}
		if id == "" || ev.Plan.ID == id {
			p := *ev.Plan
			found = &p
		}
	}
	return found, found != nil
}

// Plans returns all stored plans newest first.
func (s *Store) Plans() []model.Plan {
	var out []model.Plan
	for _, ev := range s.Events() {
		if ev.Type == model.EvPlanCreated && ev.Plan != nil {
			out = append(out, *ev.Plan)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].RequestedAt.After(out[j].RequestedAt) })
	return out
}

// EventsForPlan returns execution/planning events belonging to a plan.
func (s *Store) EventsForPlan(planID string) []model.Event {
	var out []model.Event
	for _, ev := range s.Events() {
		if ev.PlanID == planID {
			out = append(out, ev)
		}
	}
	return out
}
