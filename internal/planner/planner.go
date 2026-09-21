// Package planner turns migration node definitions into executable waves.
package planner

import (
	"container/heap"
	"fmt"
	"sort"
	"strings"
	"time"

	"migrationplanner/internal/model"
)

// CycleError carries one concrete cycle path, e.g. ["a","b","c","a"].
type CycleError struct {
	Cycle []string
}

func (e *CycleError) Error() string {
	return "依赖图存在环: " + strings.Join(e.Cycle, " -> ")
}

// ValidationError describes invalid node definitions.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// FindCycle returns a concrete cycle path (first == last) or nil.
// Edges follow the "depends on" direction: n -> each dependency.
func FindCycle(nodes map[string]model.Node) []string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	onStack := map[string]bool{}

	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var dfs func(string) []string
	dfs = func(u string) []string {
		color[u] = gray
		onStack[u] = true
		stack = append(stack, u)

		deps := append([]string{}, nodes[u].DependsOn...)
		sort.Strings(deps)
		for _, v := range deps {
			if _, ok := nodes[v]; !ok {
				continue
			}
			if color[v] == white {
				if cyc := dfs(v); cyc != nil {
					return cyc
				}
			} else if color[v] == gray {
				// Extract v..u from the stack and close with v.
				var cyc []string
				for i, x := range stack {
					if onStack[x] && i >= indexOf(stack, v) {
						cyc = append(cyc, x)
					}
				}
				cyc = append(cyc, v)
				return cyc
			}
		}
		stack = stack[:len(stack)-1]
		onStack[u] = false
		color[u] = black
		return nil
	}

	for _, id := range ids {
		if color[id] == white {
			if cyc := dfs(id); cyc != nil {
				return cyc
			}
		}
	}
	return nil
}

func indexOf(xs []string, v string) int {
	for i, x := range xs {
		if x == v {
			return i
		}
	}
	return -1
}

func validate(nodes map[string]model.Node) error {
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	seen := map[string]bool{}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return &ValidationError{Msg: "存在 ID 为空的节点"}
		}
		if seen[id] {
			return &ValidationError{Msg: fmt.Sprintf("节点 ID 重复: %s", id)}
		}
		seen[id] = true
		n := nodes[id]
		if n.Duration < 0 {
			return &ValidationError{Msg: fmt.Sprintf("节点 %s 预计时长不能为负", id)}
		}
		for _, dep := range n.DependsOn {
			if _, ok := nodes[dep]; !ok {
				return &ValidationError{Msg: fmt.Sprintf("节点 %s 依赖了不存在的节点 %s", id, dep)}
			}
		}
	}
	return nil
}

type stringHeap []string

func (h stringHeap) Len() int            { return len(h) }
func (h stringHeap) Less(i, j int) bool  { return h[i] < h[j] }
func (h stringHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *stringHeap) Push(x interface{}) { *h = append(*h, x.(string)) }
func (h *stringHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// topoOrder returns a deterministic Kahn topological order (smallest ID first).
func topoOrder(nodes map[string]model.Node) []string {
	indeg := map[string]int{}
	dependents := map[string][]string{}
	for id, n := range nodes {
		indeg[id] = len(n.DependsOn)
		for _, dep := range n.DependsOn {
			dependents[dep] = append(dependents[dep], id)
		}
	}
	h := &stringHeap{}
	heap.Init(h)
	for id, d := range indeg {
		if d == 0 {
			heap.Push(h, id)
		}
	}
	var order []string
	for h.Len() > 0 {
		u := heap.Pop(h).(string)
		order = append(order, u)
		dsu := append([]string{}, dependents[u]...)
		sort.Strings(dsu)
		for _, v := range dsu {
			indeg[v]--
			if indeg[v] == 0 {
				heap.Push(h, v)
			}
		}
	}
	return order
}

// Plan generates waves and explanations. On a cyclic graph it returns
// *CycleError carrying a concrete cycle path.
func Plan(planID string, req model.PlanRequest, nodes map[string]model.Node, now time.Time) (*model.Plan, error) {
	if err := validate(nodes); err != nil {
		return nil, err
	}
	if cyc := FindCycle(nodes); cyc != nil {
		return nil, &CycleError{Cycle: cyc}
	}
	if !req.WindowEnd.After(req.WindowStart) {
		return nil, &ValidationError{Msg: "维护窗口结束时间必须晚于开始时间"}
	}

	order := topoOrder(nodes)

	// Blocked reasons, filled in topological order so successors inherit
	// blocked ancestors (the whole downstream chain blocks).
	blockedReasons := map[string][]string{}
	blockedSet := map[string]bool{}

	addBlock := func(id string, reason string) {
		blockedReasons[id] = append(blockedReasons[id], reason)
		blockedSet[id] = true
	}

	for _, id := range order {
		n := nodes[id]

		// Inherit blocking from dependencies (one level suffices in topo order).
		deps := append([]string{}, n.DependsOn...)
		sort.Strings(deps)
		for _, dep := range deps {
			if blockedSet[dep] {
				firstReason := ""
				if rs := blockedReasons[dep]; len(rs) > 0 {
					firstReason = rs[0]
				}
				addBlock(id, fmt.Sprintf("前置节点 %s 被阻塞（%s），后继链阻塞", dep, firstReason))
				break
			}
		}

		// Fingerprint chain checks.
		if n.InputFingerprint != "" {
			if len(deps) == 0 {
				if n.InputFingerprint != req.StartFingerprint {
					addBlock(id, fmt.Sprintf("输入指纹 %s 与起始 schema 指纹 %s 不匹配", n.InputFingerprint, req.StartFingerprint))
				}
			} else {
				for _, dep := range deps {
					out := nodes[dep].OutputFingerprint
					if out != "" && out != n.InputFingerprint {
						addBlock(id, fmt.Sprintf("前置节点 %s 声明输出指纹 %s，与本节点输入指纹 %s 不匹配", dep, out, n.InputFingerprint))
					}
				}
			}
		}
	}

	// Base wave numbers: longest dependency path, over schedulable nodes.
	wave := map[string]int{}
	for _, id := range order {
		if blockedSet[id] {
			continue
		}
		w := 0
		for _, dep := range nodes[id].DependsOn {
			if !blockedSet[dep] && wave[dep]+1 > w {
				w = wave[dep] + 1
			}
		}
		wave[id] = w
	}

	// Mutex resources: users of the same resource must never share a wave.
	// Enforce strictly increasing waves in stable (wave, ID) order until fixpoint.
	mutexRaised := map[string]string{} // node -> human reason for the raise
	for {
		changed := false
		byResource := map[string][]string{}
		for id := range wave {
			for _, r := range nodes[id].MutexResources {
				byResource[r] = append(byResource[r], id)
			}
		}
		resources := make([]string, 0, len(byResource))
		for r := range byResource {
			resources = append(resources, r)
		}
		sort.Strings(resources)
		for _, r := range resources {
			users := byResource[r]
			sort.Slice(users, func(i, j int) bool {
				if wave[users[i]] != wave[users[j]] {
					return wave[users[i]] < wave[users[j]]
				}
				return users[i] < users[j]
			})
			for i := 1; i < len(users); i++ {
				prev, cur := users[i-1], users[i]
				if wave[cur] <= wave[prev] {
					wave[cur] = wave[prev] + 1
					mutexRaised[cur] = fmt.Sprintf("与节点 %s 共用互斥资源 %q，顺延至波次 %d", prev, r, wave[cur])
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}

	// Group into waves with stable intra-wave ordering.
	maxWave := -1
	for _, w := range wave {
		if w > maxWave {
			maxWave = w
		}
	}
	waves := []model.Wave{}
	var overflowBlocked []string
	t := req.WindowStart
	for idx := 0; idx <= maxWave; idx++ {
		var ids []string
		var waveDur time.Duration
		for id, w := range wave {
			if w == idx {
				ids = append(ids, id)
				if nodes[id].Duration > waveDur {
					waveDur = nodes[id].Duration
				}
			}
		}
		sort.Strings(ids)
		start := t
		end := start.Add(waveDur)
		waves = append(waves, model.Wave{Index: idx, NodeIDs: ids, Start: start, End: end})
		if end.After(req.WindowEnd) {
			overflowBlocked = append(overflowBlocked, ids...)
		}
		t = end
	}

	// Window overflow blocks this wave and every wave after it.
	overflowFrom := -1
	for i := len(waves) - 1; i >= 0; i-- {
		if waves[i].End.After(req.WindowEnd) {
			overflowFrom = i
		}
	}
	if overflowFrom >= 0 {
		for idx := overflowFrom; idx < len(waves); idx++ {
			for _, id := range waves[idx].NodeIDs {
				if !blockedSet[id] {
					addBlock(id, fmt.Sprintf("维护窗口不足：波次 %d 将于 %s 结束，超过窗口结束时间 %s", idx, waves[idx].End.Format("15:04"), req.WindowEnd.Format("15:04")))
				}
			}
		}
		// Keep only waves that fit.
		waves = waves[:overflowFrom]
	}

	// Explanations.
	expl := map[string][]string{}
	for _, id := range order {
		n := nodes[id]
		if blockedSet[id] {
			expl[id] = blockedReasons[id]
			continue
		}
		var why []string
		deps := append([]string{}, n.DependsOn...)
		sort.Strings(deps)
		if len(deps) == 0 {
			why = append(why, "无前置依赖，最早可执行")
		} else {
			parts := make([]string, 0, len(deps))
			maxDepWave := -1
			for _, dep := range deps {
				if blockedSet[dep] {
					continue
				}
				parts = append(parts, fmt.Sprintf("%s(波次%d)", dep, wave[dep]))
				if wave[dep] > maxDepWave {
					maxDepWave = wave[dep]
				}
			}
			why = append(why, fmt.Sprintf("依赖 %s 完成后进入波次 %d", strings.Join(parts, ", "), wave[id]))
		}
		if reason, ok := mutexRaised[id]; ok {
			why = append(why, reason)
		}
		if w := wave[id]; w >= 0 && w < len(waves) {
			why = append(why, fmt.Sprintf("窗口内计划 %s–%s（时长 %s）", waves[w].Start.Format("15:04"), waves[w].End.Format("15:04"), n.Duration))
		}
		expl[id] = why
	}

	var blocked []model.BlockedNode
	for _, id := range order {
		if blockedSet[id] {
			blocked = append(blocked, model.BlockedNode{NodeID: id, Reasons: blockedReasons[id]})
		}
	}

	snapshot := map[string]model.Node{}
	for id, n := range nodes {
		snapshot[id] = n
	}

	return &model.Plan{
		ID:           planID,
		RequestedAt:  now,
		Request:      req,
		NodeSnapshot: snapshot,
		Waves:        waves,
		Blocked:      blocked,
		Explanations: expl,
	}, nil
}
