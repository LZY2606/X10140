package model

import "time"

// Status values for migration nodes during execution.
const (
	StatusPending    = "未开始"
	StatusRunning    = "运行中"
	StatusSucceeded  = "成功"
	StatusFailed     = "失败"
	StatusRolledBack = "已回滚"
	StatusManual     = "需要人工接管"
)

// Node is a registered migration step definition.
type Node struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	DependsOn      []string      `json:"dependsOn"`
	MutexResources []string      `json:"mutexResources"`
	Duration       time.Duration `json:"duration"`
	Rollbackable   bool          `json:"rollbackable"`
	// InputFingerprint is the schema fingerprint this node expects before running.
	// Empty means "accept whatever the upstream produced".
	InputFingerprint string `json:"inputFingerprint"`
	// OutputFingerprint is the schema fingerprint this node declares it produces
	// after successful execution. Used for drift detection on recovery.
	OutputFingerprint string    `json:"outputFingerprint"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

// PlanRequest selects the starting schema fingerprint and the maintenance window.
type PlanRequest struct {
	StartFingerprint string    `json:"startFingerprint"`
	WindowStart      time.Time `json:"windowStart"`
	WindowEnd        time.Time `json:"windowEnd"`
}

// BlockedNode explains why a node cannot be scheduled in any wave.
type BlockedNode struct {
	NodeID  string   `json:"nodeId"`
	Reasons []string `json:"reasons"`
}

// Wave is a set of nodes that may execute concurrently.
type Wave struct {
	Index   int       `json:"index"`
	NodeIDs []string  `json:"nodeIds"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
}

// Plan is the immutable output of one planning run.
type Plan struct {
	ID          string      `json:"id"`
	RequestedAt time.Time   `json:"requestedAt"`
	Request     PlanRequest `json:"request"`
	// NodeSnapshot keeps the node definitions used for this plan, so later edits
	// never rewrite a historical plan.
	NodeSnapshot map[string]Node `json:"nodeSnapshot"`
	Waves        []Wave          `json:"waves"`
	Blocked      []BlockedNode   `json:"blocked"`
	// Explanation per node: why it is in this wave or why it is blocked.
	Explanations map[string][]string `json:"explanations"`
}

// Event is an append-only planning or execution record.
type Event struct {
	Seq               int           `json:"seq"`
	Time              time.Time     `json:"time"`
	Type              string        `json:"type"`
	IdempotencyKey    string        `json:"idempotencyKey,omitempty"`
	PlanID            string        `json:"planId,omitempty"`
	NodeID            string        `json:"nodeId,omitempty"`
	Status            string        `json:"status,omitempty"`
	OutputFingerprint string        `json:"outputFingerprint,omitempty"`
	Detail            string        `json:"detail,omitempty"`
	Plan              *Plan         `json:"plan,omitempty"`
	Node              *Node         `json:"node,omitempty"`
	DeletedNodeID     string        `json:"deletedNodeId,omitempty"`
	Duration          time.Duration `json:"duration,omitempty"`
}

// Event types.
const (
	EvNodeUpserted   = "node_upserted"
	EvNodeDeleted    = "node_deleted"
	EvPlanCreated    = "plan_created"
	EvNodeStarted    = "node_started"
	EvNodeSucceeded  = "node_succeeded"
	EvNodeFailed     = "node_failed"
	EvNodeRolledBack = "node_rolled_back"
	EvCrashSimulated = "crash_simulated"
)
