# Review

> Post-audit correction (2026-07-16): the public `xai-org/grok-build` repository has no Git tags or Releases; use the official npm/channel-pointer release sources instead. The sampler's `ApiBackend::Messages` implementation alone does not prove production entitlement. Same-token probes showed free OAuth: Responses 200 (`grok-4.5-build-free`) and Messages 403; X Premium+ OAuth (JWT tier 4): Responses 200 (`grok-4.5-build`) and Messages 200 for both non-streaming and streaming. `/v1/models` advertised only `responses` in both cases, so it is not a complete capability list. This does not prove Messages support for SuperGrok tiers. See `.ccg/tasks/archive/2026-07/verify-version-tags-and-messages-oauth/review.md` and the corrected combined audit.

## Baseline

- Current adapter: `/root/work/grokcli2api-go` at `fc2f2cdca682aaf1f864317bc9aa0c83c2e8e891`
- Official source: `/root/work/grok-build` at `c68e39f60462f28d9be5e683d9cbe2c57b1a5027`
- Official changelog latest entry at this snapshot: `0.2.101` (2026-07-13)
- Verification: `go test ./...` passed in the adapter repository.

## Verdict

The supplied gap list is directionally correct for proxy headers, Anthropic conversion losses, retry metadata, and agent/product boundaries. It is not accurate enough to implement unchanged. It misses three concrete Chat wire divergences, treats native Responses passthrough gaps as global gaps, overstates production Messages availability, understates Anthropic compatibility losses, and recommends an already stale client version.

An exact “80-85% covered” figure is not supported by a stable denominator: the report mixes wire fields, optional metadata, backend architecture, and deliberately excluded agent features.

## Critical corrections

### 1. Chat body parity is not complete

- Official Chat serializes `user`, not `user_id`: `xai-grok-sampling-types/src/types.rs:64-89`. The adapter rewrites `user` and `metadata.user_id` to `user_id`: `internal/openai/chat_prepare.go:274-320`.
- Official streaming Chat always injects `stream_options.include_usage=true`: `xai-grok-sampler/src/client.rs:256-273` and `:901-909`. The adapter intentionally strips `stream_options`: `internal/openai/chat_prepare_test.go:9-35` and `internal/server/server_test.go:95-147`.
- Official `search_parameters` is now `{mode, sources[], from_date, to_date, return_citations, max_search_results}`, with typed `x/web/news/rss` source objects: `xai-grok-sampling-types/src/types.rs:652-713`. The adapter accepts an older flattened shape and drops `mode` and `sources`: `internal/openai/chat_prepare.go:778-837`.

Therefore “Chat fields: no P0/key gap” should be replaced with: basic chat is covered, but optional user identity, streaming usage, and realtime-search wire parity are not.

### 2. URL-derived proxy headers must remain URL-derived

Official injection adds `X-XAI-Token-Auth`, `x-authenticateresponse`, and `x-grok-client-mode` only when the assembled URL matches the trusted cli-chat-proxy base: `xai-grok-shell/src/agent/config.rs:4667-4696`; the trusted URL check rejects public API and spoofed hosts: `xai-grok-shell-base/src/util/mod.rs:45-76,211-230`.

The adapter currently sends `x-xai-token-auth` unconditionally from `BuildHeaders`: `internal/grok/headers.go:52-80`, even though `GROK_CHAT_PROXY_BASE_URL` is configurable: `internal/config/config.go:97-118`.

The proposed patch must not unconditionally add the two missing headers. It should apply all three proxy-derived headers only to the trusted cli-chat-proxy URL (using the base URL plus configured version). This also fixes the existing over-injection of `X-XAI-Token-Auth` to custom/BYOK hosts.

### 3. `0.2.99` is not the current official target

The adapter is internally inconsistent: runtime/header default and fixtures use `0.2.93` (`internal/config/config.go:115`, `.env.example:75-76`), while the Responses compatibility code and README claim a `0.2.99` baseline (`internal/openai/converter.go:461-473`, `README.md:79`).

However, the official snapshot's newest changelog entry is `0.2.101`: `xai-grok-shell/CHANGELOG.md:3`. Official builds inject the shipping version through `GROK_VERSION`, falling back to the version crate package value: `xai-grok-version/src/lib.rs:1-10`. Updating to `0.2.99` merely replaces one stale value with another. Select and document a verified release baseline (for this snapshot, investigate `0.2.101`) and centralize fixtures around it.

This is a real consistency/version-gate concern, but source alone does not prove it is P0 unless the proxy currently returns 426 for `0.2.93`.

## Warning corrections

### 4. Client mode semantics need precise wording

The missing `x-grok-client-mode` header is real. Official mode defaults to `interactive`; non-TUI entry points set the process latch to `headless`: `xai-grok-http/src/lib.rs:244-262`, `xai-grok-shell/src/agent/app.rs:430-436`. `headless` is the sensible adapter default, but it is a product choice rather than the official process default. A config value should be validated to `headless|interactive`.

### 5. User ID dual-send recommendation is correct

Official sampler sends `x-grok-user-id`: `xai-grok-sampler/src/client.rs:43-71`. Official remote APIs use `x-userid`: `xai-grok-shell/src/remote/client.rs:26-70`. The adapter sends only `x-userid`: `internal/grok/headers.go:70-72`. Sending both for a non-empty session user ID is a sound compatibility fix.

### 6. Retry semantics are more specific than the report states

Official parses `x-should-retry`, but only `false` vetoes a retry; `true` does not force one: `xai-grok-sampler/src/client.rs:206-227`, `xai-grok-sampler/src/retry.rs:173-186`. Official retryable API statuses are `429, 500, 502, 503, 504, 520`: `xai-grok-sampling-types/src/error.rs:240-248`.

The adapter ignores the header and retries only quota cases, 429, 502, 503, and 504: `internal/grok/client.go:681-702`; `parseAPIError` reads only request ID and Retry-After metadata: `internal/grok/client.go:898-920`. A parity fix should add a tri-state hint, make `false` prevent account rotation/cooldown, leave `true` to existing classification, and separately decide whether to add 500/520.

### 7. Responses xAI extensions are compatibility-branch gaps, not global gaps

`stream_tool_calls` is injected only for official streaming Responses when per-model config enables it: `xai-grok-sampler/src/client.rs:1245-1289`. Raw `x_search` is likewise injected in the streaming path: `xai-grok-sampling-types/src/conversation.rs:2399-2426`, `xai-grok-sampler/src/client.rs:1849-1867`.

The adapter's OpenAI compatibility whitelist/validation rejects these (`internal/openai/converter.go:57-76,310-360`), but native Grok requests already retain arbitrary extension fields and raw tools: `internal/openai/converter.go:553-558`, selected by `internal/server/server.go:1031-1051`. Add them only if non-native OpenAI clients should opt into xAI extensions; do not describe them as missing from native passthrough.

### 8. Store/include policy is already documented

Official sampler defaults omitted `store` to false and always adds `reasoning.encrypted_content`: `xai-grok-sampler/src/client.rs:1068-1101`. The adapter compatibility branch intentionally defaults `store` to true and removes encrypted reasoning include: `internal/openai/converter.go:421-458`, `internal/openai/responses_compat.go:78-81`. This policy is already explicit in `README.md:79-83`; the proposed documentation task is complete. Native passthrough does not synthesize official defaults, but a real official client already sends them after its sampler applies defaults.

### 9. Anthropic compatibility loss is understated

The adapter behavior is confirmed: downstream `/v1/messages` always becomes upstream `/v1/responses`: `internal/server/server.go:366-405,601-603`. Official source has a `Messages` protocol implementation (`xai-grok-sampling-types/src/types.rs:1010-1021`), but that alone does not establish production Build API OAuth entitlement. The later free-OAuth A/B probe shows Responses is the only advertised and usable backend for this target.

Corrections:

- Adapter `top_k` is rejected with a local 400, not merely warned/stripped: `internal/anthropic/converter.go:29-44`.
- Official supports enabled, adaptive, and disabled thinking: `xai-grok-sampling-types/src/messages.rs:158-184`; adapter maps only `thinking.type=enabled`: `internal/anthropic/converter.go:119-131`.
- Adapter maps only `output_config.format`; `output_config.effort` is ignored: `internal/anthropic/converter.go:132-136`.
- Local `stop_sequences` execution is implemented and is a semantic approximation, as the report says: `internal/anthropic/converter.go:166-172` plus response/stream filters.

A native Messages backend should remain disabled for the current free-OAuth target. It becomes reasonable only behind a capability check after `/v1/models` advertises `apiBackend=messages` or a paid/entitled OAuth probe succeeds; it would then still need request auth selection, streaming policy, error mapping, and tests.

## Confirmed findings

- Missing `x-authenticateresponse` is a genuine official fingerprint gap. Calling it P0 is reasonable for proxy parity, although the open-source code proves unconditional injection on the trusted proxy, not that every request fails without it.
- Missing `x-grok-client-mode` is genuine.
- `x-grok-user-id` is missing and dual-send is appropriate.
- Official supports three upstream inference paths while the adapter uses Chat and Responses upstream only.
- `x-should-retry` is ignored by the adapter.
- Official reads response model metadata; adapter does not. The report also omitted official `x-models-etag`: `xai-grok-sampler/src/client.rs:229-253`.
- Official rewrites terminal Responses SSE `usage.total_tokens` from `usage.context_details`; adapter sanitizes usage without this rewrite: `xai-grok-sampler/src/client.rs:89-193`, `internal/openai/converter.go:616-646`.
- Agent-only doom-loop, compaction, MCP host, sandbox, WebSocket, and TUI capabilities should remain outside this API adapter unless the product scope changes.

## Additional notes

- UA mismatch is real and broader than the report states. The adapter defaults to `grok-pager/<version> grok-shell/<version>` and uses Go OS/arch names (`darwin`, `amd64`, `arm64`): `internal/grok/headers.go:37-50`. Official fallback is one `grok-shell/<compiled-version>` slot and normalizes to `macos`, `x86_64`, and `aarch64`: `xai-grok-sampler/src/client.rs:335-387`. The adapter's alternate UA is used for streaming requests with `trace=true`, which means Responses/Anthropic streams, not ordinary streaming Chat: `internal/grok/client.go:665-670`, `internal/server/server.go:407-409,601-603`.
- Official `patch_reasoning_text_types` compensates for a Rust typed-serialization omission. This Go adapter receives already-serialized JSON, so absence of an equivalent post-serializer patch is not itself a parity bug.
- Chinese README correctly describes local `stop_sequences`; English README still says it is only warned/ignored: `README.md:85` vs `README_EN.md:85`.

## Revised priority

1. P0: trusted-URL-scoped `x-authenticateresponse` injection, with regression tests.
2. P1: add validated `x-grok-client-mode` (adapter default `headless`); dual-send `x-grok-user-id`; choose one verified client version baseline; fix Chat `user` and current `search_parameters` shape.
3. P2: honor `x-should-retry:false` and reconcile retry statuses; inject Chat streaming usage options; normalize UA; optionally expose `stream_tool_calls`/`x_search` to compatibility clients; retain a Messages capability probe without enabling it for free OAuth.
4. P3: Responses context-details presentation, response model metadata, and documentation cleanup unless operational needs raise their priority.
5. Out of scope: agent-only doom-loop/compaction/TUI/MCP/sandbox/WS product features.

## Codex duplicate `call_id` review

### Confirmed

- The exact text `duplicate tool call_id: ...` is generated locally by `validateResponsesInput`: `internal/openai/converter.go:167-231`.
- For non-native clients, `Server.responses` calls `ValidateResponsesRequest` before `PrepareCompatibleResponses`, replay, normalization, or any upstream request: `internal/server/server.go:310-347`.
- `previous_response_id` only relaxes the unmatched-output check. It does not relax duplicate calls or duplicate outputs: `internal/openai/converter.go:189-229`.
- The compatibility pipeline is explicitly intended for OpenAI/Codex and Alma/Codex tool continuity, and it runs item-reference expansion, replay, and normalization after this ingress validation: `internal/openai/responses_compat.go:60-145`.
- Replay does not insert another call when that `call_id` is already present in the request: `internal/openai/tool_replay.go:357-430`. Therefore this specific error proves at least two call items with the same ID were already visible to ingress validation; it does not prove which component duplicated them.
- The post-normalization validator rejects duplicate outputs and orphan outputs, but does not check duplicate calls: `internal/openai/responses_compat.go:895-929`. Simply deleting or moving the ingress duplicate check would let duplicate calls reach upstream unless this validator is extended.

### Attribution corrections

- A `call-<UUID-like>` value is not a Codex fingerprint. OpenAI's Responses schema defines `call_id` as the unique ID generated by the model. Codex `0.144.4` deserializes the upstream `response.output_item.done` item and stores its `call_id`; no function-call UUID generation path was found in the official source. The only UUID generation in history normalization is for synthetic output item `id`, not `call_id`: `codex-rs/codex-api/src/sse/responses.rs:326-335`, `codex-rs/core/src/context_manager/normalize.rs:12-141`, and `codex-rs/protocol/src/models.rs:996-1040` at tag `rust-v0.144.4`.
- Current Codex HTTP mode builds each request from its local conversation `input`, defaults `store=false` for non-Azure providers, and does not put `previous_response_id` in `ResponsesApiRequest`: `codex-rs/core/src/client.rs:829-915`, `codex-rs/codex-api/src/common.rs:215-239`. Its history normalization only inserts missing outputs and removes orphan outputs; it does not deduplicate calls: `codex-rs/core/src/context_manager/history.rs:355-367`.
- Consequently, “this is a Codex request” may be established by request metadata, but “Codex created the duplicate” is not established by the error or ID format. A middleware, persisted-history replay, corrupted resumed session, provider response, or Codex history assembly could be responsible. The final payload at this server is required to distinguish them.
- The claim that official OpenAI is more lenient is unsupported. The current API reference calls `call_id` unique, and the official function-calling guide demonstrates one output for each returned call. No documented duplicate-acceptance behavior was found.
- Official xAI agent code does deduplicate duplicate `ToolResult` entries, keeping the last result, but only inside the consecutive result run immediately following an assistant tool-call group. It does not deduplicate duplicate tool calls: `xai-grok-sampling-types/src/conversation.rs:2898-2965`.

### Corrected product judgment

The 400 is definitely local and occurs before Grok. It is reasonable to describe the adapter as strict and potentially insufficiently robust for imperfect Codex-compatible clients. It is not yet proven to be a Codex compatibility bug, and strict rejection is consistent with the published uniqueness contract. Responsibility cannot be assigned until the duplicated items are compared and the component that assembled the final request is identified.

### Safer implementation policy

Do not globally apply “first call wins, last output wins.” Two entries with one `call_id` can disagree on type, namespace, name, arguments, or output; silently choosing one can execute or attribute the wrong action.

If leniency is desired, split validation into two phases:

1. Before normalization: validate JSON shapes and required fields without enforcing cross-item uniqueness.
2. Normalize aliases, expand references, and apply replay.
3. Canonicalize and collapse only semantically identical duplicate calls/outputs, while emitting a structured diagnostic.
4. After normalization: reject conflicting duplicate calls and conflicting duplicate outputs with both input indexes; retain orphan/matching checks and ensure only one call/output pair is forwarded.

Required tests should cover identical duplicate calls, conflicting duplicate calls, identical and conflicting outputs, custom-to-function alias collisions, `previous_response_id` replay with an already-present call, and an upstream-capture assertion that exactly one canonical pair is sent.

### Operational guidance corrections

- Capturing `type/index/call_id/name` is a good first diagnostic, but also compare canonical `arguments` and output hashes. The key distinction is identical replay versus conflicting reuse.
- “Prefer `previous_response_id` in Codex” is not a general current Codex HTTP setting. Official Codex normally resends full stateless history; WebSocket mode can use connection-scoped previous-response state.
- Avoiding process restart matters only for this adapter's local `store:false` replay cache. It is not necessary for a stateful upstream `store:true` continuation.
- Parameter passthrough in an intermediate proxy prevents field loss; it does not prevent that proxy from duplicating history. Inspect the payload at the adapter boundary before blaming either Codex or the intermediate service.
