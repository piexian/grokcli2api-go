# 后端任务：修正模型级冷却的管理台统计（第六批）

## 背景与生产证据

只改 Go 后端，禁止修改 `frontend/`。

生产当前有 10,156 个池账号：

- 账号级 `CooldownUntil` 活跃：1
- 当前账号中存在活跃 `ModelCooldowns["grok-4.5"]`：880
- 任一冷却（账号级或模型级）：881
- 880 个模型级原因均为 `model_free_quota_exhausted`

但当前 API 返回：

- Dashboard `cooling_accounts=1`
- `GET /v1/admin/credentials?status=cooling_down` 的 `total=1`
- 模型 summary 把这 880 个仍计入 `usable_accounts`

根因：`credentialInfoAt` 只将账号级 `account.cooldownUntil` 写入 `CredentialInfo.Status/Usable/CooldownUntil`，忽略 `account.modelCooldowns`；Dashboard 和模型聚合又只基于 `CredentialInfo.Status/Usable`。

## 正确语义

### 1. 凭证元数据纳入模型级冷却

为 `CredentialInfo` 增加脱敏字段（字段名可按现有风格调整）：

```go
type ModelCooldownInfo struct {
    Until  time.Time `json:"until"`
    Reason string    `json:"reason,omitempty"`
}

ModelCooldowns map[string]ModelCooldownInfo `json:"model_cooldowns,omitempty"`
```

`credentialInfoAt` 只复制当前仍有效的模型冷却，并保证独立深拷贝。

`CredentialInfo.Usable` 的管理台语义改为：账号基础凭证可用、未禁用、无账号级冷却，且在已公布的模型中至少有一个当前未处于模型冷却。不能改调度器行为。

状态规则：

1. disabled → `disabled`
2. 账号级冷却 → `cooling_down`
3. 凭证不可用 → `needs_refresh`
4. 无模型 → `pending_models`
5. 所有已公布模型均处于活跃模型冷却 → `cooling_down`
6. 否则 → `ready`（即使部分模型冷却）

当所有已公布模型均冷却且无账号级冷却时，`CooldownUntil` 应表示账号重新能服务任一模型的最早时间（活跃模型冷却的最早 `Until`）。

快照 `validUntil` 必须同时考虑账号级和模型级冷却的最早状态转换，确保不需要外部 mutation 也会在过期后重建。

### 2. Dashboard 统计

- `cooling_accounts`：存在账号级冷却或至少一个活跃模型级冷却的当前账号数（去重）。生产修复后预期为 881，不是 1。
- `usable_accounts`：使用修正后的 `CredentialInfo.Usable`。在生产只有 `grok-4.5` 的情况下，880 个全模型冷却账号不得计为可用。
- `disabled_accounts` 语义保持不变。

### 3. 凭证列表

- `status=cooling_down` 必须包含账号级冷却和“所有已公布模型均冷却”的账号。
- 返回 `model_cooldowns` 供管理台后续展示。
- 排序、cursor、分页缓存不回归。

### 4. 模型 summary

对每个模型独立计算：

- 账号基础不可用/禁用/账号级冷却 → 该模型不可用，按账号状态计数。
- 当前模型存在活跃模型冷却 → 此模型 `status_counts.cooling_down++`，不得计入 `usable_accounts`。
- 某账号只有模型 A 冷却、模型 B 正常时，A 不可用，B 仍可用。

生产修复后 `grok-4.5.usable_accounts` 应比当前值减少约 880（以部署时实时数据为准）。

## 测试要求

新增/补充测试：

1. 仅账号级冷却。
2. 单模型且模型级冷却：凭证 status=cooling_down、usable=false、cooldown_until 正确。
3. 双模型仅一个冷却：凭证 status=ready、usable=true；模型 summary 分别统计。
4. 双模型全部冷却：凭证 status=cooling_down、usable=false、cooldown_until 取最早。
5. 模型冷却到期后 credential snapshot 自动失效重建。
6. Dashboard `cooling_accounts` 对账号级+模型级去重。
7. `model_cooldowns` 深拷贝，调用方不能修改池内状态。
8. `go build ./...`、`go vet ./...`、`go test ./...`、相关 race 测试通过。

## 性能约束

- 16k 账号快照与 Dashboard 聚合仍保持线性。
- 不在每个 Dashboard 请求重新扫描 pool 内部 state；继续使用 generation 快照。
- 不破坏 task-04 的真分页性能与缓存。

## 提交

提交信息：

```text
fix: include model cooldowns in admin account status
```

若沙箱 `.git` 只读，保留工作区改动和完整验证结果，由主会话提交。