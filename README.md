# 迁移依赖规划器（Migration Dependency Planner）

一个从零实现的 Go Web 服务：登记数据库迁移节点（前置依赖、互斥资源、预计
时长、可回滚性、适用的当前 schema 指纹），生成可执行波次，解释每个节点
为何在该波次或为何阻塞；并可逐步记录执行、模拟崩溃恢复、生成安全的回滚
计划。不接入真实数据库，所有数据追加写入项目目录下的 JSONL 事件日志。

## 安装检查

```sh
go build ./...
```

## 演示

```sh
go test ./... -count=1
go run ./cmd/server --addr 127.0.0.1:5222
# 打开 http://127.0.0.1:5222 ，页面标题为「迁移依赖规划器」
```

首次启动会写入 6 个演示节点（含互斥资源、不可回滚节点、指纹不匹配节点）。
数据默认落在 `./data/events.jsonl`，可用 `--data <目录>` 修改。

## 能力对照

- 依赖图：DFS 着色检测环，返回一条具体闭合环路（如 `a -> b -> c -> a`），
  前置指向不存在的节点也会被拒绝（`internal/graph`）。
- 波次生成：拓扑深度给出最早波次；同一互斥资源在同一波次不得并发，冲突
  节点顺延并在节点说明里写明占用者（`internal/scheduler`）。
- 维护窗口：开始/结束时间按 IANA 时区解释（也接受 RFC3339）；波次串行、
  波内并行；恰好在结束时刻完成允许，晚于结束或无法在结束前启动的节点
  明确阻塞并给出原因。
- 稳定调度：同深度按节点 ID 升序，波内顺序按 ID 升序；相同输入多次生成
  结果一致。
- 指纹适用性：根节点必须匹配所选起始 schema 指纹；每条前置边的输出指纹
  必须在后继节点的可接受输入集合中；不满足的节点及其后继链不进入波次。
- 执行状态：未开始 / 运行中 / 成功 / 失败 / 已回滚 / 需要人工接管，状态
  转换在服务端校验。
- 崩溃恢复（勾选页面复选框模拟）：中断时仍为运行中的节点结果未知，转为
  人工接管；已成功且实际输出指纹与计划快照一致的节点跳过不重跑；指纹
  漂移的节点禁止重跑并使其整条后继链阻塞。
- 回滚计划：只包含已执行且声明可回滚的祖先，按执行逆序排列；遇到已执行
  但不可回滚、或结果未知/人工接管的祖先立即明确停止，绝不输出看似完整
  的方案（`internal/runtime/rollback.go`）。
- 追加日志：节点保存/删除、计划创建（快照当前定义）、每次执行事件都
  追加到 `events.jsonl` 并 fsync；历史计划不可变，后来改节点定义不会
  重写历史；执行事件按「计划 ID + 幂等键」去重，重复提交只记一次。

## HTTP API

- `GET  /api/state`：当前节点定义、历史计划、数据文件路径
- `POST /api/nodes` / `DELETE /api/nodes?id=`：登记/删除节点（新版本）
- `POST /api/plans`：body `{startFingerprint, window:{start,end,zone}}`
- `GET  /api/plans/{id}/view?recovered=1`：执行投影（含恢复报告）
- `POST /api/plans/{id}/events`：body 含 `nodeId,type,idemKey,
  actualFingerprint`；重复幂等键/非法转换/前置未满足返回 409
- `GET  /api/plans/{id}/rollback?node=<失败节点>`：逆序回滚计划或停止边界
- `GET  /api/events`：原始追加事件日志（NDJSON）

## 目录结构

```
cmd/server        HTTP 入口与演示种子数据
internal/model    领域模型
internal/graph    环检测、未知前置、拓扑顺序
internal/scheduler 波次调度、互斥资源、时区窗口、指纹适用性
internal/store    仅追加 JSONL 存储、重放、幂等去重
internal/runtime  状态投影、崩溃恢复、漂移阻塞链、回滚边界
internal/web      HTTP API 与内嵌单页前端（static/index.html）
```
