# grokcli2api-go 完整适配审计

## 1. 审计基线

- 当前适配仓库：`/root/work/grokcli2api-go`
- 当前适配基线提交：`fc2f2cdca682aaf1f864317bc9aa0c83c2e8e891`
- 官方源码仓库：`/root/work/grok-build`
- 官方源码基线提交：`c68e39f60462f28d9be5e683d9cbe2c57b1a5027`
- Codex 对照：`openai/codex` tag `rust-v0.144.4`
- 官方 xAI changelog 在该快照的最新版本：`0.2.101`
- 验证：当前适配执行 `go test ./...` 全部通过。

## 2. 总结论

原字段缺口清单方向基本正确，但不能原样作为实施清单：

1. Proxy 指纹确实缺 `x-authenticateresponse` 和 `x-grok-client-mode`，但官方是按可信 URL 派生注入；不能在所有可配置上游上无条件发送。
2. Chat 主体可用，但不是“无关键字段缺口”：`user`、流式 `stream_options.include_usage`、新版 `search_parameters` 均有明确 wire 漂移。
3. Responses 兼容层较完整，但必须区分 native Grok 透传和 OpenAI/Codex 兼容分支；`stream_tool_calls`、raw `x_search` 不是全局缺失。
4. Anthropic `/v1/messages` 能用，但当前实际转为上游 `/v1/responses`，其语义损失比原报告写得更大。
5. Retry、响应模型元数据和 SSE usage 仍有真实缺口。
6. Codex duplicate `call_id` 的 400 确实由适配层在出网前生成；但现有证据只能证明最终入站 payload 已重复，不能证明 Codex 是重复项的制造者。
7. doom-loop、compaction、TUI、MCP host、sandbox、agent WebSocket 等仍应排除在 API 适配范围之外。

“核心协议覆盖 80–85%”没有可复现的分母：字段、可选元数据、后端路径和明确排除的 agent 能力被混在一起计数。更准确的说法是：Chat/Responses 核心调用已覆盖，但仍有一组 P0/P1/P2 级兼容行为需要按产品目标取舍。

## 3. 请求头与客户端指纹

### 3.1 Proxy URL 派生头

官方在可信 `cli-chat-proxy` URL 分支注入：

- `X-XAI-Token-Auth: xai-grok-cli`
- `x-authenticateresponse: authenticate-response`
- `x-grok-client-mode: interactive|headless`

当前状态：

- `X-XAI-Token-Auth` 已发送。
- `x-authenticateresponse` 缺失。
- `x-grok-client-mode` 缺失。
- 更重要的是，当前 `BuildHeaders` 会把 `X-XAI-Token-Auth` 发往任意配置的上游 URL；这与官方 URL-derived 语义不一致，也会污染 BYOK/自定义 host。

正确补法：在 client 已组装出最终 URL 后，复用可信 proxy URL 判断，一次性决定是否发送这三个头。不能直接在通用 `BuildHeaders` 中无条件增加后两个头。

证据：

- 官方：`xai-grok-shell/src/agent/config.rs:4667-4696`
- 官方可信 URL 判断：`xai-grok-shell-base/src/util/mod.rs:45-76,211-230`
- 当前：`internal/grok/headers.go:52-80`
- 当前 URL 可配置：`internal/config/config.go:97-118`

### 3.2 Client mode

缺口成立，但语义需要修正：官方进程默认是 `interactive`，非 TUI 入口会切为 `headless`。本 API 适配器默认 `headless` 合理，但这是产品默认，不是官方全局默认。配置值应限制为 `headless|interactive`。

### 3.3 User ID

原建议正确：

- 官方 sampler 使用 `x-grok-user-id`。
- 官方 remote client 使用 `x-userid`。
- 当前只发 `x-userid`。

对非空用户 ID 双发两个头是低风险兼容修复。

### 3.4 Client version

当前内部不一致：

- runtime/header 和 `.env.example` 使用 `0.2.93`。
- README 与 Responses 兼容基线写 `0.2.99`。
- 官方源码快照的 changelog 最新为 `0.2.101`。

因此不能简单把默认值改成 `0.2.99`。应先选定一个实际验证可通的发布基线，再集中定义 runtime、fixture、README 和过滤规则。源码本身不能证明旧版本一定触发 426，所以版本不一致是 P1；只有线上确认版本门禁后才升级为 P0。

### 3.5 User-Agent 与可选头

UA 漂移成立：

- 当前默认带 `grok-pager/<ver> grok-shell/<ver>`，并使用 Go 的 `darwin/amd64/arm64`。
- 官方 fallback 是 `grok-shell/<compiled-version>`，平台名规范化为 `macos/x86_64/aarch64`。

`traceparent`、doom-loop、compaction、deployment-id、turn-idx 等不是普通 API proxy 存活条件；除非产品边界改变，不应为了“字段齐全”而补。

## 4. 上游路径与 Backend

| 能力 | 官方 | 当前 | 判断 |
| --- | --- | --- | --- |
| `POST /v1/chat/completions` | 支持 | 支持 | 核心可用 |
| `POST /v1/responses` | 支持 | 支持 | 核心可用 |
| `POST /v1/messages` | 原生 backend | 下游 Messages 转上游 Responses | 架构/语义缺口 |
| `GET /v1/models` | 支持 | 支持并池化 | 可用 |

Anthropic 原生 Messages backend 是功能债，不是当前服务的存活债。增加 `GROK_ANTHROPIC_BACKEND=responses|messages` 的 feature flag 是合理方向，但不仅是改 path，还需要鉴权选择、流式事件策略、错误映射和行为测试。

## 5. Chat body 审计

### 5.1 已覆盖

`model`、`messages`、temperature/top-p、max token aliases、penalties、tools/tool_choice、reasoning_effort、response_format、stop 清洗及严格模型字段剔除均已有实现。

### 5.2 原报告漏掉的三处 wire 漂移

1. `user`
   - 官方 Chat wire 序列化 `user`。
   - 当前把 `user` 和 `metadata.user_id` 改写为 `user_id`。
   - 证据：官方 `xai-grok-sampling-types/src/types.rs:64-89`；当前 `internal/openai/chat_prepare.go:274-320`。

2. 流式 usage
   - 官方流式 Chat 注入 `stream_options.include_usage=true`。
   - 当前主动剥离 `stream_options`。
   - 证据：官方 `xai-grok-sampler/src/client.rs:256-273,901-909`；当前测试 `internal/openai/chat_prepare_test.go:9-35`、`internal/server/server_test.go:95-147`。

3. `search_parameters`
   - 官方当前形状是 `mode + sources[] + from_date/to_date + return_citations + max_search_results`，其中 source 是 `x/web/news/rss` 类型对象。
   - 当前接受旧的扁平结构，并丢弃 `mode`、`sources`。
   - 证据：官方 `xai-grok-sampling-types/src/types.rs:652-713`；当前 `internal/openai/chat_prepare.go:778-837`。

结论：基础 Chat 覆盖良好，但不能写成“字段层无关键缺口”。

## 6. Responses 审计

### 6.1 兼容层能力

当前兼容层已经覆盖较多 Codex/OpenAI 形态：

- Codex input history 清洗。
- `item_reference` 展开。
- `previous_response_id`/prompt cache 下的本地 tool replay。
- reasoning、server tool 历史和自定义工具别名转换。
- OpenAI 字段白名单、工具规范化和 SSE 响应转换。

### 6.2 Native 与 compatibility 必须分开描述

`stream_tool_calls` 和 tools 中 raw `x_search`：

- 非 native OpenAI/Codex 兼容分支会因白名单/工具校验拒绝。
- native Grok 分支已经保留任意扩展字段和 raw tools。
- 官方也只在特定 streaming/config 路径注入这些扩展。

因此它们属于“是否向兼容客户端开放 xAI 扩展”的 P2 产品选择，不是 Responses 全局缺失。

### 6.3 `store/include` 策略

- 官方 sampler 默认 `store=false`，并加入 `reasoning.encrypted_content`。
- 当前兼容分支默认 `store=true`，并删除 encrypted reasoning include。
- 当前 README 已明确说明这一策略。

这是有意的 OpenAI-compatible 产品策略，不应继续列为“漏字段”。native 请求也不会被该兼容策略改写。

### 6.4 `patch_reasoning_text_types`

官方 Rust 补丁用于修正 typed serialization 遗漏。当前 Go 适配接收的已是 JSON；没有同名后处理不能单独证明存在 parity bug。

## 7. Anthropic Messages 审计

当前 `/v1/messages` 的主体转换能工作，但原报告低估了差异：

- `top_k`：当前本地直接 400，不是“警告后忽略”。
- `thinking`：官方支持 enabled/adaptive/disabled；当前只映射 enabled。
- `output_config`：当前只映射 format，忽略 effort。
- `stop_sequences`：当前本地执行，是近似语义，不是上游原生停止。
- `metadata.user_id` 到 `safety_identifier`：已映射。
- 上游 `/v1/messages`：当前不使用。

这使原生 Messages feature flag 保持为 P2，但实现工作量应按一个完整 backend 评估，而不是简单透传开关。

## 8. Retry、响应头与 SSE

### 8.1 `x-should-retry`

缺口成立，但官方语义不是 `true` 强制重试：

- `false` 明确否决重试。
- `true` 不覆盖现有状态码分类。
- 官方 retryable 状态：429、500、502、503、504、520。
- 当前忽略该头，只重试 quota、429、502、503、504。

正确实现是三态提示：`false` 阻止重试、换号和 cooldown；`true/缺失` 继续走状态码规则。500/520 是否加入应单独评估。

### 8.2 其他响应元数据

真实但低优先级的缺口：

- `x-grok-context-window`
- `x-grok-max-completion-tokens`
- `x-models-etag`
- Responses SSE `usage.context_details` 对 `total_tokens` 的终态重写

这些主要影响模型元数据、展示和计量，通常为 P2/P3。

## 9. Codex duplicate `call_id` 审计

### 9.1 已确认事实

1. `duplicate tool call_id: ...` 由 `internal/openai/converter.go:167-231` 本地生成。
2. 非 native 请求在 `internal/server/server.go:310-347` 先执行 `ValidateResponsesRequest`，随后才进入 `PrepareCompatibleResponses`、item reference、replay、normalize 和上游调用。
3. `previous_response_id` 只放宽无 matching call 的 output；重复 call 和重复 output 仍会 400。
4. 当前 replay 在请求已经含该 `call_id` 时不会再插入缓存 call：`internal/openai/tool_replay.go:357-430`。
5. 所以该错误证明 ingress 看到至少两个同 ID call；它不能证明是哪一层复制的。
6. post-normalize 校验会检查重复/孤立 output，但不检查重复 call：`internal/openai/responses_compat.go:895-929`。单纯删除前置校验会把重复 call 放到上游。

### 9.2 不能成立的归因

- `call-<UUID-like>` 不是 Codex 指纹。OpenAI Responses schema 将 `call_id` 定义为模型生成的唯一 ID。
- Codex `0.144.4` 从 `response.output_item.done` 反序列化并保存 call ID；没有发现为 function call 生成该 UUID 的路径。
- Codex HTTP 通常从本地 conversation input 构造完整无状态请求；其 history normalize 只补缺失 output、删除 orphan output，不去重 duplicate call。
- 因此即使请求 UA 是 Codex，也不能仅凭该错误断言重复项由 Codex 制造。中间层 history merge、恢复会话损坏、provider 输出、Codex 持久化/组装都仍是候选。
- 没有证据证明官方 OpenAI 会容忍重复；公开协议反而要求 call ID 唯一。

### 9.3 官方 xAI dedupe 的真实边界

官方 xAI agent 确实有 `dedup_duplicate_tool_results`，但它：

- 只处理 assistant tool-call group 后紧邻的一段 `ToolResult`。
- 同一 tool call 有 cancelled result 和 real result 时保留最后一个。
- 不去重 duplicate tool call。

所以不能把这段 agent 会话修复逻辑直接类推为 API ingress 应静默接受重复 call。

### 9.4 安全修复策略

不建议全局采用“call 保留第一份、output 保留最后一份”。同一 ID 的两项可能在 type、namespace、name、arguments 或 output 上冲突，静默选择会执行或归因错误的工具动作。

若产品决定提高兼容容错：

1. ingress 先做 JSON 形状和必填字段校验，不先做跨 item 唯一性判断。
2. 执行 alias normalization、item reference 展开和 tool replay。
3. canonicalize 后只折叠语义完全一致的重复 call/output，并记录结构化诊断。
4. 同 ID 但内容冲突继续 400，错误中给出两个 input index。
5. post-normalize 校验补齐 duplicate call、duplicate output、orphan/matching 检查，保证上游只收到一对 canonical call/output。

必补测试：

- 完全相同的 duplicate function/custom call。
- 同 ID、不同 name/arguments/type 的冲突 call。
- 完全相同和冲突的 duplicate output。
- custom-to-function alias 后发生 ID 冲突。
- `previous_response_id` replay 遇到已存在 call。
- server capture 断言上游只收到一对 canonical item。

### 9.5 运维判断

- 首先记录重复项的 index/type/call_id/name，并比较 canonical arguments 和 output hash，区分完全重放与冲突复用。
- `previous_response_id` 不是普通 Codex HTTP 的通用开关；不能把它作为主要规避方案。
- 不重启适配进程只影响本地 `store:false` replay cache；上游 `store:true` 状态续写不依赖该进程缓存。
- 中间层“参数透传”只能避免字段被删，不能保证其 history merge 不复制 item。应在本适配 ingress 边界抓取脱敏后的最终结构。

## 10. 修正后的优先级

### P0：仅在当前 proxy 可用性受影响时立即处理

1. 对可信 cli-chat-proxy URL 注入 `x-authenticateresponse`。
2. 同一 URL 派生逻辑统一管理现有 `X-XAI-Token-Auth`，避免发往自定义 host。

### P1：短周期兼容修复

1. 增加并校验 `x-grok-client-mode`，适配器默认 `headless`。
2. 双发 `x-userid` 与 `x-grok-user-id`。
3. 选定并统一一个实测发布版本基线，不直接追随过时的 `0.2.99`。
4. 修正 Chat `user` wire 行为。
5. 支持当前官方 `search_parameters.mode/sources[]` 形态。
6. 若真实流量证实 identical duplicate call 来自正常 Codex 链路，实施保守 canonical dedupe；冲突重复继续拒绝。

### P2：协议增强

1. 支持 `x-should-retry:false` veto，并评估 500/520。
2. 对齐流式 Chat usage options。
3. 统一 UA 平台/架构格式。
4. 选择性向 compatibility 分支开放 `stream_tool_calls`、raw `x_search`。
5. 评估原生 Anthropic Messages backend。
6. 处理 SSE context-details usage 和响应模型元数据。

### P3/不补

- doom-loop 事件/头。
- compaction headers 与 agent repair 全家桶。
- deployment-id、turn-idx。
- TUI、MCP host、sandbox、agent tools、完整 WebSocket 产品能力。
- 仅为了形式对齐而复制 Rust serializer 的 reasoning type patch。

## 11. 最终回答

| 问题 | 完整结论 |
| --- | --- |
| 当前适配能否用于 Chat/Responses/Codex？ | 能，核心路径已具备，但并非完全 wire parity。 |
| 最大存活风险是什么？ | 可信 proxy 的 URL-derived 头和实际版本门禁；是否 P0 要结合线上响应确认。 |
| 前一版漏了什么？ | Chat `user`、流式 usage、新版 search shape；Anthropic top_k/thinking/output_config 的实际损失。 |
| Responses 扩展是否全缺？ | 否，native 已透传；主要是 compatibility 分支是否开放的问题。 |
| duplicate call_id 是上游 Grok 报的吗？ | 否，是适配层出网前本地 400。 |
| 能否断言是 Codex 造出的重复？ | 不能；只能确认最终 ingress payload 已重复。 |
| 是否应该无条件 soft-dedupe？ | 不应该；只折叠 canonical-equivalent 重复，冲突内容必须继续报错。 |
| 最大架构债是什么？ | Anthropic 下游 Messages 仍通过上游 Responses 实现。 |
| 哪些能力明确不补？ | agent/TUI/MCP/sandbox/doom-loop/compaction 等产品层能力。 |
