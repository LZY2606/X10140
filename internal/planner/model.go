package planner

import (
	"fmt"
	"strings"
	"time"
)

// Node 描述一个迁移节点。
type Node struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Deps              []string `json:"deps"`      // 前置节点 ID
	Resources         []string `json:"resources"` // 互斥资源
	DurationMin       int      `json:"duration_min"`
	Rollbackable      bool     `json:"rollbackable"`
	AppliesTo         []string `json:"applies_to"` // 适用的当前 schema 指纹，空表示不限
	OutputFingerprint string   `json:"output_fingerprint"`
	RequiresWindow    bool     `json:"requires_window"`
}

// WindowSpec 维护窗口，时间按 Timezone 解释。
type WindowSpec struct {
	Start    string `json:"start"`
	End      string `json:"end"`
	Timezone string `json:"timezone"`
}

// PlanRequest 一次规划的输入。
type PlanRequest struct {
	StartSchema string      `json:"start_schema"`
	StartTime   string      `json:"start_time"`
	Window      *WindowSpec `json:"window,omitempty"`
}

// PlannedNode 已排入波次的节点。
type PlannedNode struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Resources   []string  `json:"resources"`
	DurationMin int       `json:"duration_min"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Reason      string    `json:"reason"`
}

// Wave 一个可执行波次，波次内节点并行。
type Wave struct {
	Index int           `json:"index"`
	Start time.Time     `json:"start"`
	End   time.Time     `json:"end"`
	Nodes []PlannedNode `json:"nodes"`
}

// BlockedNode 被阻塞的节点及原因。
type BlockedNode struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Plan 一次规划的不可变快照。
type Plan struct {
	ID        string            `json:"id"`
	CreatedAt time.Time         `json:"created_at"`
	Request   PlanRequest       `json:"request"`
	Nodes     map[string]Node   `json:"nodes"` // 节点定义快照
	Waves     []*Wave           `json:"waves"`
	Blocked   []BlockedNode     `json:"blocked"`
}

// CycleError 依赖图中存在环。
type CycleError struct {
	Path []string
}

func (e *CycleError) Error() string {
	return "依赖图存在环: " + strings.Join(e.Path, " -> ")
}

// NodeStatus 执行状态。
type NodeStatus string

const (
	StatusPending     NodeStatus = "pending"
	StatusRunning     NodeStatus = "running"
	StatusSuccess     NodeStatus = "success"
	StatusFailed      NodeStatus = "failed"
	StatusRolledBack  NodeStatus = "rolled_back"
	StatusNeedsManual NodeStatus = "needs_manual"
)

// Event 一条执行记录，Key 为幂等键。
type Event struct {
	Key               string     `json:"key"`
	PlanID            string     `json:"plan_id"`
	NodeID            string     `json:"node_id"`
	Status            NodeStatus `json:"status"`
	OutputFingerprint string     `json:"output_fingerprint,omitempty"`
	Time              time.Time  `json:"time"`
}

func parseTimeIn(s, tz string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("时间为空")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	loc := time.Local
	if tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return time.Time{}, fmt.Errorf("未知时区 %q", tz)
		}
		loc = l
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析时间 %q", s)
}
