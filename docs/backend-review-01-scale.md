# 后端专项审查：1.6 万账号规模

审查对象：`feat/admin-console`（HEAD `d3f0c06`）。本次只读审查后端实现，未修改 Go 源文件。规模估算以 16,000 个账号、凭证页大小 500、审计表几十万行起步为基线；账号数本身不等于推理 QPS，审计吞吐结论因此同时给出按请求速率换算的方法。

## 阻塞级（必须立即修）

- [`internal/server/server.go:243-251`, `internal/auth/pool.go:858-867`, `internal/server/admin_credentials.go:85-115`] 当前分页只限制返回条数，没有限制快照和排序成本：每一页都调用 `Pool.Credentials()` 构造全部账号的脱敏对象、复制每个账号的 `models`/`billing`、按 ID 排序，再由 `listCredentials` 做一次有序性扫描和全量过滤/计数。500 条一页遍历 16,000 个账号需要 32 次完整快照和 32 次排序，累计处理至少 512,000 个账号对象；固定页大小下总体复杂度接近 `O(n^2 log n / pageSize)`。这正是任务要求排除的“每页重新 sort”隐藏平方级路径，会把管理台渐进加载变成用更高后端 CPU/GC 换首屏更快。建议在 Pool 内维护按 generation 发布的不可变、有序脱敏快照（或维护有序账号 ID 并只物化命中页），游标携带/校验 generation；至少应避免每页重建和排序全部 `CredentialInfo`。
- [`internal/server/admin_credentials_test.go:232-249`] 现有 16,000 条基准只调用预先构造且已经排序的 `listCredentials`，没有覆盖 `Pool.Credentials()`、账号锁、模型/计费复制、排序、HTTP handler 和 JSON 编码，不能证明“单次过滤+分页 <50ms”。本机复测该窄基准为 `2.90-3.11ms/op`、`277,792 B/op`、`16,002 allocs/op`；这些分配主要来自逐条大小写归一化，真实 handler 只会更重。必须补一个覆盖真实 Pool 快照到 HTTP 响应的端到端 benchmark，并以 16,000 条验证 p95，而不是只测过滤函数。

## 高（影响 1.6 万规模体验）

- [`docs/backend-task-03-credentials-pagination.md:5`, `internal/server/server.go:250-251`, `internal/server/admin_credentials.go:91-107`, `internal/server/server.go:1000-1003`] 无分页兼容路径仍会产生高峰内存。生产实测 10,156 条为 3.2MB/3.5s，线性外推 16,000 条约 5.0MB。amd64 上 `CredentialInfo` 结构体约 208B，16,000 条 backing array 约 3.17MiB；`listCredentials` 又复制一份同等大小的 `page.Data`，`json.Encoder` 还会构造约 5MB 编码缓冲，故单请求瞬时堆占用下限约 11.4MiB，尚未计入每账号 `models` slice、`BillingInfo` 深拷贝和编码临时对象。并发数个全量请求即可制造明显 GC 压力。建议保留协议兼容但为无过滤/无分页增加零复制快路径，并让新管理台只使用真正的分页快照。
- [`internal/audit/store.go:19`, `internal/audit/store.go:117-136`, `internal/audit/store.go:198-248`] 审计热路径确实不阻塞推理，但 4,096 容量满后直接丢记录，仅写日志；若 16,000 个请求近同时结束、消费者尚未来得及腾空，理论上可丢约 11,904 条，Dashboard/审计查询将静默低估请求和 token。批量写为 100 条或 250ms 一批；要承载 `R` requests/s，单连接至少要持续完成约 `R/100` 个事务/s，队列只提供约 `4096/R` 秒缓冲。仓库没有写入吞吐、queue-full 或 dropped 指标测试。建议暴露 dropped 指标/健康状态、让队列容量可配置，并对持续过载提供磁盘 spool 或明确降级告警。
- [`internal/audit/types.go:13-30`, `internal/server/server.go:914-941`, `internal/server/audit_middleware.go:111-114`] channel 的结构体 backing array 下限约 0.75MiB（约 192B/record），正常模型名下总量通常只是数 MiB；但请求体允许 16MiB，`model` 只校验非空且原字符串被审计记录持有，严格最坏情况下 4,096 条队列可保留接近 64GiB 的模型字符串。这是可触发 OOM 的放大器。建议在进入 Capture 时把模型限制到协议允许长度，或只保存截断值/哈希。
- [`internal/audit/query.go:170-204`, `internal/audit/query.go:237-274`, `internal/audit/store.go:64-68`] 每次 summary/dashboard 都先同步 Flush，再串行执行总计、`GROUP BY model` 和计算时间桶的三次范围聚合；单 DB 连接使写入、清理和查询全部串行。SQLite `EXPLAIN QUERY PLAN` 显示 `by_model` 使用 `created_at` 范围索引后仍建临时 B-tree 做 GROUP BY 和 ORDER BY，`series` 也要临时 B-tree 分桶。几十万行时每次刷新至少多次遍历时间范围；16,000 账号哪怕每账号每小时 0.1 个请求，30 天也约 115 万行。建议按小时/天增量汇总，或对 summary 做短 TTL 缓存，避免每次管理台轮询重算全窗口。
- [`internal/audit/store.go:149-158`, `internal/audit/store.go:238-244`, `internal/audit/store.go:64-65`] retention 用一个无分批的 `DELETE WHERE created_at < ?`，并在审计消费 goroutine 内同步执行。虽然 WAL 允许不同连接的读写并发，但这里 `SetMaxOpenConns(1)`，所以大 DELETE 会占用唯一连接，同时停止消费队列；清理持续时间内 List/Summary 等待、队列继续积压并可能丢审计。建议按小批次删除并在批次间让出 writer，设置独立 maintenance 超时，并监控 WAL/删除耗时。

## 中

- [`internal/audit/query.go:76-136`, `internal/audit/store.go:106-108`] `GET /v1/admin/audits` 的基础游标查询不是全表扫描：实测查询计划会使用 `idx_request_audits_created_at`，带 model/account 过滤时会使用对应复合索引。但排序是 `created_at DESC, id DESC`，现有索引没有 `id`，SQLite 会为同时间戳的第二排序键建立临时 B-tree；protocol/status/tenant 也没有索引，稀有过滤值可能从最新记录向前扫描大量行。建议增加 `(created_at DESC, id DESC)`，并按真实查询频率评估 `(protocol, created_at DESC, id DESC)`、`(status_code, created_at DESC, id DESC)`。
- [`internal/server/admin_audits.go:70-98`, `internal/auth/pool.go:858-933`] Dashboard 资源统计只需要计数，却调用完整 `Credentials()`：复制/排序全部模型、复制 billing，并在持有 Pool `RLock` 时逐个获取账号 `RLock`。这不会直接阻塞常规 Acquire/Release，但会延迟需要 Pool 写锁的导入、删除、reload，并制造无用分配。建议提供不排序、不复制 models/billing 的资源统计快照，或维护原子计数。
- [`internal/audit/store.go:205-214`, `internal/audit/store.go:253-286`] 批量 INSERT 失败后仍立即清空 batch，没有重试或回填队列；busy timeout、磁盘满、I/O 错误会一次丢最多 100 条（Flush/Close 的 drain 还可能形成更大事务）。建议只在 Commit 成功后清空，失败时有限退避重试并记录可观测的永久丢失计数。
- [`internal/server/inference_execution.go:384-499`, `internal/server/audit_middleware.go:100-107`] 流式客户端断开后记录会以 HTTP 200 加 `client_write_error`/`request_canceled` 收尾，因为 SSE header 已经发出；duration 截止到检测到断开。总体成功率使用 `error_code` 判断，因此聚合正确，但按纯数值 `status=200` 查询会混入这类失败。建议在 API 文档明确该语义，并补 client-disconnect 测试。

## 低（可选优化）

- [`internal/server/web.go:47-68`] SPA handler 每次请求都 `fs.ReadFile`，磁盘和 embed 路径都会重新读取并分配完整文件；hash asset 有浏览器长期缓存，风险主要在冷启动/并发首次加载。可在 `newSPAHandler` 时缓存 `index.html`，embed 构建下缓存全部静态字节，磁盘开发模式则保留动态读取。
- [`internal/server/web.go:71-87`] `runtime-config.js` 每次请求重复 `json.Marshal` 和三次 Write。配置在进程生命周期内不变，可以在 `Server.New` 预生成；当前 payload 很小，优先级低。

## 已确认无阻塞回归

- [`internal/server/admin_credentials.go:85-115`, `internal/server/admin_credentials.go:145-168`] 空结果、最后一页和 cursor 指向已删除 ID 都能按 `id > cursor` 正常继续；最后一页 `has_more=false`、`next_cursor=""`。并发插入/删除不提供快照隔离，新插入且 `id <= cursor` 的记录会被跳过，这是普通 keyset pagination 的一致性边界。
- [`internal/server/admin_credentials.go:118-143`] `q` 对 ID/scope/tier/models 做不区分大小写匹配，`nil models` range 安全；问题是重复分配和重复全表扫描，不是匹配正确性。
- [`internal/server/admin_credentials.go:31-37`, `internal/server/admin_credentials.go:160-168`, `internal/auth/pool.go:58-76`] 无 `limit` 时分页字段因 `omitempty` 完全省略；相对 main，凭证 JSON 只新增内部 `Paid` 字段且标记 `json:"-"`，所以旧响应字段集合保持一致。
- [`internal/auth/pool.go:336-343`, `internal/auth/pool.go:440-553`, `internal/auth/pool.go:858-867`] `Credentials()` 的 Pool 读锁不会直接阻塞常规 Acquire（使用原子 scheduling snapshot 和账号读锁）或 Release（原子 inflight）；主要影响 Pool 写操作。现有 10,000 账号 Acquire 基准本机约 `0.84-0.89us/op`、336B/op、2 allocs/op。
- [`cmd/grok2api/main.go:60-68`, `internal/audit/store.go:117-136`, `internal/audit/store.go:183-195`] 正常关闭先 `http.Server.Shutdown` 再 `Store.Close`；Close 与 Enqueue 由 `enqueueMu`/closed flag 协调，不会 send-on-closed 或 panic。若 10 秒 Shutdown 超时后仍有请求存活，后续审计会返回 false 并丢弃，但不会崩溃。
- [`internal/audit/store.go:82-108`, `internal/audit/store.go:253-286`] SQLite 已启用 WAL、`synchronous=NORMAL`、5 秒 busy timeout、单事务 prepared batch insert；配置方向正确，但真实吞吐仍缺负载测试，不能只由账号数推断。

## 验证

- `go test ./...`：通过。
- `go vet ./...`：通过。
- `go test -race ./internal/audit`：通过。
- `go test -race ./internal/server` 全包受沙箱禁止监听本地端口影响，在既有 `httptest.NewServer` 用例 panic；改为定向运行凭证分页、审计 API/中间件、SPA/runtime-config 测试后通过。
- `BenchmarkListCredentials`（3 次）：`2.90-3.11ms/op`、`277,792 B/op`、`16,002 allocs/op`。
- SQLite 查询计划：audits 基础/模型游标使用现有时间索引，但需临时 B-tree 完成 `id` 次排序；`by_model`/`series` 使用时间范围索引后仍需临时 B-tree 聚合。

## 总体结论

- 当前代码可以在低频管理操作、温和推理流量下运行 1.6 万账号，但还不能认为已经达到稳健的 1.6 万规模生产标准。
- 上线前置项：首先修复“每页全量快照+排序”的伪分页并补真实 handler benchmark；其次为审计过载/丢弃建立可观测性和容量验证；然后处理 summary 全窗口聚合与 retention 独占单连接的问题。
- 游标边界、大小写/nil 模型、无参数 JSON 兼容、Acquire/Release 并发、关闭防 panic 和流式收尾逻辑未发现阻塞级正确性回归。
