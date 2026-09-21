package model

import (
	"time"
)

type Node struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Prereqs   []string `json:"prereqs"`
	Resources []string `json:"resources"`
	Minutes   int      `json:"minutes"`
	Rollback  bool     `json:"rollback"`
	Inputs    []string `json:"inputs"`
	Output    string   `json:"output"`
}

type Window struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Zone  string `json:"zone"`
}

type PlanNode struct {
	Node
	Wave      int    `json:"wave"`
	StartMin  int64  `json:"startMin"`
	FinishMin int64  `json:"finishMin"`
	Note      string `json:"note"`
}

type Wave struct {
	Index    int         `json:"index"`
	StartMin int64       `json:"startMin"`
	NodeIDs  []string    `json:"nodeIds"`
	Nodes    []*PlanNode `json:"nodes"`
}

type Plan struct {
	ID       string        `json:"id"`
	Created  string        `json:"created"`
	StartFP  string        `json:"startFingerprint"`
	Window   Window        `json:"window"`
	Nodes    []*PlanNode   `json:"nodes"`
	Waves    []*Wave       `json:"waves"`
	Blocked  []BlockedNode `json:"blocked"`
	NodeByID map[string]*PlanNode
}

type BlockedNode struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// ExecStatus enumerates the six required execution states.
type ExecStatus string

const (
	StatusPending    ExecStatus = "pending"
	StatusRunning    ExecStatus = "running"
	StatusSuccess    ExecStatus = "success"
	StatusFailed     ExecStatus = "failed"
	StatusRolledBack ExecStatus = "rolled_back"
	StatusManual     ExecStatus = "manual"
)

type ExecEventType string

const (
	EventStarted   ExecEventType = "started"
	EventSucceeded ExecEventType = "succeeded"
	EventFailed    ExecEventType = "failed"
	EventRolled    ExecEventType = "rolled_back"
	EventManual    ExecEventType = "manual_takeover"
)

type ExecEvent struct {
	PlanID   string        `json:"planId"`
	NodeID   string        `json:"nodeId"`
	Type     ExecEventType `json:"type"`
	IdemKey  string        `json:"idemKey"`
	ActualFP string        `json:"actualFingerprint,omitempty"`
	Detail   string        `json:"detail,omitempty"`
	At       string        `json:"at"`
	Seq      int64         `json:"seq"`
}

type NodeEvent struct {
	Type   string     `json:"type"`
	At     string     `json:"at"`
	Node   *Node      `json:"node,omitempty"`
	NodeID string     `json:"nodeId,omitempty"`
	PlanID string     `json:"planId,omitempty"`
	Plan   *Plan      `json:"plan,omitempty"`
	Exec   *ExecEvent `json:"exec,omitempty"`
	Seq    int64      `json:"seq"`
}

// ParsedWindow is a Window interpreted in its IANA timezone.
type ParsedWindow struct {
	Location *time.Location
	Start    time.Time
	End      time.Time
}
