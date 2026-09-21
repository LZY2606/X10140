package planner

import (
	"fmt"
	"sort"
	"time"
)

// NodeState 是节点在执行记录中的状态。
type NodeState string

const (
	StatePending     NodeState = "pending"
	StateRunning     NodeState = "running"
	StateSucceeded   NodeState = "succeeded"
	StateFailed      NodeState = "failed"
	StateRolledBack  NodeState = "rolled_back"
	StateNeedsManual NodeState = "needs_manual"
	StateInterrupted NodeState = "interrupted"
	StateBlocked     NodeState = "blocked"
)

// 执行事件类型。
const (
	EventStarted     = "started"
	EventSucceeded   = "succeeded"
	EventFailed      = "failed"
	EventRolledBack  = "rolled_back"
	EventNeedsManual = "needs_manual"
	EventCrash       = "crash"
)

// Event 是一条追加保存的执行事件，Key 为幂等键。
type Event struct {
	Key               string    `json:"key"`
	Type              string    `json:"type"`
	NodeID            string    `json:"node_id,omitempty"`
	OutputFingerprint string    `json:"output_fingerprint,omitempty"`
	At                time.Time `json:"at"`
}

// Recovery 是从事件流重放出的执行状态。
type Recovery struct {
	States         map[string]NodeState `json:"states"`
	BlockReasons   map[string]string    `json:"block_reasons,omitempty"`
	Outputs        map[string]string    `json:"outputs,omitempty"`
	ExecutionOrder []string             `json:"execution_order,omitempty"`
}

func nodeIndex(plan *Plan) map[string]Node {
	m := map[string]Node{}
	for _, n := range plan.Nodes {
		m[n.ID] = n
	}
	return m
}

// descendants 返回 id 的全部传递后继（按 ID 字典序）。
func descendants(nodes []Node, id string) []string {
	dependents := map[string][]string{}
	for _, n := range nodes {
		for _, d := range n.DependsOn {
			dependents[d] = append(dependents[d], n.ID)
		}
	}
	seen := map[string]bool{}
	var queue = []string{id}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, nxt := range dependents[cur] {
			if !seen[nxt] {
				seen[nxt] = true
				queue = append(queue, nxt)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ancestors 返回 id 的全部传递前置集合。
func ancestors(nodes []Node, id string) map[string]bool {
	byID := map[string]Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	seen := map[string]bool{}
	var walk func(cur string)
	walk = func(cur string) {
		for _, d := range byID[cur].DependsOn {
			if !seen[d] {
				seen[d] = true
				walk(d)
			}
		}
	}
	walk(id)
	return seen
}

// Recover 按顺序重放事件，得到当前执行状态。
// 规则：崩溃时处于 running 的节点标记为 interrupted（可重跑）；
// 已成功但输出指纹与节点声明不匹配的节点，连同其整条后继链进入阻塞。
func Recover(plan *Plan, events []Event) *Recovery {
	rec := &Recovery{
		States:       map[string]NodeState{},
		BlockReasons: map[string]string{},
		Outputs:      map[string]string{},
	}
	for _, item := range plan.Items {
		if item.Blocked {
			rec.States[item.NodeID] = StateBlocked
			rec.BlockReasons[item.NodeID] = item.BlockReason
		} else {
			rec.States[item.NodeID] = StatePending
		}
	}
	crashed := false
	for _, ev := range events {
		if ev.Type == EventCrash {
			crashed = true
			continue
		}
		st, ok := rec.States[ev.NodeID]
		if !ok || st == StateBlocked {
			continue
		}
		switch ev.Type {
		case EventStarted:
			rec.States[ev.NodeID] = StateRunning
		case EventSucceeded:
			rec.States[ev.NodeID] = StateSucceeded
			rec.Outputs[ev.NodeID] = ev.OutputFingerprint
			rec.ExecutionOrder = append(rec.ExecutionOrder, ev.NodeID)
		case EventFailed:
			rec.States[ev.NodeID] = StateFailed
		case EventRolledBack:
			rec.States[ev.NodeID] = StateRolledBack
		case EventNeedsManual:
			rec.States[ev.NodeID] = StateNeedsManual
		}
	}
	if crashed {
		for id, st := range rec.States {
			if st == StateRunning {
				rec.States[id] = StateInterrupted
			}
		}
	}
	// 输出指纹漂移：节点本身与整条后继链阻塞。
	byID := nodeIndex(plan)
	for _, item := range plan.Items {
		id := item.NodeID
		if rec.States[id] != StateSucceeded {
			continue
		}
		want := byID[id].OutputFingerprint
		if want == "" || rec.Outputs[id] == want {
			continue
		}
		got := rec.Outputs[id]
		rec.States[id] = StateBlocked
		rec.BlockReasons[id] = fmt.Sprintf("输出指纹漂移：期望 %q，实得 %q", want, got)
		for _, desc := range descendants(plan.Nodes, id) {
			if rec.States[desc] == StateBlocked {
				continue
			}
			rec.States[desc] = StateBlocked
			rec.BlockReasons[desc] = fmt.Sprintf("上游节点 %s 输出指纹漂移，后继链阻塞", id)
		}
	}
	return rec
}

// RollbackResult 是失败节点的回滚方案。
type RollbackResult struct {
	FailedNode string   `json:"failed_node"`
	Steps      []string `json:"steps"` // 逆序的可回滚祖先
	Complete   bool     `json:"complete"`
	StoppedAt  string   `json:"stopped_at,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

// RollbackPlan 为失败节点生成回滚方案：只包含已执行成功且声明可回滚的
// 逆序祖先；遇到不可回滚节点必须明确停止，不给出看似完整的方案。
func RollbackPlan(plan *Plan, failedID string, rec *Recovery) RollbackResult {
	res := RollbackResult{FailedNode: failedID, Complete: true}
	if rec.States[failedID] != StateFailed {
		res.Complete = false
		res.Reason = fmt.Sprintf("节点 %s 当前状态为 %s，不是失败状态", failedID, rec.States[failedID])
		return res
	}
	byID := nodeIndex(plan)
	anc := ancestors(plan.Nodes, failedID)
	for i := len(rec.ExecutionOrder) - 1; i >= 0; i-- {
		id := rec.ExecutionOrder[i]
		if !anc[id] || rec.States[id] != StateSucceeded {
			continue
		}
		if byID[id].Rollbackable {
			res.Steps = append(res.Steps, id)
			continue
		}
		res.Complete = false
		res.StoppedAt = id
		res.Reason = fmt.Sprintf("节点 %s 已执行成功但声明不可回滚，回滚链在此中断，需要人工接管", id)
		return res
	}
	if len(res.Steps) == 0 {
		res.Reason = "没有需要回滚的已执行祖先"
	}
	return res
}
