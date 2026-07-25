/* eslint-disable react-refresh/only-export-components -- context 与 provider 同文件是官方模式 */
import { createContext, useCallback, useEffect, useState, type ReactNode } from "react";

import { getAdminKey, probeAdminKey, setAdminKey } from "@/lib/api";

type AuthState = "restoring" | "authed" | "guest";

export type AuthContextValue = {
  state: AuthState;
  login: (key: string) => Promise<void>;
  logout: () => void;
};

export const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  // 无存储密钥时直接是 guest，避免一次无意义的 restoring 闪烁。
  const [state, setState] = useState<AuthState>(() => (getAdminKey() ? "restoring" : "guest"));

  useEffect(() => {
    const stored = getAdminKey();
    if (!stored) return;
    let cancelled = false;
    void probeAdminKey(stored).then((ok) => {
      if (cancelled) return;
      if (ok) {
        setState("authed");
      } else {
        setAdminKey(null);
        setState("guest");
      }
    });
    // 任意 API 返回 401/403 时回到登录页
    const onUnauthorized = () => setState("guest");
    window.addEventListener("grokcli2api:unauthorized", onUnauthorized);
    return () => {
      cancelled = true;
      window.removeEventListener("grokcli2api:unauthorized", onUnauthorized);
    };
  }, []);

  const login = useCallback(async (key: string) => {
    const trimmed = key.trim();
    const ok = await probeAdminKey(trimmed);
    if (!ok) throw new Error("invalid_admin_key");
    setAdminKey(trimmed);
    setState("authed");
  }, []);

  const logout = useCallback(() => {
    setAdminKey(null);
    setState("guest");
  }, []);

  return <AuthContext.Provider value={{ state, login, logout }}>{children}</AuthContext.Provider>;
}
