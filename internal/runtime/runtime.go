package runtime

import (
	"fmt"
	"sort"
	"strings"

	"migplanner/internal/model"
)

// NodeView is the projected execution state of one plan node.
type NodeView struct {
	Node     *model.PlanNode    `json:"node"`
	Status   model.ExecStatus   `json:"status"`
	ActualFP string             `json:"actualFingerprint,omitempty"`
	Detail   string             `json:"detail,omitempty"`
	Events   []*model.ExecEvent `json:"events"`
	Runnable bool               `json:"runnable"`
	Blocked  bool               `json:"blocked"`
	Reason   string             `json:"reason"`
	Handled  bool               `json:"handled"` // drifted or interrupted -> manual
}

type View struct {
	Plan      *model.Plan          `json:"plan"`
	Nodes     map[string]*NodeView `json:"nodes"`
	Order     []string             `json:"order"`
	Events    []*model.ExecEvent   `json:"events"`
	Recovered bool                 `json:"recovered"`
	Recovery  *RecoveryReport      `json:"recovery"`
}

type RecoveryReport struct {
	Interrupted  []string `json:"interrupted"`
	Skipped      []string `json:"skipped"`
	Drifted      []string `json:"drifted"`
	ChainBlocked []string `json:"chainBlocked"`
	Notes        []string `json:"notes"`
}

// Project replays execution events on top of the immutable plan snapshot.
// recovered=true simulates restarting after a crash: any node left "running"
// has an unknown outcome and is forced to manual takeover.
func Project(plan *model.Plan, events []*model.ExecEvent, recovered bool) *View {
	v := &View{Plan: plan, Nodes: map[string]*NodeView{}, Events: []*model.ExecEvent{},
		Recovered: recovered, Order: []string{}}
	v.Events = append(v.Events, events...)

	blockedByPlan := map[string]string{}
	for _, b := range plan.Blocked {
		blockedByPlan[b.ID] = b.Reason
	}
	for _, pn := range plan.Nodes {
		v.Order = append(v.Order, pn.ID)
		v.Nodes[pn.ID] = &NodeView{Node: pn, Status: model.StatusPending, Events: []*model.ExecEvent{}}
	}
	sort.Strings(v.Order)

	// Replay events in append (sequence) order.
	for _, e := range events {
		nv, ok := v.Nodes[e.NodeID]
		if !ok {
			continue
		}
		switch e.Type {
		case model.EventStarted:
			nv.Status = model.StatusRunning
		case model.EventSucceeded:
			nv.Status = model.StatusSuccess
			nv.ActualFP = e.ActualFP
			nv.Detail = e.Detail
		case model.EventFailed:
			nv.Status = model.StatusFailed
			nv.Detail = e.Detail
		case model.EventRolled:
			nv.Status = model.StatusRolledBack
			nv.Detail = e.Detail
		case model.EventManual:
			nv.Status = model.StatusManual
			nv.Detail = e.Detail
		}
		nv.Events = append(nv.Events, e)
	}

	rep := &RecoveryReport{Interrupted: []string{}, Skipped: []string{},
		Drifted: []string{}, ChainBlocked: []string{}, Notes: []string{}}
	// Crash recovery: running nodes have unknown real-world outcomes.
	if recovered {
		for _, id := range v.Order {
			nv := v.Nodes[id]
			if nv.Status == model.StatusRunning {
				nv.Status = model.StatusManual
				nv.Handled = true
				nv.Blocked = true
				nv.Detail = "崩溃恢复：节点中断时处于运行中，真实结果未知，需要人工接管确认"
				rep.Interrupted = append(rep.Interrupted, id)
			}
		}
	}

	// Fingerprint drift: a finished node whose actual output differs from
	// the plan snapshot must not be re-run; it and every successor chain
	// become blocked and require manual takeover.
	for _, id := range v.Order {
		nv := v.Nodes[id]
		if nv.Status == model.StatusSuccess && nv.ActualFP != "" && nv.ActualFP != nv.Node.Output {
			nv.Blocked = true
			nv.Handled = true
			if nv.Status != model.StatusManual {
				nv.Status = model.StatusManual
			}
			nv.Reason = fmt.Sprintf("输出指纹漂移：实际 %q 与计划快照期望 %q 不符；禁止重跑，需要人工接管",
				nv.ActualFP, nv.Node.Output)
			rep.Drifted = append(rep.Drifted, id)
		}
	}

	// Propagate blocker reasons along successor edges. A node is blocked if
	// it was blocked at planning time or any prerequisite is blocked / not
	// successfully done. Successful nodes with matching fingerprints are
	// never re-run after interruption.
	successors := map[string][]string{}
	for _, pn := range plan.Nodes {
		for _, pre := range pn.Prereqs {
			successors[pre] = append(successors[pre], pn.ID)
		}
	}
	// Propagate in stable topological (wave then id) order.
	for _, id := range v.sortedByWave() {
		nv := v.Nodes[id]
		if reason, ok := blockedByPlan[id]; ok {
			nv.Blocked = true
			nv.Reason = joinReason(nv.Reason, "规划阶段已阻塞："+reason)
		}
		var predIssues []string
		for _, pre := range nv.Node.Prereqs {
			pv := v.Nodes[pre]
			if pv == nil {
				continue
			}
			if pv.Blocked || pv.Handled {
				predIssues = append(predIssues, pre+"（"+stateText(pv)+"）")
			} else if pv.Status == model.StatusRolledBack {
				predIssues = append(predIssues, pre+"（已回滚，前置成果不存在）")
			} else if pv.Status == model.StatusFailed {
				predIssues = append(predIssues, pre+"（失败）")
			}
		}
		if len(predIssues) > 0 {
			nv.Blocked = true
			nv.Reason = joinReason(nv.Reason, "后继链阻塞：前置 "+strings.Join(predIssues, "、")+" 未提供可信成果")
			if recovered && !contains(rep.ChainBlocked, id) {
				rep.ChainBlocked = append(rep.ChainBlocked, id)
			}
		}
	}

	// Runability for the next step.
	for _, id := range v.Order {
		nv := v.Nodes[id]
		if nv.Blocked || nv.Handled {
			continue
		}
		if nv.Status == model.StatusSuccess {
			nv.Reason = joinReason(nv.Reason, fmt.Sprintf("已成功完成，实际指纹 %q 与计划匹配，恢复时不会重跑", fpOr(nv)))
			continue
		}
		if nv.Status != model.StatusPending {
			continue
		}
		ready := true
		var waiting []string
		for _, pre := range nv.Node.Prereqs {
			pv := v.Nodes[pre]
			if pv == nil {
				continue
			}
			if pv.Status != model.StatusSuccess {
				ready = false
				waiting = append(waiting, pre+"（"+statusLabel(pv.Status)+"）")
			}
		}
		if ready {
			nv.Runnable = true
			if nv.Reason == "" {
				nv.Reason = "前置均已成功且指纹匹配，可以开始"
			}
		} else {
			nv.Reason = joinReason(nv.Reason, "等待前置完成："+strings.Join(waiting, "、"))
		}
	}

	if recovered {
		rep.Skipped = append(rep.Skipped, v.successfulMatching()...)
		v.Recovery = rep
	}
	return v
}

func (v *View) sortedByWave() []string {
	ids := append([]string{}, v.Order...)
	sort.SliceStable(ids, func(i, j int) bool {
		a, b := v.Nodes[ids[i]].Node, v.Nodes[ids[j]].Node
		if a.Wave != b.Wave {
			return a.Wave < b.Wave
		}
		return ids[i] < ids[j]
	})
	return ids
}

func (v *View) successfulMatching() []string {
	var out []string
	for _, id := range v.Order {
		nv := v.Nodes[id]
		if nv.Status == model.StatusSuccess && (nv.ActualFP == "" || nv.ActualFP == nv.Node.Output) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func stateText(nv *NodeView) string {
	if nv.Reason != "" {
		return short(nv.Reason)
	}
	return statusLabel(nv.Status)
}

func statusLabel(s model.ExecStatus) string {
	switch s {
	case model.StatusPending:
		return "未开始"
	case model.StatusRunning:
		return "运行中"
	case model.StatusSuccess:
		return "成功"
	case model.StatusFailed:
		return "失败"
	case model.StatusRolledBack:
		return "已回滚"
	case model.StatusManual:
		return "需要人工接管"
	}
	return string(s)
}

func joinReason(a, b string) string {
	if a == "" {
		return b
	}
	return a + "；" + b
}

func short(s string) string {
	if i := strings.Index(s, "；"); i >= 0 {
		return s[:i]
	}
	return s
}

func fpOr(nv *NodeView) string {
	if nv.ActualFP != "" {
		return nv.ActualFP
	}
	return nv.Node.Output
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// TransitionError names an illegal state transition.
type TransitionError struct{ Msg string }

func (e *TransitionError) Error() string { return e.Msg }

// ValidateTransition checks a legal event against the current projected
// status.
func ValidateTransition(cur model.ExecStatus, ev model.ExecEventType) error {
	ok := map[model.ExecStatus]map[model.ExecEventType]bool{
		model.StatusPending:    {model.EventStarted: true, model.EventManual: true},
		model.StatusRunning:    {model.EventSucceeded: true, model.EventFailed: true, model.EventManual: true},
		model.StatusFailed:     {model.EventStarted: true, model.EventRolled: true, model.EventManual: true},
		model.StatusSuccess:    {model.EventRolled: true, model.EventManual: true},
		model.StatusRolledBack: {model.EventStarted: true, model.EventManual: true},
		model.StatusManual:     {},
	}
	if !ok[cur][ev] {
		return &TransitionError{Msg: fmt.Sprintf("非法状态转换：%s 状态下不能提交 %s",
			statusLabel(cur), eventLabel(ev))}
	}
	return nil
}

func eventLabel(ev model.ExecEventType) string {
	switch ev {
	case model.EventStarted:
		return "开始"
	case model.EventSucceeded:
		return "成功"
	case model.EventFailed:
		return "失败"
	case model.EventRolled:
		return "回滚"
	case model.EventManual:
		return "人工接管"
	}
	return string(ev)
}
