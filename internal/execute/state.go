// Package execute replays execution events and derives per-node state,
// crash-recovery readiness and rollback plans.
package execute

import (
	"fmt"
	"sort"
	"time"

	"migrationplanner/internal/model"
)

// NodeState is the derived execution state of one node.
type NodeState struct {
	NodeID            string        `json:"nodeId"`
	Status            string        `json:"status"`
	OutputFingerprint string        `json:"outputFingerprint"`
	Interrupted       bool          `json:"interrupted"`
	Drift             bool          `json:"drift"`
	Reasons           []string      `json:"reasons"`
	StartedAt         time.Time     `json:"startedAt,omitempty"`
	FinishedAt        time.Time     `json:"finishedAt,omitempty"`
	Duration          time.Duration `json:"duration,omitempty"`
}

// States is the full derived execution picture of a plan.
type States struct {
	PlanID string                `json:"planId"`
	Nodes  map[string]*NodeState `json:"nodes"`
}

func (s *States) node(id string) *NodeState {
	st, ok := s.Nodes[id]
	if !ok {
		st = &NodeState{NodeID: id, Status: model.StatusPending}
		s.Nodes[id] = st
	}
	return st
}

// Replay rebuilds states from append-only events belonging to planID.
// Events must be passed in sequence number order.
func Replay(planID string, events []model.Event) *States {
	s := &States{PlanID: planID, Nodes: map[string]*NodeState{}}
	crashed := false
	for _, ev := range events {
		if ev.PlanID != planID {
			continue
		}
		switch ev.Type {
		case model.EvCrashSimulated:
			crashed = true
		case model.EvNodeStarted:
			st := s.node(ev.NodeID)
			st.Status = model.StatusRunning
			st.OutputFingerprint = ""
			st.Drift = false
			st.Interrupted = false
			st.StartedAt = ev.Time
			if ev.Duration > 0 {
				st.Duration = ev.Duration
			}
		case model.EvNodeSucceeded:
			st := s.node(ev.NodeID)
			st.Status = model.StatusSucceeded
			st.OutputFingerprint = ev.OutputFingerprint
			st.FinishedAt = ev.Time
			st.Interrupted = false
			crashed = false
		case model.EvNodeFailed:
			st := s.node(ev.NodeID)
			st.Status = model.StatusFailed
			st.FinishedAt = ev.Time
			st.Interrupted = false
			st.Reasons = append(st.Reasons, ev.Detail)
			crashed = false
		case model.EvNodeRolledBack:
			st := s.node(ev.NodeID)
			st.Status = model.StatusRolledBack
			st.FinishedAt = ev.Time
			st.Reasons = append(st.Reasons, ev.Detail)
			crashed = false
		}
	}

	// A crash with no terminal result after the last start means interruption.
	if crashed {
		for _, st := range s.Nodes {
			if st.Status == model.StatusRunning {
				st.Interrupted = true
				st.Reasons = append(st.Reasons, "服务崩溃：该节点停留在运行中，恢复后可安全重跑")
			}
		}
	}
	return s
}

// blockedByPredecessor reports whether a predecessor prevents this node from
// ever becoming ready. Drift/failure/manual/rollback hard-block the chain.
func predecessorStatus(status string) bool {
	switch status {
	case model.StatusManual, model.StatusFailed, model.StatusRolledBack:
		return true
	}
	return false
}

// Derive annotates states with drift detection (comparing actual outputs with
// the fingerprint declared in the plan snapshot) and recovery explanations.
// It must be called after Replay and before Ready/CanStart queries.
func Derive(plan *model.Plan, states *States) {
	if plan == nil {
		return
	}
	order := topoOrder(plan.NodeSnapshot)

	for _, id := range order {
		st, ok := states.Nodes[id]
		if !ok {
			continue
		}
		n := plan.NodeSnapshot[id]

		// Fingerprint drift: a succeeded node whose actual output no longer
		// matches what it declared is taken over manually.
		if st.Status == model.StatusSucceeded && n.OutputFingerprint != "" && st.OutputFingerprint != n.OutputFingerprint {
			st.Drift = true
			st.Status = model.StatusManual
			st.Reasons = append(st.Reasons, fmt.Sprintf("输出指纹漂移：实际 %s，计划声明 %s，整条后继链阻塞", st.OutputFingerprint, n.OutputFingerprint))
		}
	}

	for _, id := range order {
		st := states.node(id)
		n := plan.NodeSnapshot[id]
		deps := append([]string{}, n.DependsOn...)
		sort.Strings(deps)

		// Hard chain blocking from predecessors.
		switch st.Status {
		case model.StatusPending, model.StatusRunning:
			for _, dep := range deps {
				dst := states.node(dep)
				if predecessorStatus(dst.Status) || dst.Drift {
					if st.Status == model.StatusPending {
						st.Status = model.StatusManual
					}
					st.Reasons = append(st.Reasons, fmt.Sprintf("后继链阻塞：前置节点 %s 状态为 %s", dep, dst.Status))
				}
			}
		}

		// Recovery / scheduling explanation.
		switch {
		case st.Interrupted:
			// reason already attached in Replay
		case st.Status == model.StatusSucceeded:
			if n.OutputFingerprint != "" && st.OutputFingerprint == n.OutputFingerprint {
				st.Reasons = append(st.Reasons, fmt.Sprintf("已成功且输出指纹 %s 匹配，恢复时不重跑", st.OutputFingerprint))
			} else {
				st.Reasons = append(st.Reasons, "已成功，恢复时跳过")
			}
		case st.Status == model.StatusFailed:
			st.Reasons = append(st.Reasons, "失败后只能前滚：可修正后重试，或为其执行节点生成回滚计划")
		case st.Status == model.StatusPending:
			var pending []string
			var running []string
			for _, dep := range deps {
				switch states.node(dep).Status {
				case model.StatusSucceeded:
				case model.StatusRunning:
					running = append(running, dep)
				default:
					pending = append(pending, dep)
				}
			}
			if len(pending) > 0 {
				st.Reasons = append(st.Reasons, fmt.Sprintf("等待前置节点成功：%s", join(pending)))
			} else if len(running) > 0 {
				st.Reasons = append(st.Reasons, fmt.Sprintf("前置节点运行中：%s", join(running)))
			} else {
				st.Reasons = append(st.Reasons, "所有前置已成功，可以启动")
			}
		}
	}
}

func join(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}

func topoOrder(nodes map[string]model.Node) []string {
	indeg := map[string]int{}
	dependents := map[string][]string{}
	for id, n := range nodes {
		indeg[id] = len(n.DependsOn)
		for _, dep := range n.DependsOn {
			dependents[dep] = append(dependents[dep], id)
		}
	}
	var roots []string
	for id, d := range indeg {
		if d == 0 {
			roots = append(roots, id)
		}
	}
	sort.Strings(roots)
	var order []string
	queue := append([]string{}, roots...)
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		order = append(order, u)
		dsu := append([]string{}, dependents[u]...)
		sort.Strings(dsu)
		for _, v := range dsu {
			indeg[v]--
			if indeg[v] == 0 {
				queue = append(queue, v)
			}
		}
	}
	return order
}

// CanStart reports whether a node may be started/retried right now:
// it must belong to a scheduled wave and all deps must be succeeded.
func CanStart(plan *model.Plan, states *States, nodeID string) (bool, string) {
	if plan == nil {
		return false, "没有可用计划"
	}
	n, ok := plan.NodeSnapshot[nodeID]
	if !ok {
		return false, fmt.Sprintf("节点 %s 不在该计划快照中", nodeID)
	}
	scheduled := false
	for _, w := range plan.Waves {
		for _, id := range w.NodeIDs {
			if id == nodeID {
				scheduled = true
			}
		}
	}
	if !scheduled {
		return false, "节点未进入任何可执行波次（被阻塞或超出窗口）"
	}
	st := states.node(nodeID)
	switch st.Status {
	case model.StatusSucceeded:
		return false, "节点已成功且输出指纹匹配，不得重跑"
	case model.StatusRunning:
		if st.Interrupted {
			return true, "崩溃中断的运行中节点，恢复后可安全重跑"
		}
		return false, "节点正在运行中"
	case model.StatusManual:
		return false, "节点需要人工接管，不能直接启动"
	case model.StatusRolledBack:
		return false, "节点已回滚，前滚请先重新登记迁移步骤"
	}
	for _, dep := range n.DependsOn {
		dst := states.node(dep)
		if dst.Status != model.StatusSucceeded || dst.Drift {
			return false, fmt.Sprintf("前置节点 %s 尚未成功（%s）", dep, dst.Status)
		}
	}
	return true, "可以启动"
}
