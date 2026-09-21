package planner

import "sort"

// FindCycle 在依赖图中寻找环，找到时返回一条具体环路（首尾相同），否则返回 nil。
func FindCycle(nodes []Node) []string {
	ids := map[string]bool{}
	for _, n := range nodes {
		ids[n.ID] = true
	}
	graph := map[string][]string{}
	for _, n := range nodes {
		deps := append([]string{}, n.Deps...)
		sort.Strings(deps)
		for _, d := range deps {
			if ids[d] {
				graph[n.ID] = append(graph[n.ID], d)
			}
		}
	}
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var cycle []string
	var dfs func(u string) bool
	dfs = func(u string) bool {
		color[u] = gray
		stack = append(stack, u)
		for _, v := range graph[u] {
			switch color[v] {
			case gray:
				idx := 0
				for i, s := range stack {
					if s == v {
						idx = i
						break
					}
				}
				cycle = append([]string{}, stack[idx:]...)
				cycle = append(cycle, v)
				return true
			case white:
				if dfs(v) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[u] = black
		return false
	}
	var sorted []string
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	for _, id := range sorted {
		if color[id] == white && dfs(id) {
			return cycle
		}
	}
	return nil
}

// FindCycleFrom 返回从 from 出发可达的环（若有），否则返回 nil。
func FindCycleFrom(nodes []Node, from string) []string {
	byID := map[string]Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	// 收集 from 的可达子图
	reachable := map[string]bool{}
	var mark func(id string)
	mark = func(id string) {
		if reachable[id] {
			return
		}
		reachable[id] = true
		for _, d := range byID[id].Deps {
			mark(d)
		}
	}
	mark(from)
	var sub []Node
	for _, n := range nodes {
		if reachable[n.ID] {
			sub = append(sub, n)
		}
	}
	return FindCycle(sub)
}

// descendantsOf 构建 节点 -> 直接后继 的邻接表。
func descendantsOf(nodes map[string]Node) map[string][]string {
	children := map[string][]string{}
	for _, n := range nodes {
		for _, d := range n.Deps {
			children[d] = append(children[d], n.ID)
		}
	}
	return children
}

// ancestors 返回 id 的全部祖先（不含自身）。
func ancestors(nodes map[string]Node, id string) map[string]bool {
	out := map[string]bool{}
	var walk func(cur string)
	walk = func(cur string) {
		n, ok := nodes[cur]
		if !ok {
			return
		}
		for _, d := range n.Deps {
			if !out[d] {
				out[d] = true
				walk(d)
			}
		}
	}
	walk(id)
	return out
}

// descendants 返回 id 的全部后继（不含自身）。
func descendants(nodes map[string]Node, id string) map[string]bool {
	children := descendantsOf(nodes)
	out := map[string]bool{}
	var walk func(cur string)
	walk = func(cur string) {
		for _, ch := range children[cur] {
			if !out[ch] {
				out[ch] = true
				walk(ch)
			}
		}
	}
	walk(id)
	return out
}

// topologicalLayers 返回每个节点的拓扑层（无依赖为 0）。调用前需保证无环。
func topologicalLayers(nodes []Node) map[string]int {
	byID := map[string]Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	layers := map[string]int{}
	var layerOf func(id string) int
	layerOf = func(id string) int {
		if l, ok := layers[id]; ok {
			return l
		}
		mx := -1
		for _, d := range byID[id].Deps {
			if _, ok := byID[d]; ok {
				if l := layerOf(d); l > mx {
					mx = l
				}
			}
		}
		l := mx + 1
		layers[id] = l
		return l
	}
	for _, n := range nodes {
		layerOf(n.ID)
	}
	return layers
}
