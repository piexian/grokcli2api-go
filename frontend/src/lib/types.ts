// grokcli2api-go admin API 类型定义（后端响应为 snake_case，此处归一化为 camelCase）。

export type BillingInfo = {
  usagePercent?: number;
  periodType?: string;
  periodEnd?: string;
  onDemandRemainingCents?: number;
  prepaidBalanceCents?: number;
  unifiedBilling?: boolean;
  exhausted: boolean;
  updatedAt: string;
};

export type Credential = {
  id: string;
  scope?: string;
  authMode?: string;
  status: string;
  usable: boolean;
  disabled: boolean;
  expiresAt?: string;
  cooldownUntil?: string;
  models: string[];
  discoveryStatus?: string;
  hasRefreshToken: boolean;
  subscriptionTier?: string;
  subscriptionTierDisplay?: string;
  billing?: BillingInfo;
};

export type CredentialPage = {
  items: Credential[];
  hasMore: boolean;
  nextCursor: string;
  total: number;
};

export type AuditRecord = {
  id: string;
  requestId: string;
  createdAt: number;
  protocol: string;
  model: string;
  accountId?: string;
  statusCode: number;
  streaming: boolean;
  durationMs: number;
  inputTokens: number;
  outputTokens: number;
  totalTokens: number;
  errorCode?: string;
  attemptCount: number;
};

export type AuditPage = {
  items: AuditRecord[];
  pageSize: number;
  nextCursor: string;
  hasMore: boolean;
};

export type UsageAggregate = {
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

export type Dashboard = {
  period: string;
  generatedAt: string;
  resources: {
    totalAccounts: number;
    usableAccounts: number;
    disabledAccounts: number;
    coolingAccounts: number;
    paidAccounts: number;
    totalModels: number;
  };
  usage: UsageAggregate;
  byModel: Array<UsageAggregate & { model: string }>;
  series: Array<{
    bucket: string;
    requests: number;
    failedRequests: number;
    totalTokens: number;
  }>;
};

export type AuditHealth = {
  queueSize: number;
  queueCap: number;
  droppedTotal: number;
  oldestPendingMs: number;
};
