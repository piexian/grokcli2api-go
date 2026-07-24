import { apiRequest } from "@/shared/api/client";

// 与 docs/backend-task-01-audits.md 契约对齐（snake_case 响应）。
export type AuditDTO = {
  id: string;
  requestId: string;
  createdAt: number;
  protocol: string;
  model: string;
  accountId?: string;
  tenantKey?: string;
  statusCode: number;
  streaming: boolean;
  durationMs: number;
  inputTokens: number;
  cachedInputTokens: number;
  outputTokens: number;
  reasoningTokens: number;
  totalTokens: number;
  errorCode?: string;
  attemptCount: number;
};

export type AuditCursorPageDTO = {
  items: AuditDTO[];
  pageSize: number;
  nextCursor: string;
  hasMore: boolean;
};

export type AuditQuery = {
  cursor?: string;
  limit?: number;
  model?: string;
  accountId?: string;
  protocol?: string;
  status?: string;
  since?: string;
  until?: string;
};

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function normalizeAudit(raw: any): AuditDTO {
  return {
    id: raw.id ?? "",
    requestId: raw.request_id ?? "",
    createdAt: raw.created_at ?? 0,
    protocol: raw.protocol ?? "",
    model: raw.model ?? "",
    accountId: raw.account_id || undefined,
    tenantKey: raw.tenant_key || undefined,
    statusCode: raw.status_code ?? 0,
    streaming: Boolean(raw.streaming),
    durationMs: raw.duration_ms ?? 0,
    inputTokens: raw.input_tokens ?? 0,
    cachedInputTokens: raw.cached_input_tokens ?? 0,
    outputTokens: raw.output_tokens ?? 0,
    reasoningTokens: raw.reasoning_tokens ?? 0,
    totalTokens: raw.total_tokens ?? 0,
    errorCode: raw.error_code || undefined,
    attemptCount: raw.attempt_count ?? 0,
  };
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function decodePage(value: any): AuditCursorPageDTO {
  if (typeof value !== "object" || value === null || !Array.isArray(value.items)) {
    throw new Error("invalid audit page");
  }
  return {
    items: value.items.map(normalizeAudit),
    pageSize: value.page_size ?? value.items.length,
    nextCursor: value.next_cursor ?? "",
    hasMore: Boolean(value.has_more),
  };
}

export function getRequestAudits(query: AuditQuery): Promise<AuditCursorPageDTO> {
  const params = new URLSearchParams();
  if (query.cursor) params.set("cursor", query.cursor);
  if (query.limit) params.set("limit", String(query.limit));
  if (query.model) params.set("model", query.model);
  if (query.accountId) params.set("account_id", query.accountId);
  if (query.protocol) params.set("protocol", query.protocol);
  if (query.status) params.set("status", query.status);
  if (query.since) params.set("since", query.since);
  if (query.until) params.set("until", query.until);
  const suffix = params.toString();
  return apiRequest(`/v1/admin/audits${suffix ? `?${suffix}` : ""}`, { method: "GET" }, decodePage);
}
