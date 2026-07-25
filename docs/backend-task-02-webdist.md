# 后端任务：前端静态托管 + 运行时配置（第二批）

## 背景

管理台前端（`frontend/`，React+Vite）已完成骨架，构建产物为 `frontend/dist/`。现在需要 Go 后端直接托管该 SPA，并提供运行时配置注入，使单容器部署时前端开箱即用。**只改 Go 后端，不碰 `frontend/`。**

分支：`feat/admin-console`。

## 任务一：SPA 静态托管

1. 使用 `embed.FS` 内嵌 `frontend/dist`（构建标签分离：见任务三）。
2. 路由规则：
   - 现有 `/v1/*`、`/`（根 JSON）保持不变，优先级最高。
   - 其余 GET 路径：先尝试静态文件；不存在则回退到 `index.html`（SPA history 路由）。
   - 静态资源带 `Cache-Control: public, max-age=31536000, immutable`（带 hash 的 assets），`index.html` 用 `no-cache`。
3. 前端托管仅在配置开启时生效：环境变量 `GROK_WEB_DIST` 指定 dist 目录（默认 `frontend/dist`）；设为 `off` 时完全不注册静态路由（保持现状行为，API-only）。
4. 当 dist 目录存在时优先从磁盘读（开发友好），不存在时回退到 embed 的打包副本。

## 任务二：运行时配置注入

前端通过 `/runtime-config.js` 读取 `window.__GROK2API_RUNTIME_CONFIG__`（已在前端实现消费）。

- `GET /runtime-config.js`：动态生成，内容：
  ```js
  window.__GROK2API_RUNTIME_CONFIG__ = {
    apiBaseUrl: "",
    publicApiBaseUrl: "<从配置推导：GROK_PUBLIC_BASE_URL 环境变量，缺省为空字符串>"
  };
  ```
- 带 `Cache-Control: no-store`。
- 无论静态托管是否开启，此端点都必须可用（API-only 部署时前端可能独立部署）。

## 任务三：构建与 Docker

1. 在 `internal/server/webdist/`（或类似位置）用 `embed.FS` 内嵌 `frontend/dist`：
   - 由于 `frontend/dist` 在仓库中不提交（gitignore），用 **构建标签** 分离：
     - `webdist_embed.go`（`//go:build embed_web`）：`//go:embed` 真实 dist（构建前需先生成 `frontend/dist`，可用占位文件保证编译）。
     - `webdist_stub.go`（`//go:build !embed_web`）：空实现。
   - 默认构建（无标签）行为与现状一致；`go build -tags embed_web` 才内嵌。
2. 更新 `Dockerfile`：
   - 新增 Node 阶段：`node:24-alpine`，`corepack enable && pnpm install --frozen-lockfile && pnpm build`（在 `frontend/` 目录）。
   - Go 构建改为 `go build -tags embed_web`。
   - 最终镜像不变（单二进制）。
3. 更新 `.env.example` 补 `GROK_WEB_DIST`、`GROK_PUBLIC_BASE_URL` 说明。

## 约束

- 遵循现有风格：stdlib mux、`slog`、配置走 `internal/config`（含测试）。
- 不破坏现有路由：API 路径不变；无 `GROK_ADMIN_KEY` 时行为不变。
- 测试：
  - SPA 回退（未知路径返回 index.html；`/v1/xxx` 不受影响）
  - 静态内容类型与缓存头
  - runtime-config.js 内容生成
  - `GROK_WEB_DIST=off` 时 404
- `go build ./... && go build -tags embed_web ./... && go vet ./... && go test ./...` 全绿。
- 提交：commit message `feat: serve admin console SPA with runtime config`。

## 验收

1. `pnpm -C frontend build` 后 `go run -tags embed_web ./cmd/grok2api`，浏览器打开 `http://localhost:8088/` 能加载管理台（login 页）。
2. `curl http://localhost:8088/runtime-config.js` 返回合法 JS。
3. `curl http://localhost:8088/v1/models` 行为不变（走 API key 鉴权，不被 SPA 拦截）。
