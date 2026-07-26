import type {
  AuditHealth,
  AuditPage,
  AuditRecord,
  Credential,
  CredentialPage,
  Dashboard,
} from "@/lib/types";

const KEY_STORAGE = "grokcli2api:admin-key";

export class ApiError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

export function getAdminKey(): string | null {
  return localStorage.getItem(KEY_STORAGE);
}

export function setAdminKey(key: string | null): void {
  if (key) localStorage.setItem(KEY_STORAGE, key);
  else localStorage.removeItem(KEY_STORAGE);
}

type ErrorPayload = { error?: { message?: string } | string };

export async function apiRequest<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers);
  const key = getAdminKey();
  if (key) headers.set("Authorization", `Bearer ${key}`);

  const response = await fetch(path, { ...init, headers });
  if (response.status === 401 || response.status === 403) {
    setAdminKey(null);
    // 通知路由层回到登录页
    window.dispatchEvent(new Event("grokcli2api:unauthorized"));
  }
  const text = await response.text();
  let payload: unknown = null;
  try {
    payload = text ? JSON.parse(text) : null;
  } catch {
    /* 非 JSON 响应 */
  }
  if (!response.ok) {
    const err = (payload ?? {}) as ErrorPayload;
    const message =
      typeof err.error === "string"
        ? err.error
        : (err.error?.message ?? `HTTP ${response.status}`);
    throw new ApiError(response.status, message);
  }
  return payload as T;
}

/* ---------- snake_case → 前端类型 ---------- */

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function toCredential(raw: any): Credential {
  return {
    id: raw.id,
    scope: raw.scope,
    authMode: raw.auth_mode,
    status: raw.status,
    usable: Boolean(raw.usable),
    disabled: Boolean(raw.disabled),
    disabledReason: raw.disabled_reason,
    expiresAt: raw.expires_at,
    cooldownUntil: raw.cooldown_until,
    cooldownReason: raw.cooldown_reason,
    models: raw.models ?? [],
    discoveryStatus: raw.discovery_status,
    hasRefreshToken: Boolean(raw.has_refresh_token),
    subscriptionTier: raw.subscription_tier,
    subscriptionTierDisplay: raw.subscription_tier_display,
    buildRouteMode: raw.build_route_mode ?? "auto",
    buildSuperEntitled: Boolean(raw.build_super_entitled),
    buildSuperEntitledOverride: Boolean(raw.build_super_entitled_override),
    buildBotFlagged: Boolean(raw.build_bot_flagged),
    buildApiFallback: Boolean(raw.build_api_fallback),
    buildEffectiveRoute: raw.build_effective_route ?? (raw.auth_mode === "api_key" ? "xai" : "build"),
    billing: raw.billing
      ? {
          usagePercent: raw.billing.usage_percent,
          periodType: raw.billing.period_type,
          periodEnd: raw.billing.period_end,
          onDemandRemainingCents: raw.billing.on_demand_remaining_cents,
          prepaidBalanceCents: raw.billing.prepaid_balance_cents,
          unifiedBilling: raw.billing.unified_billing,
          exhausted: Boolean(raw.billing.exhausted),
          updatedAt: raw.billing.updated_at,
        }
      : undefined,
  };
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function toAudit(raw: any): AuditRecord {
  return {
    id: raw.id,
    requestId: raw.request_id ?? "",
    createdAt: raw.created_at ?? 0,
    protocol: raw.protocol ?? "",
    model: raw.model ?? "",
    accountId: raw.account_id || undefined,
    statusCode: raw.status_code ?? 0,
    streaming: Boolean(raw.streaming),
    durationMs: raw.duration_ms ?? 0,
    inputTokens: raw.input_tokens ?? 0,
    outputTokens: raw.output_tokens ?? 0,
    totalTokens: raw.total_tokens ?? 0,
    errorCode: raw.error_code || undefined,
    attemptCount: raw.attempt_count ?? 0,
  };
}

/* ---------- 凭证 ---------- */

const PAGE_SIZE = 100;

export type CredentialQuery = {
  cursor?: string;
  limit?: number;
  q?: string;
  status?: string;
  usable?: boolean;
  sort?: "id" | "tier" | "status" | "expires_at" | "models_count" | "usable";
  order?: "asc" | "desc";
};

/** 服务端分页拉取一页凭证（懒加载：只取可视所需，不预拉全量）。 */
export async function fetchCredentialPage(query: CredentialQuery): Promise<CredentialPage> {
  const params = new URLSearchParams({ limit: String(query.limit ?? PAGE_SIZE) });
  if (query.cursor) params.set("cursor", query.cursor);
  if (query.q) params.set("q", query.q);
  if (query.status) params.set("status", query.status);
  if (query.usable !== undefined) params.set("usable", String(query.usable));
  if (query.sort) params.set("sort", query.sort);
  if (query.order) params.set("order", query.order);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const page = await apiRequest<any>(`/v1/admin/credentials?${params}`);
  return {
    items: (page.data ?? []).map(toCredential),
    hasMore: page.has_more === true,
    nextCursor: page.next_cursor ?? "",
    total: page.total ?? 0,
  };
}

export async function uploadCredential(content: string | File): Promise<{ modelDiscovery: string }> {
  let init: RequestInit;
  if (typeof content === "string") {
    init = { method: "POST", body: content, headers: { "Content-Type": "application/json" } };
  } else {
    const form = new FormData();
    form.append("file", content, content.name);
    init = { method: "POST", body: form };
  }
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const result = await apiRequest<any>("/v1/admin/credentials", init);
  return { modelDiscovery: result.model_discovery ?? "unknown" };
}

export type UploadItemResult = {
  name: string;
  ok: boolean;
  error?: string;
};

/** 把粘贴文本拆成凭证对象：支持 JSON 数组、单个对象和 NDJSON。 */
export function splitCredentialPayloads(text: string): string[] {
  const trimmed = text.trim();
  if (!trimmed) return [];

  try {
    const parsed: unknown = JSON.parse(trimmed);
    if (Array.isArray(parsed)) {
      const objects = parsed.filter(
        (item): item is Record<string, unknown> =>
          typeof item === "object" && item !== null && !Array.isArray(item),
      );
      return objects.length === parsed.length ? objects.map((item) => JSON.stringify(item)) : [];
    }
    if (typeof parsed === "object" && parsed !== null) return [JSON.stringify(parsed)];
    return [];
  } catch {
    // 顶层不是完整 JSON 时，按 NDJSON 逐行严格解析。
  }

  const payloads: string[] = [];
  for (const line of trimmed.split(/\r?\n/).map((item) => item.trim()).filter(Boolean)) {
    try {
      const parsed: unknown = JSON.parse(line);
      if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) return [];
      payloads.push(JSON.stringify(parsed));
    } catch {
      return [];
    }
  }
  return payloads;
}

/** 批量上传：最多 4 路并发，单项失败不影响其余项。 */
export async function uploadCredentialsBatch(
  items: Array<{ name: string; content: string | File }>,
  onProgress?: (done: number, total: number) => void,
): Promise<UploadItemResult[]> {
  const results = new Array<UploadItemResult>(items.length);
  let nextIndex = 0;
  let completed = 0;
  const workerCount = Math.min(4, items.length);

  async function worker(): Promise<void> {
    for (;;) {
      const index = nextIndex++;
      if (index >= items.length) return;
      const item = items[index];
      try {
        await uploadCredential(item.content);
        results[index] = { name: item.name, ok: true };
      } catch (error) {
        results[index] = {
          name: item.name,
          ok: false,
          error: error instanceof Error ? error.message : "upload failed",
        };
      }
      completed += 1;
      onProgress?.(completed, items.length);
    }
  }

  await Promise.all(Array.from({ length: workerCount }, () => worker()));
  return results;
}

export async function deleteCredential(id: string): Promise<void> {
  await apiRequest(`/v1/admin/credentials/${encodeURIComponent(id)}`, { method: "DELETE" });
}
export type CredentialRoutingUpdate = {
  buildRouteMode?: Credential["buildRouteMode"];
  buildSuperEntitled?: boolean;
  buildApiFallback?: boolean;
};

export async function updateCredentialRouting(id: string, update: CredentialRoutingUpdate): Promise<Credential> {
  const body: Record<string, unknown> = {};
  if (update.buildRouteMode !== undefined) body.build_route_mode = update.buildRouteMode;
  if (update.buildSuperEntitled !== undefined) body.build_super_entitled = update.buildSuperEntitled;
  if (update.buildApiFallback !== undefined) body.build_api_fallback = update.buildApiFallback;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const result = await apiRequest<any>(`/v1/admin/credentials/${encodeURIComponent(id)}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  return toCredential(result.credential);
}

/* ---------- 审计 ---------- */

export async function fetchAudits(params: {
  cursor?: string;
  limit?: number;
  model?: string;
  protocol?: string;
}): Promise<AuditPage> {
  const search = new URLSearchParams();
  if (params.cursor) search.set("cursor", params.cursor);
  search.set("limit", String(params.limit ?? 50));
  if (params.model) search.set("model", params.model);
  if (params.protocol) search.set("protocol", params.protocol);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const page = await apiRequest<any>(`/v1/admin/audits?${search}`);
  return {
    items: (page.items ?? []).map(toAudit),
    pageSize: page.page_size ?? 0,
    nextCursor: page.next_cursor ?? "",
    hasMore: Boolean(page.has_more),
  };
}

export async function fetchAuditHealth(): Promise<AuditHealth> {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const raw = await apiRequest<any>("/v1/admin/audits/health");
  return {
    queueSize: raw.queue_size ?? 0,
    queueCap: raw.queue_cap ?? 0,
    droppedTotal: raw.dropped_total ?? 0,
    oldestPendingMs: raw.oldest_pending_ms ?? 0,
  };
}

/* ---------- Dashboard ---------- */

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function toUsage(raw: any) {
  return {
    requests: raw?.requests ?? 0,
    successfulRequests: raw?.successful_requests ?? 0,
    failedRequests: raw?.failed_requests ?? 0,
    inputTokens: raw?.input_tokens ?? 0,
    cachedInputTokens: raw?.cached_input_tokens ?? 0,
    outputTokens: raw?.output_tokens ?? 0,
    reasoningTokens: raw?.reasoning_tokens ?? 0,
    totalTokens: raw?.total_tokens ?? 0,
    averageDurationMs: raw?.average_duration_ms ?? 0,
    successRate: raw?.success_rate ?? 0,
  };
}

export async function fetchDashboard(period: string): Promise<Dashboard> {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const raw = await apiRequest<any>(`/v1/admin/dashboard?period=${encodeURIComponent(period)}`);
  const resources = raw.resources ?? {};
  return {
    period: raw.period ?? period,
    generatedAt: raw.generated_at ?? "",
    resources: {
      totalAccounts: resources.total_accounts ?? 0,
      usableAccounts: resources.usable_accounts ?? 0,
      disabledAccounts: resources.disabled_accounts ?? 0,
      coolingAccounts: resources.cooling_accounts ?? 0,
      paidAccounts: resources.paid_accounts ?? 0,
      totalModels: resources.total_models ?? 0,
    },
    usage: toUsage(raw.usage),
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    byModel: (raw.by_model ?? []).map((m: any) => ({ model: m.model ?? "", ...toUsage(m) })),
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    series: (raw.series ?? []).map((s: any) => ({
      bucket: s.bucket ?? s.start ?? "",
      requests: s.requests ?? 0,
      failedRequests: s.failed_requests ?? 0,
      totalTokens: s.total_tokens ?? 0,
    })),
  };
}

/* ---------- 登录探测 ---------- */

/** 验证 admin key 是否有效（拉一页凭证探测）。 */
export async function probeAdminKey(key: string): Promise<boolean> {
  const response = await fetch("/v1/admin/credentials?limit=1", {
    headers: { Authorization: `Bearer ${key}` },
  });
  return response.ok;
}
