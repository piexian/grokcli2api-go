# 后端任务：修复分页伪实现 + 审计可观测性（第四批）

## 背景

`docs/backend-review-01-scale.md` 发现当前分页是**伪实现**：每页仍调用 `Pool.Credentials()` 全量快照+排序，32 页累计处理 512k 对象（近似 O(n²log n/pageSize)）。同时审计有若干过载/可观测性问题。**只改 Go 后端，不碰 `frontend/`。**

分支：`feat/admin-console`。先读 `docs/backend-review-01-scale.md` 全文再动手。

## 任务一：真分页（阻塞级）

**问题**：`internal/server/admin_credentials.go` 的 `listCredentials` 每页都做全量 `Pool.Credentials()` + `sort.Slice`。

**修复**（选一，取最简单正确的）：
1. **方案 A（推荐）**：在 `Pool` 内维护一个按 generation 发布的**不可变有序脱敏快照**。
   - Pool 已有 `generation` 概念（账号增删改时递增）。
   - 新增 `Pool.CredentialSnapshot() (generation uint64, infos []CredentialInfo)`：内部缓存上次 generation 的快照，generation 不变则直接返回缓存（只读共享，不要深拷贝）；变化时才重建。
   - 快照构建时一次排序；后续所有分页/过滤请求复用，直到 generation 变化。
   - handler 的 cursor 里编码 generation；请求间 generation 变化时**降级行为**：用新快照重新过滤（接受 keyset 边界不一致，文档注明），不要报错。
2. **方案 B**：只物化命中页。维护有序账号 ID 列表（Pool 已按 ID 排序 accounts），handler 用游标定位起始 index，只对 `[start, start+limit]` 调用 `credentialInfo()`；`total` 用过滤计数（需要全扫但不物化）。

**要求**：1.6 万账号、500/页，**32 页总耗时 < 200ms**（当前是秒级）。补**真实端到端 benchmark**：`BenchmarkAdminCredentialsHandler_16k`（构造真实 Pool + httptest + JSON 编码），p95 延迟写入 bench 注释。

## 任务二：审计过载可观测性（高）

1. **队列容量可配置**：`GROK_AUDIT_QUEUE_SIZE`（默认 4096）。
2. **丢弃计数**：`Store` 暴露 `Dropped() uint64`（原子），`GET /v1/admin/audits/health` 返回 `{"queue_size":n,"queue_cap":cap,"dropped_total":n,"oldest_pending_ms":n}`。
3. **模型名长度限制**：进入 Capture 前把 `model` 截断到 256 字符（防 16MiB 请求体放大）。
4. **批量写失败重试**：Commit 成功才清 batch；失败时退避重试 3 次（100ms/500ms/2s），最终失败计入 `dropped_total`。

## 任务三：聚合与 retention（高）

1. **Summary 短 TTL 缓存**：`Store` 内对 summary/dashboard 聚合结果做 10s 缓存（按 `period` key），避免管理台轮询重算全窗口。
2. **Retention 分批**：`DELETE ... WHERE created_at < ? LIMIT 500` 循环，每批后 `time.Sleep(10ms)` 让出 writer；设独立 5min 超时。

## 约束

- 遵循现有风格；配置走 `internal/config`（含测试）。
- 所有改动必须有测试：真分页的 generation 失效、benchmark 数字、审计丢弃计数、retention 分批。
- `go build ./... && go vet ./... && go test ./... && go test -race ./internal/audit ./internal/server` 全绿（sandbox 限制 httptest.NewServer 的用例可跳过，用 `testing.Short()` 或环境检测）。
- 提交：一个逻辑改动一个 commit：
  - `fix: true keyset pagination via pool credential snapshot`
  - `feat: audit queue observability and overload hardening`
  - `feat: audit summary caching and batched retention`

## 验收

1. `BenchmarkAdminCredentialsHandler_16k` 32 页总耗时 < 200ms。
2. `curl /v1/admin/audits/health` 返回队列状态。
3. 审计队列打满后 `dropped_total` 增加且健康端点可见。
4. Summary 连续两次请求第二次命中缓存（日志或测试断言）。