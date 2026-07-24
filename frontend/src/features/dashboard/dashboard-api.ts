import { apiRequest } from "@/shared/api/client";
import type { PeriodValue } from "@/shared/lib/period";

// 与 docs/backend-task-01-audits.md 中 Codex 实现的契约对齐（snake_case）。
export type DashboardPeriod = PeriodValue;

export type UsageAggregateDTO = {
  requests: number;
  successfulRequests: number;
  failedRequests: number;
  inputTokens: number;
  cachedInputTokens: number;
  outputTokens: number;
  reasoningTokens: number;
  totalTokens: number;
  averageDurationMs: number;
  successRate: number;
};

export type ModelUsageDTO = UsageAggregateDTO & { model: string };

export type SeriesPointDTO = {
  bucket: string;
  requests: number;
  successfulRequests: number;
  failedRequests: number;
  inputTokens: number;
  outputTokens: number;
  totalTokens: number;
};

export type DashboardDTO = {
  period: DashboardPeriod;
  generatedAt: string;
  range: { start: string; end: string };
  resources: {
    totalAccounts: number;
    usableAccounts: number;
    disabledAccounts: number;
    coolingAccounts: number;
    paidAccounts: number;
    totalModels: number;
  };
  usage: UsageAggregateDTO;
  byModel: ModelUsageDTO[];
  series: SeriesPointDTO[];
};

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function normalizeUsage(raw: any): UsageAggregateDTO {
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

// eslint-disable-next-line @typescript-eslint/no-explicit-any
function decodeDashboard(value: any): DashboardDTO {
  if (typeof value !== "object" || value === null) throw new Error("invalid dashboard");
  const resources = value.resources ?? {};
  return {
    period: value.period ?? "today",
    generatedAt: value.generated_at ?? "",
    range: { start: value.range?.start ?? "", end: value.range?.end ?? "" },
    resources: {
      totalAccounts: resources.total_accounts ?? 0,
      usableAccounts: resources.usable_accounts ?? 0,
      disabledAccounts: resources.disabled_accounts ?? 0,
      coolingAccounts: resources.cooling_accounts ?? 0,
      paidAccounts: resources.paid_accounts ?? 0,
      totalModels: resources.total_models ?? 0,
    },
    usage: normalizeUsage(value.usage),
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    byModel: Array.isArray(value.by_model) ? value.by_model.map((item: any) => ({ model: item.model ?? "", ...normalizeUsage(item) })) : [],
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    series: Array.isArray(value.series) ? value.series.map((item: any) => ({
      bucket: item.bucket ?? "",
      requests: item.requests ?? 0,
      successfulRequests: item.successful_requests ?? 0,
      failedRequests: item.failed_requests ?? 0,
      inputTokens: item.input_tokens ?? 0,
      outputTokens: item.output_tokens ?? 0,
      totalTokens: item.total_tokens ?? 0,
    })) : [],
  };
}

export function getDashboard(period: DashboardPeriod): Promise<DashboardDTO> {
  return apiRequest(`/v1/admin/dashboard?period=${encodeURIComponent(period)}`, { method: "GET" }, decodeDashboard);
}
