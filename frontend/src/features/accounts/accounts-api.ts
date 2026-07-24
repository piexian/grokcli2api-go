import { apiRequest, type ApiDecoder } from "@/shared/api/client";
import { isArrayOf, isBoolean, isObject, isString } from "@/shared/api/decoder";

// grokcli2api-go 的 CredentialInfo（脱敏视图），与 internal/auth/pool.go 对齐。
export type BillingDTO = {
  usagePercent?: number;
  periodType?: string;
  periodEnd?: string;
  onDemandRemainingCents?: number;
  prepaidBalanceCents?: number;
  unifiedBilling?: boolean;
  exhausted: boolean;
  updatedAt: string;
};

export type CredentialDTO = {
  id: string;
  scope?: string;
  authMode?: string;
  status: "ready" | "cooling_down" | "disabled" | "needs_refresh" | "pending_models" | string;
  usable: boolean;
  disabled: boolean;
  expiresAt?: string;
  cooldownUntil?: string;
  models: string[];
  discoveryStatus?: string;
  catalogEtag?: string;
  catalogUpdatedAt?: string;
  hasRefreshToken: boolean;
  subscriptionTier?: string;
  subscriptionTierDisplay?: string;
  billing?: BillingDTO;
};

export type CredentialListDTO = {
  object: "list";
  data: CredentialDTO[];
};

const credentialValidator = (value: unknown): boolean => {
  if (!isObject(value)) return false;
  const v = value as Record<string, unknown>;
  const id = v.id;
  const status = v.status;
  const usable = v.usable;
  const disabled = v.disabled;
  const models = v.models;
  return (
    isString(id) &&
    isString(status) &&
    isBoolean(usable) &&
    isBoolean(disabled) &&
    Array.isArray(models) &&
    models.every(isString)
  );
};

// 后端是 snake_case；在此做归一化。
// eslint-disable-next-line @typescript-eslint/no-explicit-any
function normalizeCredential(raw: any): CredentialDTO {
  return {
    id: raw.id,
    scope: raw.scope,
    authMode: raw.auth_mode,
    status: raw.status,
    usable: raw.usable,
    disabled: raw.disabled,
    expiresAt: raw.expires_at,
    cooldownUntil: raw.cooldown_until,
    models: raw.models ?? [],
    discoveryStatus: raw.discovery_status,
    catalogEtag: raw.catalog_etag,
    catalogUpdatedAt: raw.catalog_updated_at,
    hasRefreshToken: raw.has_refresh_token ?? false,
    subscriptionTier: raw.subscription_tier,
    subscriptionTierDisplay: raw.subscription_tier_display,
    billing: raw.billing
      ? {
          usagePercent: raw.billing.usage_percent,
          periodType: raw.billing.period_type,
          periodEnd: raw.billing.period_end,
          onDemandRemainingCents: raw.billing.on_demand_remaining_cents,
          prepaidBalanceCents: raw.billing.prepaid_balance_cents,
          unifiedBilling: raw.billing.unified_billing,
          exhausted: raw.billing.exhausted ?? false,
          updatedAt: raw.billing.updated_at,
        }
      : undefined,
  };
}

const decodeCredentialList: ApiDecoder<CredentialDTO[]> = (value: unknown) => {
  if (!isObject(value) || !isArrayOf(credentialValidator)((value as Record<string, unknown>).data)) {
    throw new Error("invalid credential list");
  }
  return ((value as Record<string, unknown>).data as unknown[]).map(normalizeCredential);
};

export function listCredentials(): Promise<CredentialDTO[]> {
  return apiRequest("/v1/admin/credentials", { method: "GET" }, decodeCredentialList);
}

export type UploadCredentialResultDTO = {
  credential: CredentialDTO;
  created: boolean;
  modelDiscovery: string;
  warning?: string;
};

// eslint-disable-next-line @typescript-eslint/no-explicit-any
const decodeUploadResult: ApiDecoder<UploadCredentialResultDTO> = (value: any) => {
  if (!isObject(value) || !credentialValidator(value.credential)) {
    throw new Error("invalid upload result");
  }
  return {
    credential: normalizeCredential(value.credential),
    created: Boolean(value.created),
    modelDiscovery: typeof value.model_discovery === "string" ? value.model_discovery : "unknown",
    warning: typeof value.warning === "string" ? value.warning : undefined,
  };
};

export function uploadCredentialJson(raw: string): Promise<UploadCredentialResultDTO> {
  return apiRequest(
    "/v1/admin/credentials",
    { method: "POST", body: raw },
    decodeUploadResult,
  );
}

export function uploadCredentialFile(file: File): Promise<UploadCredentialResultDTO> {
  const form = new FormData();
  form.append("file", file, file.name);
  return apiRequest("/v1/admin/credentials", { method: "POST", body: form }, decodeUploadResult);
}

export function deleteCredential(id: string): Promise<void> {
  return apiRequest(
    `/v1/admin/credentials/${encodeURIComponent(id)}`,
    { method: "DELETE" },
    () => undefined,
  );
}
