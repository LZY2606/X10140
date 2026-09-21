package scheduler

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"migplanner/internal/graph"
	"migplanner/internal/model"
)

type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func errf(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

// Result is a generated plan without persistence metadata.
type Result struct {
	Nodes   []*model.PlanNode
	Waves   []*model.Wave
	Blocked []model.BlockedNode
}

const clockLayout = "2006-01-02T15:04"

// ParseWindow interprets start/end clock times in the given IANA zone.
// RFC3339 inputs are accepted as well (their own offset is preserved).
func ParseWindow(w model.Window) (*model.ParsedWindow, error) {
	if w.Zone == "" {
		return nil, errf("必须指定维护窗口时区")
	}
	loc, err := time.LoadLocation(w.Zone)
	if err != nil {
		return nil, errf("无效时区 %q: %v", w.Zone, err)
	}
	parse := func(s string) (time.Time, error) {
		if t, e := time.ParseInLocation(clockLayout, s, loc); e == nil {
			return t, nil
		}
		if t, e := time.Parse(time.RFC3339, s); e == nil {
			return t, nil
		}
		return time.Time{}, errf("时间格式无效: %q（需要 %s 或 RFC3339）", s, clockLayout)
	}
	st, err := parse(w.Start)
	if err != nil {
		return nil, err
	}
	en, err := parse(w.End)
	if err != nil {
		return nil, err
	}
	if !en.After(st) {
		return nil, errf("维护窗口结束必须晚于开始（%s -> %s）", st.Format(clockLayout), en.Format(clockLayout))
	}
	return &model.ParsedWindow{Location: loc, Start: st, End: en}, nil
}

type waveInfo struct {
	resources map[string]string // resource -> owner node id
	length    int               // longest node duration in the wave
}

type placed struct {
	node     *model.PlanNode
	wave     int
	eligible bool
	reason   string
	depth    int
}

// Build validates the graph and schedules executable waves.
func Build(nodes []*model.Node, startFP string, win model.Window) (*Result, error) {
	if len(nodes) == 0 {
		return nil, errf("没有任何迁移节点")
	}
	byID := map[string]*model.Node{}
	for _, n := range nodes {
		if strings.TrimSpace(n.ID) == "" {
			return nil, errf("存在没有 ID 的节点")
		}
		if _, dup := byID[n.ID]; dup {
			return nil, errf("节点 ID 重复: %s", n.ID)
		}
		if n.Minutes < 0 {
			return nil, errf("节点 %s 预计时长不能为负", n.ID)
		}
		byID[n.ID] = n
	}
	if bad := graph.UnknownPrereqs(nodes); len(bad) > 0 {
		return nil, errf("前置依赖指向不存在的节点: %s", strings.Join(bad, ", "))
	}
	if err := graph.DetectCycle(nodes); err != nil {
		return nil, err
	}
	pw, err := ParseWindow(win)
	if err != nil {
		return nil, err
	}
	winMinutes := int(pw.End.Sub(pw.Start) / time.Minute)

	topo, err := topoOrder(nodes)
	if err != nil {
		return nil, err
	}

	pl := map[string]*placed{}
	var blocked []model.BlockedNode
	addBlocked := func(id, reason string) {
		for _, b := range blocked {
			if b.ID == id {
				return
			}
		}
		blocked = append(blocked, model.BlockedNode{ID: id, Reason: reason})
	}

	// Eligibility: start-fingerprint match for roots, output/input chain
	// match for every prerequisite edge.
	for _, id := range topo {
		n := byID[id]
		p := &placed{node: &model.PlanNode{Node: *n, Wave: -1}}
		pl[id] = p

		if len(n.Prereqs) == 0 {
			if len(n.Inputs) == 0 || contains(n.Inputs, startFP) {
				p.eligible = true
				p.depth = 0
				p.reason = fmt.Sprintf("根节点无前置；起始指纹 %q 命中其可接受输入 %s，放入波次 0",
					startFP, fpList(n.Inputs))
			} else {
				p.reason = fmt.Sprintf("起始指纹 %q 不在可接受输入 %s 中，当前 schema 下不适用",
					startFP, fpList(n.Inputs))
				addBlocked(id, p.reason)
			}
			continue
		}

		var complaints []string
		maxDepth := 0
		for _, pre := range n.Prereqs {
			pp := pl[pre]
			if pp.depth > maxDepth {
				maxDepth = pp.depth
			}
			if !pp.eligible {
				complaints = append(complaints, pre+"（"+short(pp.reason)+"）")
				continue
			}
			if !contains(n.Inputs, pp.node.Output) {
				complaints = append(complaints,
					fmt.Sprintf("前置 %s 的输出指纹 %q 不在本节点可接受输入 %s 中", pre, pp.node.Output, fpList(n.Inputs)))
			}
		}
		p.depth = maxDepth + 1
		if len(complaints) > 0 {
			p.reason = "前置链不可用：" + strings.Join(complaints, "；")
			addBlocked(id, p.reason)
			continue
		}
		p.eligible = true
		var refs []string
		for _, pre := range n.Prereqs {
			refs = append(refs, pre)
		}
		p.reason = fmt.Sprintf("前置 %s 均在更早波次完成，且其输出指纹在可接受输入 %s 中",
			strings.Join(refs, "、"), fpList(n.Inputs))
	}

	// Stable placement order: dependency depth asc, then id asc.
	order := append([]string{}, topo...)
	sort.SliceStable(order, func(i, j int) bool {
		a, b := pl[order[i]], pl[order[j]]
		if a.depth != b.depth {
			return a.depth < b.depth
		}
		return order[i] < order[j]
	})

	var waves []*waveInfo
	// assignWave places the node in the first wave >= minWave without a
	// mutex-resource collision, and explains any postponement.
	assignWave := func(id string, minWave int, res []string) (int, string) {
		for w := minWave; ; w++ {
			for w >= len(waves) {
				waves = append(waves, &waveInfo{resources: map[string]string{}})
			}
			owner := ""
			for _, r := range res {
				if o, ok := waves[w].resources[r]; ok {
					owner = o
					break
				}
			}
			if owner == "" {
				for _, r := range res {
					waves[w].resources[r] = id
				}
				if w == minWave {
					return w, ""
				}
				for pw2 := minWave; pw2 < w; pw2++ {
					for _, r := range res {
						if o, ok := waves[pw2].resources[r]; ok {
							return w, fmt.Sprintf("按依赖最早可入波次 %d；互斥资源 %s 在波次 %d 已被 %s 占用，顺延至波次 %d",
								minWave, r, pw2, o, w)
						}
					}
				}
				return w, ""
			}
		}
	}

	for _, id := range order {
		p := pl[id]
		if !p.eligible {
			continue
		}
		minWave := 0
		for _, pre := range p.node.Prereqs {
			if w := pl[pre].wave + 1; w > minWave {
				minWave = w
			}
		}
		w, extra := assignWave(id, minWave, p.node.Resources)
		p.wave = w
		p.node.Wave = w
		if extra != "" {
			p.reason += "；" + extra
		}
		if p.node.Minutes > waves[w].length {
			waves[w].length = p.node.Minutes
		}
	}

	// Timeline: waves run sequentially, nodes inside a wave in parallel.
	waveStarts := map[int]int{}
	cursor := 0
	for w, info := range waves {
		waveStarts[w] = cursor
		cursor += info.length
	}
	endAt := func(m int) string {
		return pw.Start.Add(time.Duration(m) * time.Minute).In(pw.Location).Format("01-02 15:04")
	}
	windowEnd := pw.End.In(pw.Location).Format("01-02 15:04")
	windowStart := pw.Start.In(pw.Location).Format("01-02 15:04")

	var outNodes []*model.PlanNode
	outWaves := map[int]*model.Wave{}
	for _, id := range order {
		p := pl[id]
		if !p.eligible {
			outNodes = append(outNodes, p.node)
			continue
		}
		start := waveStarts[p.wave]
		finish := start + p.node.Minutes
		p.node.StartMin = int64(start)
		p.node.FinishMin = int64(finish)
		p.node.Note = p.reason
		outNodes = append(outNodes, p.node)

		wv := outWaves[p.wave]
		if wv == nil {
			wv = &model.Wave{Index: p.wave, StartMin: int64(start)}
			outWaves[p.wave] = wv
		}
		wv.NodeIDs = append(wv.NodeIDs, id)
		wv.Nodes = append(wv.Nodes, p.node)
	}

	// Maintenance window (both endpoints interpreted in the zone):
	// a wave that starts at/after window end cannot run; a node that would
	// finish after the end is blocked (finishing exactly at end is allowed).
	for w, wv := range outWaves {
		if waveStarts[w] >= winMinutes {
			for _, id := range wv.NodeIDs {
				p := pl[id]
				p.node.Note += fmt.Sprintf("；维护窗口阻塞：波次最早第 %d 分钟（%s）启动，窗口 %s~%s（%d 分钟）内无法开始",
					waveStarts[w], endAt(waveStarts[w]), windowStart, windowEnd, winMinutes)
				addBlocked(id, fmt.Sprintf("维护窗口长度不足：波次 %d 最早在第 %d 分钟（%s）开始，窗口 %s~%s 共 %d 分钟",
					w, waveStarts[w], endAt(waveStarts[w]), windowStart, windowEnd, winMinutes))
			}
			continue
		}
		for _, id := range wv.NodeIDs {
			p := pl[id]
			if int(p.node.FinishMin) > winMinutes {
				p.node.Note += fmt.Sprintf("；维护窗口阻塞：预计第 %d 分钟（%s）完成，超过窗口结束 %s；恰好在结束时刻完成允许",
					p.node.FinishMin, endAt(int(p.node.FinishMin)), windowEnd)
				addBlocked(id, fmt.Sprintf("维护窗口长度不足：节点在第 %d 分钟（%s）完成，超过窗口结束 %s（共 %d 分钟）；结束端点恰好完成允许",
					p.node.FinishMin, endAt(int(p.node.FinishMin)), windowEnd, winMinutes))
			}
		}
	}

	var waveList []*model.Wave
	for w := 0; w < len(waves); w++ {
		if wv, ok := outWaves[w]; ok {
			waveList = append(waveList, wv)
		}
	}
	sort.SliceStable(blocked, func(i, j int) bool { return blocked[i].ID < blocked[j].ID })
	return &Result{Nodes: outNodes, Waves: waveList, Blocked: blocked}, nil
}

func topoOrder(nodes []*model.Node) ([]string, error) {
	indeg := map[string]int{}
	succ := map[string][]string{}
	ids := map[string]struct{}{}
	for _, n := range nodes {
		ids[n.ID] = struct{}{}
	}
	for _, n := range nodes {
		for _, p := range n.Prereqs {
			if _, ok := ids[p]; ok {
				indeg[n.ID]++
				succ[p] = append(succ[p], n.ID)
			}
		}
	}
	var ready []string
	for id := range ids {
		sort.Strings(succ[id])
		if indeg[id] == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	var order []string
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for _, s := range succ[id] {
			indeg[s]--
			if indeg[s] == 0 {
				ready = append(ready, s)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(ids) {
		return nil, errf("依赖图存在环，无法完成拓扑排序")
	}
	return order, nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func fpList(fps []string) string {
	if len(fps) == 0 {
		return "（空=接受任意）"
	}
	return "[" + strings.Join(fps, ", ") + "]"
}

func short(s string) string {
	if i := strings.Index(s, "；"); i >= 0 {
		return s[:i]
	}
	return s
}
