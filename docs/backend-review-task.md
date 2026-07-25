# 后端审查任务：1.6 万账号规模的性能与正确性

## 背景

管理台后端已完成三批功能（审计/SPA 托管/凭证分页）。jp 生产环境有 **10156 个账号**（目标支撑 1.6 万+）。请对**当前 `feat/admin-console` 分支的后端代码**做一次专项审查，**只读代码、输出报告，不修改任何文件**。

## 审查目标

逐条检查以下场景在 1.6 万账号规模下的表现，给出**具体文件:行号**的证据和**严重度评级**（阻塞/高/中/低）：

### 1. Admin API 性能
- `GET /v1/admin/credentials`（无分页参数时全量返回）：`pool.Credentials()` 快照的内存分配、JSON 序列化耗时；1.6 万账号单次响应大小估算。
- 分页路径：过滤+游标的实现是否有隐藏 O(n²)（如每次分页都重新 sort）；`q` 搜索对 `models[]` 的匹配是否会重复分配大字符串。
- `GET /v1/admin/dashboard`：`resources` 从 `pool.Credentials()` 派生的开销；审计聚合 SQL 在审计表增长到几十万行后的耗时（索引是否覆盖 `by_model`/`series` 聚合）。
- `GET /v1/admin/audits` 游标分页：SQL 是否走了 `(created_at)` 索引，还是全表扫描。

### 2. 审计写入路径
- 审计中间件对**每个推理请求**的开销：是否有同步 DB 写入；缓冲 channel 满了会怎样（阻塞推理？丢弃？）。
- SQLite 批量 insert 的事务大小、WAL 模式、synchronous 设置；1.6 万账号高并发推理时的写入吞吐估算。
- 保留清理（retention）的 DELETE 是否会锁表阻塞读取。

### 3. 并发与竞态
- `pool.Credentials()` 在 1.6 万账号时持有锁的时长；是否会阻塞调度（Acquire/Release）。
- 审计 store 与 server 关闭顺序：race（请求进行中关闭服务，审计 flush 是否会 panic）。
- 流式请求的审计记录：stream 中途 client 断开，记录是否正确收尾（status_code/duration）。

### 4. 内存与资源
- 审计缓冲 channel 的容量与最坏内存占用（1.6 万账号洪峰）。
- embed SPA 是否在每次请求都重新读 embed FS（应缓存）。
- runtime-config.js 每次请求是否重复 `json.Marshal`（可缓存）。

### 5. 正确性回归
- 分页游标的边界：空结果、最后一页、cursor 指向已删除的 id。
- `q` 搜索对 `models[]` 大小写、`nil` models 的处理。
- 向后兼容：无参数响应的 JSON 字段集合是否与 main 分支完全一致（不能有新增字段，否则旧前端解析失败）。

## 输出格式

```markdown
## 阻塞级（必须立即修）
- [文件:行] 问题描述 / 影响 / 建议修法

## 高（影响 1.6 万规模体验）
...

## 中/低（可选优化）
...

## 总体结论
- 当前代码能否支撑 1.6 万账号生产使用？有哪些必须先修的前置项？
```

把报告写到 `docs/backend-review-01-scale.md` 并提交（只提交报告，不改代码）。