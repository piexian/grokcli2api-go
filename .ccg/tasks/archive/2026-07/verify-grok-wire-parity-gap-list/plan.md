# Plan

1. 固定两个仓库的 commit、分支和工作区状态，读取项目 Spec。
2. 建立官方实现索引：sampler/types、URL-derived headers、三类 backend、错误与 SSE 解析。
3. 建立当前实现索引：config、headers/client、OpenAI、Anthropic、测试和文档。
4. 当前代理逐项对照源码，运行有针对性的测试或只读验证，形成带行号的判定。
5. 核验 duplicate call_id 的生成点、执行顺序、Codex 归因和安全去重策略。
6. 当前代理对两部分结论进行单代理复核，修正证据不足或优先级不当的项目。
7. 更新 review.md，检查是否有值得沉淀的 Spec，然后归档并提交 CCG 任务记录。
