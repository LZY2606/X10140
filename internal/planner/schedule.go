package planner

import (
	"fmt"
	"sort"
	"time"
)

type rawWave struct {
	ids       []string
	resources map[string]string // 资源 -> 占用节点
}

// BuildPlan 根据节点定义与规划输入生成可执行波次。
// 依赖图存在环时返回 *CycleError，包含一条具体环路。
func BuildPlan(nodes []Node, req PlanRequest, planID string, now time.Time) (*Plan, error) {
	if cyc := FindCycle(nodes); cyc != nil {
		return nil, &CycleError{Path: cyc}
	}
	tz := ""
	if req.Window != nil {
		tz = req.Window.Timezone
	}
	startTime, err := parseTimeIn(req.StartTime, tz)
	if err != nil {
		return nil, fmt.Errorf("起始时间: %w", err)
	}
	var winStart, winEnd time.Time
	hasWindow := req.Window != nil
	if hasWindow {
		winStart, err = parseTimeIn(req.Window.Start, tz)
		if err != nil {
			return nil, fmt.Errorf("维护窗口开始: %w", err)
		}
		winEnd, err = parseTimeIn(req.Window.End, tz)
		if err != nil {
			return nil, fmt.Errorf("维护窗口结束: %w", err)
		}
		if !winEnd.After(winStart) {
			return nil, fmt.Errorf("维护窗口结束必须晚于开始")
		}
	}

	nodeByID := map[string]Node{}
	snapshot := map[string]Node{}
	for _, n := range nodes {
		nodeByID[n.ID] = n
		snapshot[n.ID] = n
	}

	// 拓扑层：layer = 1 + max(layer(dep))
	layer := map[string]int{}
	var layerOf func(id string) int
	layerOf = func(id string) int {
		if l, ok := layer[id]; ok {
			return l
		}
		n := nodeByID[id]
		mx := -1
		for _, d := range n.Deps {
			if _, ok := nodeByID[d]; ok {
				if l := layerOf(d); l > mx {
					mx = l
				}
			}
		}
		l := mx + 1
		layer[id] = l
		return l
	}

	blocked := map[string]string{}
	children := descendantsOf(snapshot)
	blockSubtree := func(root, reason string) {
		queue := []string{root}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, ch := range children[cur] {
				if _, ok := blocked[ch]; !ok {
					blocked[ch] = reason
					queue = append(queue, ch)
				}
			}
		}
	}

	// 规划前阻塞：schema 指纹不适用、前置缺失
	for _, n := range nodes {
		if len(n.AppliesTo) > 0 && !contains(n.AppliesTo, req.StartSchema) {
			blocked[n.ID] = fmt.Sprintf("当前 schema 指纹 %q 不在适用列表 %v 中", req.StartSchema, n.AppliesTo)
			continue
		}
		for _, d := range n.Deps {
			if _, ok := nodeByID[d]; !ok {
				blocked[n.ID] = fmt.Sprintf("前置节点 %q 不存在", d)
				break
			}
		}
	}
	// 传播到后继
	for _, n := range nodes {
		if r, ok := blocked[n.ID]; ok {
			blockSubtree(n.ID, fmt.Sprintf("前置链上节点 %s 被阻塞（%s）", n.ID, r))
		}
	}

	// 可运行节点按 (layer, id) 稳定排序
	var runnable []Node
	for _, n := range nodes {
		if _, ok := blocked[n.ID]; !ok {
			runnable = append(runnable, n)
		}
	}
	sort.Slice(runnable, func(i, j int) bool {
		li, lj := layerOf(runnable[i].ID), layerOf(runnable[j].ID)
		if li != lj {
			return li < lj
		}
		return runnable[i].ID < runnable[j].ID
	})

	// 波次分配：前置所在波次之后、互斥资源不并发的最早波次
	var raw []*rawWave
	assigned := map[string]int{}
	conflict := map[string][2]string{} // id -> (资源, 冲突节点)
	for _, n := range runnable {
		minW := 0
		for _, d := range n.Deps {
			if w, ok := assigned[d]; ok && w+1 > minW {
				minW = w + 1
			}
		}
		w := minW
		for {
			for len(raw) <= w {
				raw = append(raw, &rawWave{resources: map[string]string{}})
			}
			badRes, badNode := "", ""
			ok := true
			for _, r := range n.Resources {
				if other, used := raw[w].resources[r]; used {
					ok, badRes, badNode = false, r, other
					break
				}
			}
			if ok {
				break
			}
			conflict[n.ID] = [2]string{badRes, badNode}
			w++
		}
		raw[w].ids = append(raw[w].ids, n.ID)
		for _, r := range n.Resources {
			raw[w].resources[r] = n.ID
		}
		assigned[n.ID] = w
	}

	// 时间轴：逐波推进，处理维护窗口与窗口内阻塞
	cursor := startTime
	var waves []*Wave
	for _, rw := range raw {
		var active []Node
		for _, id := range rw.ids {
			if _, ok := blocked[id]; ok {
				continue
			}
			active = append(active, nodeByID[id])
		}
		if len(active) == 0 {
			continue
		}
		waveStart := cursor
		needsWindow := false
		for _, n := range active {
			if n.RequiresWindow {
				needsWindow = true
			}
		}
		if needsWindow && hasWindow && waveStart.Before(winStart) {
			waveStart = winStart
		}
		var kept []Node
		for _, n := range active {
			if n.RequiresWindow {
				if !hasWindow {
					blocked[n.ID] = "该步骤需要维护窗口，但本次规划未提供维护窗口"
					blockSubtree(n.ID, fmt.Sprintf("前置链上节点 %s 被阻塞（%s）", n.ID, blocked[n.ID]))
					continue
				}
				end := waveStart.Add(time.Duration(n.DurationMin) * time.Minute)
				if end.After(winEnd) {
					blocked[n.ID] = fmt.Sprintf("需要维护窗口：预计 %s 结束，超出窗口结束 %s（%s）",
						end.Format("15:04"), winEnd.Format("15:04"), tzName(tz))
					blockSubtree(n.ID, fmt.Sprintf("前置链上节点 %s 被阻塞（%s）", n.ID, blocked[n.ID]))
					continue
				}
			}
			kept = append(kept, n)
		}
		if len(kept) == 0 {
			continue
		}
		wave := &Wave{Index: len(waves) + 1, Start: waveStart}
		waveEnd := waveStart
		for _, n := range kept {
			end := waveStart.Add(time.Duration(n.DurationMin) * time.Minute)
			wave.Nodes = append(wave.Nodes, PlannedNode{
				ID:          n.ID,
				Name:        n.Name,
				Resources:   n.Resources,
				DurationMin: n.DurationMin,
				Start:       waveStart,
				End:         end,
			})
			if end.After(waveEnd) {
				waveEnd = end
			}
		}
		wave.End = waveEnd
		waves = append(waves, wave)
		cursor = waveEnd
	}

	// 生成每个节点的排布解释
	for _, wave := range waves {
		for i := range wave.Nodes {
			pn := &wave.Nodes[i]
			n := nodeByID[pn.ID]
			reason := fmt.Sprintf("第 %d 波", wave.Index)
			if len(n.Deps) == 0 {
				reason += "：无前置依赖，可最早执行"
			} else {
				reason += fmt.Sprintf("：前置依赖 %v 均排在更早波次", n.Deps)
			}
			if c, ok := conflict[pn.ID]; ok {
				reason += fmt.Sprintf("；互斥资源 %q 与节点 %s 冲突，延后到本波", c[0], c[1])
			}
			if n.RequiresWindow {
				reason += "；需在维护窗口内执行"
			}
			pn.Reason = reason
		}
	}

	var blockedList []BlockedNode
	for id, reason := range blocked {
		blockedList = append(blockedList, BlockedNode{ID: id, Name: nodeByID[id].Name, Reason: reason})
	}
	sort.Slice(blockedList, func(i, j int) bool { return blockedList[i].ID < blockedList[j].ID })

	return &Plan{
		ID:        planID,
		CreatedAt: now,
		Request:   req,
		Nodes:     snapshot,
		Waves:     waves,
		Blocked:   blockedList,
	}, nil
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func tzName(tz string) string {
	if tz == "" {
		return "本地时区"
	}
	return tz
}
