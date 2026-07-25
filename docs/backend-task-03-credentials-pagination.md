# 后端任务：凭证列表分页与过滤（第三批）

## 背景

管理台前端已上线，实测 jp 生产环境 `GET /v1/admin/credentials` 返回 **10156 条 / 3.2MB / 3.5s**。前端虚拟滚动已正确，但每次全量拉取成为唯一瓶颈。需要为凭证列表加分页与服务端过滤。**只改 Go 后端，不碰 `frontend/`。**

分支：`feat/admin-console`。

## 需求

### `GET /v1/admin/credentials` 增强（向后兼容）

新增**可选**查询参数：

| 参数 | 类型 | 说明 |
|---|---|---|
| `limit` | int | 每页条数（默认 0 = 返回全部，保持现状兼容旧调用方；上限 1000） |
| `cursor` | string | 上一页返回的 `next_cursor`（不透明，建议基于最后一条的 id；无需支持随机跳页） |
| `q` | string | 搜索词，对 `id`、`scope`、`subscription_tier_display`、`models[]` 做不区分大小写子串匹配 |
| `status` | string | 过滤状态：`ready` / `cooling_down` / `disabled` / `needs_refresh` / `pending_models` |
| `usable` | string | `"true"` / `"false"` 过滤可用性 |

### 响应格式（向后兼容）

- **无分页参数时**：保持现状 `{"object":"list","data":[...]}`（全量）。
- **带 `limit` 时**：
  ```json
  {
    "object": "list",
    "data": [...],
    "has_more": true,
    "next_cursor": "xxx",
    "total": 10156
  }
  ```
  - `total` 为过滤后的总数（用于前端显示"共 N 个"）。
  - `next_cursor` 仅在 `has_more=true` 时非空。

### 实现约束

- 排序固定为 **id ASC**（稳定，游标基于最后一条 id）。
- 1.6 万账号下，单次过滤+分页必须在 **50ms 内**完成（当前 `pool.Credentials()` 是一次性快照，过滤在内存中做即可，不要做额外上游调用）。
- `q` 的 `models[]` 匹配：只要任一模型包含搜索词即命中。
- 保持现有脱敏（不返回 subject/path/token）。

## 测试

- 分页遍历：limit=100 循环取直到 `has_more=false`，能取回全部且 id 无重复、有序。
- `q` 过滤：命中 id/scope/tier/models 四种字段。
- `status`/`usable` 过滤。
- 向后兼容：不传参数时响应结构与现状一致（无 `has_more`/`next_cursor`/`total` 字段，或这些字段为零值且旧 JSON 解析不受影响）。
- 1.6 万条内存过滤耗时基准测试（BenchmarkListCredentials）。

## 验收

1. `go build ./... && go vet ./... && go test ./...` 全绿。
2. `curl '/v1/admin/credentials?limit=100'` 返回 100 条 + `has_more` + `total`。
3. `curl '/v1/admin/credentials?q=grok-4&limit=10'` 只返回模型匹配的。
4. 提交：commit message `feat: paginate and filter admin credentials list`。
