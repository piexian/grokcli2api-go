import { createObjectDecoder, isString } from "@/shared/api/decoder";
export type { ApiDecoder } from "@/shared/api/decoder";
import type { ApiDecoder } from "@/shared/api/decoder";
import { runtimeConfig } from "@/shared/config/runtime-config";
import { i18n } from "@/shared/i18n";

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly requestId?: string;

  constructor(status: number, code: string, message: string, requestId?: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
}

const sessionInvalidatedListeners = new Set<() => void>();
const maxEventStreamBufferCharacters = 1 << 20;
const eventStreamInactivityTimeoutMs = 60_000;

// grokcli2api-go 使用 admin key（Bearer）鉴权，无刷新令牌。
let adminKey: string | null = null;

export function setAdminKey(key: string | null): void {
  adminKey = key;
}

export function getAdminKey(): string | null {
  return adminKey;
}

export function subscribeSessionInvalidated(listener: () => void): () => void {
  sessionInvalidatedListeners.add(listener);
  return () => sessionInvalidatedListeners.delete(listener);
}

export function invalidateSession(): void {
  adminKey = null;
  sessionInvalidatedListeners.forEach((listener) => listener());
}

function localizedErrorMessage(code: string, fallback: string): string {
  const key = `apiErrors.${code}`;
  return i18n.exists(key) ? i18n.t(key) : fallback;
}

async function parseResponse<T>(response: Response, decode: ApiDecoder<T>): Promise<T> {
  const payload: unknown = await response.json().catch(() => null);
  if (!response.ok) {
    const error = readErrorEnvelope(payload);
    const code = error.code ?? "requestFailed";
    throw new ApiError(response.status, code, localizedErrorMessage(code, error.message ?? `HTTP ${response.status}`), error.requestId);
  }
  try {
    return decode(payload);
  } catch {
    throw new ApiError(response.status, "invalidResponse", localizedErrorMessage("invalidResponse", "Server returned an invalid response"));
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function readErrorEnvelope(payload: unknown): { code?: string; message?: string; requestId?: string } {
  if (!isRecord(payload)) return {};
  // grokcli2api-go 错误格式：{"error":{"message","type","code"}} 或 {"error":"..."}
  if (typeof payload.error === "string") return { message: payload.error };
  if (isRecord(payload.error)) {
    return {
      code: typeof payload.error.code === "string" ? payload.error.code : undefined,
      message: typeof payload.error.message === "string" ? payload.error.message : undefined,
      requestId: typeof payload.error.requestId === "string" ? payload.error.requestId : undefined,
    };
  }
  return {};
}

type RequestOptions = Omit<RequestInit, "body"> & {
  body?: BodyInit | object;
  authenticated?: boolean;
};

async function sendApiRequest(path: string, options: RequestOptions): Promise<Response> {
  const { authenticated = true, body, headers, ...requestInit } = options;
  const requestHeaders = new Headers(headers);
  let requestBody: BodyInit | undefined;

  if (body instanceof FormData || typeof body === "string" || body instanceof Blob) {
    requestBody = body;
  } else if (body !== undefined) {
    requestHeaders.set("Content-Type", "application/json");
    requestBody = JSON.stringify(body);
  }
  if (authenticated && adminKey) {
    requestHeaders.set("Authorization", `Bearer ${adminKey}`);
  }

  return fetch(`${runtimeConfig.apiBaseUrl}${path}`, {
    ...requestInit,
    body: requestBody,
    headers: requestHeaders,
  });
}

export async function apiRequest<T>(path: string, options: RequestOptions, decode: ApiDecoder<T>): Promise<T> {
  const response = await sendApiRequest(path, options);
  if (response.status === 401 || response.status === 403) {
    // admin key 失效：清空并通知，不再尝试刷新。
    invalidateSession();
  }
  return parseResponse(response, decode);
}

export type ApiStreamEvent<T> = {
  event: string;
  data: T;
};

// apiEventStream 使用 admin key 鉴权发起 POST SSE，并正确处理任意分块边界。
export async function apiEventStream<T>(path: string, options: RequestOptions, decode: ApiDecoder<T>, onEvent: (value: ApiStreamEvent<T>) => void): Promise<void> {
  const response = await sendApiRequest(path, options);
  if (!response.ok) {
    await parseResponse(response, decodeNever);
  }
  if (!response.body) {
    throw new ApiError(response.status, "invalidResponse", localizedErrorMessage("invalidResponse", "Server returned an invalid response"));
  }
  const contentType = response.headers.get("Content-Type")?.toLowerCase() ?? "";
  if (!contentType.startsWith("text/event-stream")) {
    await response.body.cancel().catch(() => undefined);
    throw new ApiError(response.status, "invalidResponse", localizedErrorMessage("invalidResponse", "Server returned an invalid response"));
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  const dispatch = (block: string) => {
    let event = "message";
    const data: string[] = [];
    block.split("\n").forEach((line) => {
      const normalized = line.endsWith("\r") ? line.slice(0, -1) : line;
      if (normalized.startsWith("event:")) event = normalized.slice(6).trim();
      if (normalized.startsWith("data:")) data.push(normalized.slice(5).trimStart());
    });
    if (data.length === 0) return;
    let payload: T;
    try {
      payload = decode(JSON.parse(data.join("\n")) as unknown);
    } catch {
      throw new ApiError(response.status, "invalidResponse", localizedErrorMessage("invalidResponse", "Server returned an invalid response"));
    }
    onEvent({ event, data: payload });
  };

  try {
    for (;;) {
      const { done, value } = await readEventStreamChunk(reader, response.status);
      buffer += decoder.decode(value, { stream: !done });
      buffer = buffer.replaceAll("\r\n", "\n");
      let boundary = buffer.indexOf("\n\n");
      while (boundary >= 0) {
        if (boundary > maxEventStreamBufferCharacters) {
          throw new ApiError(response.status, "invalidResponse", localizedErrorMessage("invalidResponse", "Server returned an invalid response"));
        }
        dispatch(buffer.slice(0, boundary));
        buffer = buffer.slice(boundary + 2);
        boundary = buffer.indexOf("\n\n");
      }
      if (buffer.length > maxEventStreamBufferCharacters) {
        throw new ApiError(response.status, "invalidResponse", localizedErrorMessage("invalidResponse", "Server returned an invalid response"));
      }
      if (done) break;
    }
    if (buffer.trim()) dispatch(buffer);
  } catch (error) {
    await reader.cancel().catch(() => undefined);
    throw error;
  } finally {
    reader.releaseLock();
  }
}

async function readEventStreamChunk(reader: ReadableStreamDefaultReader<Uint8Array>, status: number): Promise<ReadableStreamReadResult<Uint8Array>> {
  let timeout = 0;
  const inactivity = new Promise<never>((_, reject) => {
    timeout = window.setTimeout(() => {
      reject(new ApiError(status, "streamTimeout", localizedErrorMessage("streamTimeout", "The progress stream stopped responding")));
    }, eventStreamInactivityTimeoutMs);
  });
  try {
    return await Promise.race([reader.read(), inactivity]);
  } finally {
    window.clearTimeout(timeout);
  }
}

export async function apiDownload(path: string): Promise<Blob> {
  const headers = new Headers();
  if (adminKey) headers.set("Authorization", `Bearer ${adminKey}`);
  const response = await fetch(`${runtimeConfig.apiBaseUrl}${path}`, { headers });
  if (!response.ok) {
    await parseResponse(response, decodeNever);
    throw new ApiError(response.status, "requestFailed", localizedErrorMessage("requestFailed", "The request failed"));
  }
  return response.blob();
}

export type AdminDTO = {
  id: string;
  username: string;
};

export const decodeAdminDTO = createObjectDecoder<AdminDTO>("admin", { id: isString, username: isString });

function decodeNever(): never {
  throw new Error("unexpected successful response");
}

export type PaginatedDTO<T> = {
  items: T[];
  page: number;
  pageSize: number;
  total: number;
};
