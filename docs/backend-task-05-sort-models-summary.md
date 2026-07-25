# 后端任务：凭证列表排序 + 模型聚合 API（第五批）

## 背景

管理台前端将切换为**无限滚动懒加载**（不再全量拉取），因此排序和模型聚合必须服务端完成。**只改 Go 后端，不碰 `frontend/`。**

分支：`feat/admin-console`。

## 任务一：`GET /v1/admin/credentials` 支持排序

新增可选参数（与现有 limit/cursor/q/status/usable 组合使用）：

| 参数 | 取值 | 说明 |
|---|---|---|
| `sort` | `id`（默认）/ `tier` / `status` / `expires_at` / `models_count` / `usable` | 排序字段 |
| `order` | `asc`（默认）/ `desc` | 方向 |

排序语义：
- `tier`：按订阅层级权重排序（Free < X Basic < X Premium < X Premium+ < SuperGrok < SuperGrok Heavy < SuperGrok Lite，未知层级排最后）。Pool 已有 tier 信息。
- `status`：按状态权重（ready < cooling_down < pending_models < needs_refresh < disabled）。
- `expires_at`：空值排最后。
- `models_count`：按模型数量。
- `usable`：usable=true 在 asc 时排前。

**游标兼容**：keyset 游标目前编码 id。排序字段变化时 keyset 语义会破坏——简化处理：cursor 里编码 `(sort, order, lastKey, lastID)` 复合键；校验请求的 sort/order 与 cursor 内一致，不一致返回 400。排序后的比较键为 `(sortKey, id)` 保证稳定。

**性能**：排序在 Pool 的 generation 快照上做（task-04 的快照已按 id 排序；其他字段排序在快照物化时缓存，不要每次请求重排）。1.6 万账号任意字段排序+单页过滤必须 < 50ms。

## 任务二：`GET /v1/admin/models/summary`

聚合每个模型的账号覆盖与状态分布（数据源：Pool 快照，O(n) 一次）：

```json
{
  "object": "list",
  "data": [
    {
      "model": "grok-4.5",
      "accounts": 10156,
      "usable_accounts": 9640,
      "status_counts": { "ready": 9640, "cooling_down": 510, "disabled": 6 }
    }
  ],
  "total_models": 1
}
```

- admin key 鉴权（复用 `adminKeyGate`）。
- 支持 `?q=` 过滤模型 id（子串，不区分大小写）。
- 1.6 万账号 < 30ms；随 generation 快照缓存失效。

## 约束

- 遵循现有风格；测试覆盖：各字段排序正确性、cursor 跨页稳定、sort 不一致 400、模型聚合计数、q 过滤。
- benchmark：排序分页 1.6 万账号单页 < 50ms。
- `go build ./... && go vet ./... && go test ./...` 全绿。
- 提交两个 commit：
  - `feat: sortable admin credentials list`
  - `feat: admin models summary API`

## 验收

1. `curl '/v1/admin/credentials?sort=tier&order=desc&limit=5'` 返回按层级降序。
2. `curl '/v1/admin/models/summary'` 返回聚合计数与状态分布。