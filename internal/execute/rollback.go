package execute

import (
	"fmt"
	"sort"

	"migrationplanner/internal/model"
)

// RollbackStep is one node to roll back, in reverse execution order.
type RollbackStep struct {
	NodeID string `json:"nodeId"`
	Reason string `json:"reason"`
}

// RollbackPlan is deliberately a partial answer when a non-rollbackable
// ancestor exists: Steps contains only safe, executable steps up to the
// boundary; Complete is false and Boundary explains why it stops.
type RollbackPlan struct {
	FailedNodeID string         `json:"failedNodeId"`
	Steps        []RollbackStep `json:"steps"`
	Complete     bool           `json:"complete"`
	Boundary     string         `json:"boundary,omitempty"`
}

// BuildRollbackPlan builds the rollback plan for a failed node.
//
// Rules:
//   - Only executed ancestors (succeeded, or running/interrupted on crash) that
//     declared rollbackable=true can appear in the plan.
//   - Steps are emitted in reverse topological order (latest executed first).
//   - At the first executed but non-rollbackable ancestor the plan stops: that
//     node and everything before it are omitted, Complete=false and Boundary
//     names the node. We never present a deceptively "complete" plan.
func BuildRollbackPlan(plan *model.Plan, states *States, failedNodeID string) (*RollbackPlan, error) {
	if plan == nil {
		return nil, fmt.Errorf("没有可用计划")
	}
	if _, ok := plan.NodeSnapshot[failedNodeID]; !ok {
		return nil, fmt.Errorf("节点 %s 不在计划快照中", failedNodeID)
	}
	fst := states.node(failedNodeID)
	if fst.Status != model.StatusFailed {
		return nil, fmt.Errorf("节点 %s 当前状态为 %s，只有失败节点可以生成回滚计划", failedNodeID, fst.Status)
	}

	// Collect executed ancestors (transitive dependencies), excluding the
	// failed node itself.
	ancestors := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		n := plan.NodeSnapshot[id]
		for _, dep := range n.DependsOn {
			if !ancestors[dep] {
				ancestors[dep] = true
				walk(dep)
			}
		}
	}
	walk(failedNodeID)

	executed := func(id string) bool {
		st := states.node(id)
		switch st.Status {
		case model.StatusSucceeded, model.StatusRunning, model.StatusRolledBack:
			return st.Status != model.StatusRolledBack
		}
		return false
	}

	var todo []string
	for id := range ancestors {
		if executed(id) {
			todo = append(todo, id)
		}
	}

	// Topological order of the snapshot; take reverse for rollback ordering.
	topo := topoOrder(plan.NodeSnapshot)
	pos := map[string]int{}
	for i, id := range topo {
		pos[id] = i
	}
	sort.Slice(todo, func(i, j int) bool {
		if pos[todo[i]] != pos[todo[j]] {
			return pos[todo[i]] > pos[todo[j]]
		}
		return todo[i] > todo[j]
	})

	rp := &RollbackPlan{FailedNodeID: failedNodeID, Complete: true, Steps: []RollbackStep{}}
	for _, id := range todo {
		n := plan.NodeSnapshot[id]
		if !n.Rollbackable {
			rp.Complete = false
			rp.Boundary = fmt.Sprintf("祖先节点 %s 已执行但声明不可回滚，回滚在此明确停止；其 %d 个更早日节点不在计划内，当前没有看似完整的方案", id, len(rp.Steps))
			break
		}
		rp.Steps = append(rp.Steps, RollbackStep{
			NodeID: id,
			Reason: fmt.Sprintf("已执行且声明可回滚，按逆序回滚（失败节点 %s 的祖先）", failedNodeID),
		})
	}
	if len(rp.Steps) == 0 && rp.Complete {
		rp.Boundary = "失败节点没有任何已执行的祖先节点，无需回滚（失败节点本身不包含在回滚计划中）"
	}
	return rp, nil
}
