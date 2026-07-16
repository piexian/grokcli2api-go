# Requirements

- 核验 `grok-build` tag 与 `GROK_VERSION`/crate version 的实际关系。
- 判断 client version 是否适合在运行时自动读取官方 tag，给出安全自动化方案。
- 从官方源码追踪 Messages backend 的 URL、鉴权和 OAuth/token 路径。
- 区分“客户端实现了 Messages”与“部署中的 Build API 对 OAuth 原生开放 Messages”。
- 修正上一份完整审计中证据强度过高的表述。
- 当前代理独立核验，不调用外部模型或子代理。
