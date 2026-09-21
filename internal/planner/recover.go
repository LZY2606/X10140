package planner

import "fmt"

// RecoverEntry 恢复后单个节点的处置建议。
type RecoverEntry struct {
	Status NodeStatus `json:"status"`
	Action string     `json:"action"` // skip / run / blocked / manual
	Reason string     `json:"reason"`
}

// RecoverReport 崩溃恢复报告。
type RecoverReport struct {
	PlanID string                  `json:"plan_id"`
	Nodes  map[string]RecoverEntry `json:"nodes"`
}

// Recover 从中断恢复：已成功且输出指纹匹配的节点不重跑；
// 输出指纹不匹配的节点及其整条后继链进入阻塞；
// 失败或运行中被中断的节点需要人工接管。
func Recover(plan *Plan, events []Event) *RecoverReport {
	latest := map[string]Event{}
	for _, e := range events {
		latest[e.NodeID] = e
	}
	blockedReason := map[string]string{}
	for id, e := range latest {
		if e.Status != StatusSuccess {
			continue
		}
		expected := plan.Nodes[id].OutputFingerprint
		if expected != "" && e.OutputFingerprint != expected {
			blockedReason[id] = fmt.Sprintf("输出指纹漂移：记录值 %q 与计划期望 %q 不一致", e.OutputFingerprint, expected)
			for d := range descendants(plan.Nodes, id) {
				if _, ok := blockedReason[d]; !ok {
					blockedReason[d] = fmt.Sprintf("上游节点 %s 输出指纹漂移，后继链阻塞", id)
				}
			}
		}
	}
	planBlocked := map[string]string{}
	for _, b := range plan.Blocked {
		planBlocked[b.ID] = b.Reason
	}
	report := &RecoverReport{PlanID: plan.ID, Nodes: map[string]RecoverEntry{}}
	for id := range plan.Nodes {
		status := StatusPending
		if e, ok := latest[id]; ok {
			status = e.Status
		}
		entry := RecoverEntry{Status: status, Action: "run", Reason: "尚未完成，需要执行"}
		if reason, ok := blockedReason[id]; ok {
			entry.Action = "blocked"
			entry.Reason = reason
			if status == StatusSuccess {
				entry.Status = StatusNeedsManual
			}
		} else if reason, ok := planBlocked[id]; ok {
			entry.Action = "blocked"
			entry.Reason = "规划期阻塞：" + reason
		} else {
			switch status {
			case StatusSuccess:
				entry.Action = "skip"
				entry.Reason = "已成功且输出指纹匹配，不重跑"
			case StatusFailed:
				entry.Action = "manual"
				entry.Reason = "执行失败，需要人工接管或生成回滚计划"
			case StatusRunning:
				entry.Action = "manual"
				entry.Reason = "中断时处于运行中，需要人工确认实际状态"
			case StatusRolledBack:
				entry.Action = "manual"
				entry.Reason = "已回滚，是否重放需要人工决定"
			case StatusNeedsManual:
				entry.Action = "manual"
				entry.Reason = "已标记为需要人工接管"
			}
		}
		report.Nodes[id] = entry
	}
	return report
}
