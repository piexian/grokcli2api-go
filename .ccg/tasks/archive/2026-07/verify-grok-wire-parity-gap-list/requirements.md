# Requirements

核验用户提供的“官方 xai-grok-sampler vs 当前适配”字段级缺口清单是否准确。

## Source baselines

- Current adapter: `/root/work/grokcli2api-go` at `fc2f2cdca682aaf1f864317bc9aa0c83c2e8e891`
- Official source: `/root/work/grok-build` at `c68e39f60462f28d9be5e683d9cbe2c57b1a5027`

## Acceptance criteria

- 对 headers、路径/backend、Chat body、Responses、Anthropic Messages、SSE/错误重试逐项核验。
- 每个重要判断引用具体文件与行号。
- 区分“确实缺失”“已有实现”“部分正确”“策略差异”“无证据/已过时”。
- 给出修正后的优先级清单，避免把 agent 专属能力误算为 API 适配缺口。
- 由当前代理独立核验；除非用户另行明确要求，不调用外部模型或子代理。

## Added Codex scenario

- 核验 duplicate tool `call_id` 400 是否由适配层在出网前生成。
- 核验校验与 Responses normalize/tool-replay 的真实执行顺序。
- 区分已证实的请求形态、Codex/New API 归因推测和官方协议行为。
- 评估 soft-dedupe 是否安全，并给出不会静默吞掉冲突调用的修复策略。
