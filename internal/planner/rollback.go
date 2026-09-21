package planner

import (
	"fmt"
	"sort"
)

// RollbackStep 一步回滚。
type RollbackStep struct {
	NodeID string `json:"node_id"`
	Name   string `json:"name"`
}

// Rollback 失败节点的回滚计划。
type Rollback struct {
	FailedNode string         `json:"failed_node"`
	Steps      []RollbackStep `json:"steps"`
	Complete   bool           `json:"complete"`
	StopReason string         `json:"stop_reason,omitempty"`
}

// LatestStatuses 从事件流计算每个节点的最新状态。
func LatestStatuses(events []Event) map[string]NodeStatus {
	out := map[string]NodeStatus{}
	for _, e := range events {
		out[e.NodeID] = e.Status
	}
	return out
}

// BuildRollback 为失败节点生成回滚计划：
// 只包含已执行成功且声明可回滚的祖先，按执行的逆序排列；
// 遇到不可回滚节点时明确停止，不给出看似完整的方案。
func BuildRollback(plan *Plan, events []Event, failedID string) (*Rollback, error) {
	if _, ok := plan.Nodes[failedID]; !ok {
		return nil, fmt.Errorf("节点 %q 不在计划中", failedID)
	}
	status := LatestStatuses(events)
	waveOf := map[string]int{}
	for _, w := range plan.Waves {
		for _, pn := range w.Nodes {
			waveOf[pn.ID] = w.Index
		}
	}
	anc := ancestors(plan.Nodes, failedID)
	var executed []string
	for id := range anc {
		if status[id] == StatusSuccess {
			executed = append(executed, id)
		}
	}
	// 逆执行顺序：波次降序，同波次按 ID 降序（稳定）
	sort.Slice(executed, func(i, j int) bool {
		wi, wj := waveOf[executed[i]], waveOf[executed[j]]
		if wi != wj {
			return wi > wj
		}
		return executed[i] > executed[j]
	})
	rb := &Rollback{FailedNode: failedID, Complete: true}
	for _, id := range executed {
		n := plan.Nodes[id]
		if n.Rollbackable {
			rb.Steps = append(rb.Steps, RollbackStep{NodeID: id, Name: n.Name})
			continue
		}
		rb.Complete = false
		rb.StopReason = fmt.Sprintf("节点 %s（%s）声明不可回滚，回滚计划在此停止；更早的已执行祖先需要人工接管", id, n.Name)
		return rb, nil
	}
	return rb, nil
}
