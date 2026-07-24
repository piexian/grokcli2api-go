# 后端任务：请求审计与用量统计（第一批）

## 背景

grokcli2api-go 是一个 Go 编写的 Grok→OpenAI/Anthropic 协议代理。当前 Admin API 仅有 `GET/POST/DELETE /v1/admin/credentials`。我们要为它补一个类似 grok2api 的管理台后端。**你（Codex）只负责 Go 后端代码，绝对不要碰 `frontend/` 目录**（前端由另一个 agent 实现）。

分支：`feat/admin-console`（已基于合并 upstream 的 main 创建）。请在该分支上直接提交。

## 任务一：请求审计（request audits）

对每一个进入 `/v1/chat/completions`、`/v1/responses`、`/v1/messages` 的请求，在请求生命周期结束后记录一条审计日志。

### 存储

- 使用 **SQLite**（纯 Go 驱动 `modernc.org/sqlite`，避免 CGO），数据库文件路径通过环境变量 `GROK_AUDIT_DB` 配置，默认 `<auths_dir>/audit.db`。
- 表 `request_audits` 字段（JSON 列不要用，全部平铺）：
  - `id` TEXT PRIMARY KEY（uuid）
  - `request_id` TEXT（内部请求 id）
  - `created_at` INTEGER（unix 毫秒）
  - `protocol` TEXT（`chat` / `responses` / `messages`）
  - `model` TEXT（请求的模型 id）
  - `account_id` TEXT（实际使用的脱敏凭证 id，失败未取得账号则为空）
  - `tenant_key` TEXT（API key 派生的 tenant namespace，server 已有此概念）
  - `status_code` INTEGER（最终返回给客户端的 HTTP 状态码）
  - `streaming` INTEGER（0/1）
  - `duration_ms` INTEGER
  - `input_tokens` INTEGER、`cached_input_tokens` INTEGER、`output_tokens` INTEGER、`reasoning_tokens` INTEGER、`total_tokens` INTEGER（从上游 usage 提取，没有则 0）
  - `error_code` TEXT（失败时的错误码，可空）
  - `attempt_count` INTEGER（重试次数）
- 建立索引：`(created_at)`、`(model, created_at)`、`(account_id, created_at)`。
- 保留策略：环境变量 `GROK_AUDIT_RETENTION_DAYS`（默认 30），启动时及每 24h 清理一次过期数据。
- 写入必须异步（带缓冲 channel + 批量 insert），**不得在请求热路径上阻塞**；服务关闭时 flush。

### 查询 API（admin key 鉴权，复用现有 `adminKeyGate`）

1. `GET /v1/admin/audits?cursor=&limit=&model=&account_id=&protocol=&status=&since=&until=`
   - 游标分页（created_at DESC + id 作为 cursor），默认 limit 50，最大 200。
   - 响应：`{"items":[...],"page_size":50,"next_cursor":"...","has_more":true}`
2. `GET /v1/admin/audits/summary?period=today|7d|30d` （period 默认 today）
   - 聚合：`requests / successful_requests / failed_requests / input_tokens / cached_input_tokens / output_tokens / reasoning_tokens / total_tokens / average_duration_ms / success_rate`，以及 `by_model`（每个模型的上述聚合）和 `series`（按小时或按天的桶，用于趋势图，period=today 按小时，其余按天）。

## 任务二：Dashboard 聚合 API

`GET /v1/admin/dashboard?period=today|7d|30d`，响应：

```json
{
  "period": "today",
  "generated_at": "RFC3339",
  "range": {"start": "...", "end": "..."},
  "resources": {
    "total_accounts": 0,
    "usable_accounts": 0,
    "disabled_accounts": 0,
    "cooling_accounts": 0,
    "paid_accounts": 0,
    "total_models": 0
  },
  "usage": { 同 summary 的聚合字段 },
  "by_model": [...],
  "series": [...]
}
```

- `resources` 从 `auth.Pool.Credentials()` 派生（已有 tier/disabled/cooldown 信息，注意 pool 有 1.6 万账号，不要在此接口做 O(n) 以上操作；现状 Credentials() 本身是一次快照，可接受）。
- `usage/by_model/series` 复用任务一的审计聚合查询。

## 约束与风格

- 遵循现有代码风格：stdlib `http.ServeMux`、`writeJSON`/`writeError` 辅助、`slog` 日志、配置走 `internal/config`（`Load()` + `GROK_` 前缀环境变量 + 校验 + `config_test.go` 同步补测试）。
- 审计记录点在 `internal/server`：建议在 `Handler()` 的中间件链或 chat/responses/messages handler 外层包一层 recorder，注意流式响应要在流结束后才写记录（可包装 ResponseWriter / 在 stream* 函数返回处收尾）。
- token 提取：非流式从上游响应 usage 字段；流式从最终 chunk 的 usage（现有 stream 代码已解析，找最合适的注入点；若上游没有 usage 则记 0，不要估算）。
- **向后兼容**：未配置新环境变量时行为与现状一致（审计功能默认开启但 DB 落到默认路径；若 `GROK_AUDIT_DB=off` 则完全禁用）。
- 所有新代码必须有测试：SQLite 存储层 CRUD/聚合、HTTP API 分页/过滤/summary、审计中间件对成功/失败/流式请求的记录。运行 `go test ./...` 必须全绿。
- 提交：完成任务后在 `feat/admin-console` 分支提交，commit message 用英文，格式 `feat: request audit storage and admin query APIs`。
- 不要修改 `frontend/`、`auths/`、`dist/`、`docs/` 之外的无关文件；不要动 `.env*`。

## 验收

1. `go build ./... && go vet ./... && go test ./...` 全绿。
2. 启动服务后 `GET /v1/admin/audits` 与 `GET /v1/admin/dashboard` 带 admin key 可返回（空库也要返回合法空结构，不是报错）。
3. 发一个真实 `/v1/chat/completions` 请求后能在 audits 中查到记录且 token 字段非负。
