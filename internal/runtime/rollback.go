package runtime

import (
	"fmt"
	"sort"

	"migplanner/internal/model"
)

type RollbackStep struct {
	NodeID  string `json:"nodeId"`
	Wave    int    `json:"wave"`
	Minutes int    `json:"minutes"`
	Reason  string `json:"reason"`
}

type RollbackPlan struct {
	Target         string         `json:"target"`
	Steps          []RollbackStep `json:"steps"`
	Complete       bool           `json:"complete"`
	StoppedAt      string         `json:"stoppedAt,omitempty"`
	BoundaryReason string         `json:"boundaryReason,omitempty"`
	Warnings       []string       `json:"warnings"`
}

// PlanRollback builds the reverse rollback sequence for a failed node.
// Only executed ancestors declared rollback-able are included; traversal
// stops explicitly at the first non-rollbackable (or unknown-outcome)
// ancestor, so the result is never presented as a complete plan.
func PlanRollback(v *View, targetID string) (*RollbackPlan, error) {
	nv, ok := v.Nodes[targetID]
	if !ok {
		return nil, fmt.Errorf("计划中不存在节点 %s", targetID)
	}
	switch nv.Status {
	case model.StatusFailed:
	default:
		return nil, fmt.Errorf("节点 %s 当前为 %s，仅失败节点可生成回滚计划（需要人工接管者请先人工处置）",
			targetID, statusLabel(nv.Status))
	}
	if !nv.Node.Rollback || nv.Handled {
		return &RollbackPlan{
			Target:         targetID,
			Steps:          nil,
			Complete:       false,
			StoppedAt:      targetID,
			BoundaryReason: boundaryReason(nv),
		}, nil
	}

	plan := &RollbackPlan{Target: targetID, Complete: true}
	visited := map[string]bool{}
	cand := map[string]*NodeView{}
	var boundaries []*NodeView

	var visit func(id string)
	visit = func(id string) {
		if visited[id] {
			return
		}
		visited[id] = true
		cur := v.Nodes[id]

		pres := append([]string{}, cur.Node.Prereqs...)
		// Deterministic traversal: later wave first, then larger id.
		sort.SliceStable(pres, func(i, j int) bool {
			a, b := v.Nodes[pres[i]], v.Nodes[pres[j]]
			if a.Node.Wave != b.Node.Wave {
				return a.Node.Wave > b.Node.Wave
			}
			return pres[i] > pres[j]
		})
		for _, pre := range pres {
			pv := v.Nodes[pre]
			if pv == nil {
				continue
			}
			if !isExecuted(pv.Status) {
				continue
			}
			if pv.Status == model.StatusRolledBack {
				plan.Warnings = append(plan.Warnings,
					fmt.Sprintf("祖先 %s 已回滚，跳过", pre))
				continue
			}
			if pv.Status == model.StatusManual || pv.Handled || !pv.Node.Rollback {
				boundaries = append(boundaries, pv)
				continue // 明确停止，不再沿该祖先向上追溯
			}
			cand[pre] = pv
			visit(pre)
		}
	}

	cand[targetID] = nv
	visit(targetID)

	if len(boundaries) > 0 {
		plan.Complete = false
		sort.SliceStable(boundaries, func(i, j int) bool {
			a, b := boundaries[i], boundaries[j]
			if a.Node.Wave != b.Node.Wave {
				return a.Node.Wave > b.Node.Wave
			}
			return a.Node.ID > b.Node.ID
		})
		b := boundaries[0]
		plan.StoppedAt = b.Node.ID
		plan.BoundaryReason = boundaryReason(b)
		for _, extra := range boundaries[1:] {
			plan.Warnings = append(plan.Warnings,
				fmt.Sprintf("另有停止边界 %s：%s", extra.Node.ID, boundaryReason(extra)))
		}
	}

	// Reverse execution order: wave desc, then id desc.
	ids := make([]string, 0, len(cand))
	for id := range cand {
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool {
		a, b := v.Nodes[ids[i]].Node, v.Nodes[ids[j]].Node
		if a.Wave != b.Wave {
			return a.Wave > b.Wave
		}
		return ids[i] > ids[j]
	})
	for i, id := range ids {
		cn := v.Nodes[id]
		reason := ""
		if i == 0 {
			reason = "回滚目标本身（已执行且声明可回滚），最先撤销"
		} else {
			reason = fmt.Sprintf("已执行的可回滚祖先（波次 %d），按逆序撤销", cn.Node.Wave)
		}
		plan.Steps = append(plan.Steps, RollbackStep{
			NodeID: id, Wave: cn.Node.Wave, Minutes: cn.Node.Minutes, Reason: reason,
		})
	}
	return plan, nil
}

func isExecuted(s model.ExecStatus) bool {
	return s == model.StatusSuccess || s == model.StatusFailed || s == model.StatusManual
}

func boundaryReason(nv *NodeView) string {
	switch {
	case nv.Status == model.StatusManual || nv.Handled:
		return fmt.Sprintf("节点 %s 结果未知/已要求人工接管，不能自动回滚，回滚链在此明确停止", nv.Node.ID)
	case !nv.Node.Rollback:
		return fmt.Sprintf("节点 %s 已执行但声明不可回滚（前滚型变更），回滚链在此明确停止", nv.Node.ID)
	default:
		return fmt.Sprintf("节点 %s 不满足自动回滚条件", nv.Node.ID)
	}
}
