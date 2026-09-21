package planner

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// WindowLayout 是维护窗口起止时间的解析格式（按指定时区解释的本地时间）。
const WindowLayout = "2006-01-02T15:04"

// Node 描述一个迁移节点。
type Node struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	DependsOn         []string `json:"depends_on"`
	Resources         []string `json:"resources"`
	DurationMinutes   int      `json:"duration_minutes"`
	Rollbackable      bool     `json:"rollbackable"`
	RequiresWindow    bool     `json:"requires_window"`
	AppliesTo         []string `json:"applies_to"`
	OutputFingerprint string   `json:"output_fingerprint"`
}

// Window 是一个维护窗口，Start/End 为时区本地时间。
type Window struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// PlanItem 是单个节点在某次规划中的落位结果。
type PlanItem struct {
	NodeID      string    `json:"node_id"`
	Wave        int       `json:"wave"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Reason      string    `json:"reason"`
	Blocked     bool      `json:"blocked"`
	BlockReason string    `json:"block_reason,omitempty"`
}

// Plan 是一次完整的波次规划，快照了当时的节点定义。
type Plan struct {
	ID          string     `json:"id"`
	CreatedAt   time.Time  `json:"created_at"`
	StartSchema string     `json:"start_schema"`
	Timezone    string     `json:"timezone"`
	Windows     []Window   `json:"windows"`
	Nodes       []Node     `json:"nodes"`
	Items       []PlanItem `json:"items"`
	WaveCount   int        `json:"wave_count"`
}

// CycleError 表示依赖图中存在环，Path 为一条具体环路。
type CycleError struct {
	Path []string
}

func (e *CycleError) Error() string {
	return "依赖图存在环: " + strings.Join(e.Path, " -> ")
}

func sortedIDs(nodes []Node) []string {
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	sort.Strings(ids)
	return ids
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// FindCycle 返回一条具体环路（首尾重复同一节点），无环返回 nil。
func FindCycle(nodes []Node) []string {
	deps := map[string][]string{}
	for _, n := range nodes {
		deps[n.ID] = append([]string(nil), n.DependsOn...)
	}
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var cycle []string
	var visit func(id string) bool
	visit = func(id string) bool {
		color[id] = gray
		stack = append(stack, id)
		for _, d := range sortedStrings(deps[id]) {
			if _, ok := deps[d]; !ok {
				continue
			}
			switch color[d] {
			case gray:
				idx := 0
				for i, s := range stack {
					if s == d {
						idx = i
						break
					}
				}
				cycle = append(append([]string(nil), stack[idx:]...), d)
				return true
			case white:
				if visit(d) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return false
	}
	for _, id := range sortedIDs(nodes) {
		if color[id] == white {
			if visit(id) {
				return cycle
			}
		}
	}
	return nil
}

// topoOrder 返回确定性的拓扑序（Kahn，按 ID 字典序取就绪节点）。
func topoOrder(nodes []Node) []string {
	indeg := map[string]int{}
	dependents := map[string][]string{}
	for _, n := range nodes {
		indeg[n.ID] = len(n.DependsOn)
		for _, d := range n.DependsOn {
			dependents[d] = append(dependents[d], n.ID)
		}
	}
	var ready []string
	for _, id := range sortedIDs(nodes) {
		if indeg[id] == 0 {
			ready = append(ready, id)
		}
	}
	var order []string
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for _, dep := range sortedStrings(dependents[id]) {
			indeg[dep]--
			if indeg[dep] == 0 {
				ready = append(ready, dep)
			}
		}
	}
	return order
}

type parsedWindow struct {
	start time.Time
	end   time.Time
}

// Build 生成一次波次规划。相同输入（节点集合、起始指纹、时区、窗口、基准时间）
// 必然得到相同的波次划分与同波次内排序。
func Build(nodes []Node, startSchema, tzName string, windows []Window, now time.Time) (*Plan, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("没有登记任何迁移节点")
	}
	byID := map[string]Node{}
	for _, n := range nodes {
		if n.ID == "" {
			return nil, fmt.Errorf("存在缺少 ID 的节点")
		}
		if _, dup := byID[n.ID]; dup {
			return nil, fmt.Errorf("节点 ID 重复: %s", n.ID)
		}
		byID[n.ID] = n
	}
	for _, n := range nodes {
		for _, d := range n.DependsOn {
			if _, ok := byID[d]; !ok {
				return nil, fmt.Errorf("节点 %s 依赖了不存在的节点 %s", n.ID, d)
			}
		}
	}
	if cyc := FindCycle(nodes); len(cyc) > 0 {
		return nil, &CycleError{Path: cyc}
	}
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return nil, fmt.Errorf("无法识别时区 %q: %w", tzName, err)
	}
	var wins []parsedWindow
	for _, w := range windows {
		s, err := time.ParseInLocation(WindowLayout, w.Start, loc)
		if err != nil {
			return nil, fmt.Errorf("维护窗口开始时间 %q 无法按 %s 解析", w.Start, tzName)
		}
		e, err := time.ParseInLocation(WindowLayout, w.End, loc)
		if err != nil {
			return nil, fmt.Errorf("维护窗口结束时间 %q 无法按 %s 解析", w.End, tzName)
		}
		if !e.After(s) {
			return nil, fmt.Errorf("维护窗口结束必须晚于开始: %s ~ %s", w.Start, w.End)
		}
		wins = append(wins, parsedWindow{start: s, end: e})
	}
	sort.Slice(wins, func(i, j int) bool { return wins[i].start.Before(wins[j].start) })

	ids := sortedIDs(nodes)

	// 适用性：AppliesTo 为空表示适用于所有 schema 指纹。
	blockReason := map[string]string{}
	for _, id := range ids {
		n := byID[id]
		if len(n.AppliesTo) > 0 && !contains(n.AppliesTo, startSchema) {
			blockReason[id] = fmt.Sprintf("当前 schema 指纹 %q 不在适用范围 [%s]",
				startSchema, strings.Join(n.AppliesTo, ", "))
		}
	}

	// 拓扑层级：level = max(前置 level) + 1；前置被阻塞则本节点阻塞。
	level := map[string]int{}
	for _, id := range topoOrder(nodes) {
		if blockReason[id] != "" {
			continue
		}
		n := byID[id]
		lv := 0
		for _, d := range sortedStrings(n.DependsOn) {
			if blockReason[d] != "" {
				blockReason[id] = fmt.Sprintf("前置节点 %s 被阻塞（%s）", d, blockReason[d])
				break
			}
			if level[d]+1 > lv {
				lv = level[d] + 1
			}
		}
		if blockReason[id] == "" {
			level[id] = lv
		}
	}
	maxLevel := 0
	for _, lv := range level {
		if lv > maxLevel {
			maxLevel = lv
		}
	}

	// 波次分配：同层节点按 ID 字典序处理，互斥资源冲突者顺延到下一可用波次。
	waveOf := map[string]int{}
	pushedNote := map[string]string{}
	used := map[int]map[string]string{} // wave -> resource -> holder
	for lv := 0; lv <= maxLevel; lv++ {
		for _, id := range ids {
			l, ok := level[id]
			if !ok || l != lv {
				continue
			}
			n := byID[id]
			w := lv
			var conflictRes, conflictHolder string
			for {
				conflictRes, conflictHolder = "", ""
				for _, r := range sortedStrings(n.Resources) {
					if h, taken := used[w][r]; taken {
						conflictRes, conflictHolder = r, h
						break
					}
				}
				if conflictRes == "" {
					break
				}
				w++
			}
			waveOf[id] = w
			if used[w] == nil {
				used[w] = map[string]string{}
			}
			for _, r := range n.Resources {
				used[w][r] = id
			}
			if w > lv {
				pushedNote[id] = fmt.Sprintf("与节点 %s 争抢互斥资源 %q，顺延至第 %d 波",
					conflictHolder, conflictRes, w+1)
			}
		}
	}
	maxWave := 0
	for _, w := range waveOf {
		if w > maxWave {
			maxWave = w
		}
	}

	// 时间排布：波次顺序执行，波内并行；含维护窗口节点的波次必须整体落入某个窗口。
	waveStart := map[int]time.Time{}
	waveDur := map[int]time.Duration{}
	waveBlocked := map[int]string{}
	waitedForWindow := map[int]bool{}
	cursor := now
	for w := 0; w <= maxWave; w++ {
		var members []string
		for _, id := range ids {
			if wv, ok := waveOf[id]; ok && wv == w {
				members = append(members, id)
			}
		}
		if len(members) == 0 {
			continue
		}
		durMin := 0
		needWindow := false
		for _, id := range members {
			if byID[id].DurationMinutes > durMin {
				durMin = byID[id].DurationMinutes
			}
			if byID[id].RequiresWindow {
				needWindow = true
			}
		}
		d := time.Duration(durMin) * time.Minute
		start := cursor
		if needWindow {
			placed := false
			for _, wn := range wins {
				if !wn.end.After(cursor) {
					continue
				}
				s := cursor
				if wn.start.After(s) {
					s = wn.start
				}
				if !s.Add(d).After(wn.end) {
					start = s
					placed = true
					break
				}
			}
			if !placed {
				waveBlocked[w] = fmt.Sprintf("没有能容纳该波次（%d 分钟）的维护窗口", durMin)
				continue
			}
			if start.After(cursor) {
				waitedForWindow[w] = true
			}
		}
		waveStart[w] = start
		waveDur[w] = d
		cursor = start.Add(d)
	}

	plan := &Plan{
		CreatedAt:   now.UTC(),
		StartSchema: startSchema,
		Timezone:    tzName,
		Windows:     append([]Window(nil), windows...),
	}
	for _, id := range ids {
		plan.Nodes = append(plan.Nodes, byID[id])
	}
	for _, id := range ids {
		item := PlanItem{NodeID: id}
		switch {
		case blockReason[id] != "":
			item.Blocked = true
			item.BlockReason = blockReason[id]
		case waveBlocked[waveOf[id]] != "":
			item.Blocked = true
			item.BlockReason = waveBlocked[waveOf[id]]
		default:
			w := waveOf[id]
			n := byID[id]
			item.Wave = w
			item.Start = waveStart[w]
			item.End = waveStart[w].Add(time.Duration(n.DurationMinutes) * time.Minute)
			item.Reason = buildReason(n, w, pushedNote[id], waitedForWindow[w], loc)
		}
		plan.Items = append(plan.Items, item)
	}
	plan.WaveCount = maxWave + 1
	return plan, nil
}

func buildReason(n Node, wave int, pushed string, waited bool, loc *time.Location) string {
	var parts []string
	if len(n.DependsOn) == 0 {
		parts = append(parts, "无前置依赖，可进入首个波次")
	} else {
		parts = append(parts, fmt.Sprintf("依赖 [%s] 均安排在此前波次", strings.Join(sortedStrings(n.DependsOn), ", ")))
	}
	if pushed != "" {
		parts = append(parts, pushed)
	}
	if n.RequiresWindow {
		if waited {
			parts = append(parts, "需要维护窗口，已等待至窗口开启")
		} else {
			parts = append(parts, "需要维护窗口，已落入窗口内")
		}
	}
	return fmt.Sprintf("第 %d 波：%s", wave+1, strings.Join(parts, "；"))
}
