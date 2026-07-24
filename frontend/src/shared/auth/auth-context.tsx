import { useCallback, useEffect, useState, type ReactNode } from "react";

import { invalidateSession, setAdminKey, subscribeSessionInvalidated } from "@/shared/api/client";
import { AuthContext, type AuthStatus } from "@/shared/auth/auth-state";

const storageKey = "grokcli2api:admin-key";

export function AuthProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<AuthStatus>("restoring");

  const restoreSession = useCallback(async (): Promise<void> => {
    setStatus("restoring");
    const stored = window.localStorage.getItem(storageKey);
    if (!stored) {
      setStatus("anonymous");
      return;
    }
    // 用 admin key 探测凭证列表验证有效性。
    try {
      const response = await fetch("/v1/admin/credentials", {
        headers: { Authorization: `Bearer ${stored}` },
      });
      if (response.ok) {
        setAdminKey(stored);
        setStatus("authenticated");
        return;
      }
      if (response.status === 401 || response.status === 403) {
        window.localStorage.removeItem(storageKey);
        setStatus("anonymous");
        return;
      }
      setStatus("unavailable");
    } catch {
      setStatus("unavailable");
    }
  }, []);

  useEffect(() => {
    const unsubscribe = subscribeSessionInvalidated(() => {
      window.localStorage.removeItem(storageKey);
      setStatus("anonymous");
    });

    const restoreTimer = window.setTimeout(() => {
      void restoreSession();
    }, 0);

    return () => {
      window.clearTimeout(restoreTimer);
      unsubscribe();
    };
  }, [restoreSession]);

  async function login(adminKey: string): Promise<void> {
    const key = adminKey.trim();
    if (!key) throw new Error("empty admin key");
    const response = await fetch("/v1/admin/credentials", {
      headers: { Authorization: `Bearer ${key}` },
    });
    if (!response.ok) {
      if (response.status === 401 || response.status === 403) {
        const { ApiError } = await import("@/shared/api/client");
        throw new ApiError(response.status, "invalidAdminKey", "Invalid admin key");
      }
      const { ApiError } = await import("@/shared/api/client");
      throw new ApiError(response.status, "requestFailed", `HTTP ${response.status}`);
    }
    window.localStorage.setItem(storageKey, key);
    setAdminKey(key);
    setStatus("authenticated");
  }

  async function logout(): Promise<void> {
    window.localStorage.removeItem(storageKey);
    invalidateSession();
    setStatus("anonymous");
  }

  return (
    <AuthContext.Provider value={{ status, retryRestore: restoreSession, login, logout }}>
      {children}
    </AuthContext.Provider>
  );
}
