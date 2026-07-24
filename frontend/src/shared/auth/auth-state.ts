import { createContext } from "react";

export type AuthStatus = "restoring" | "authenticated" | "anonymous" | "unavailable";

export type AuthContextValue = {
  status: AuthStatus;
  retryRestore: () => Promise<void>;
  login: (adminKey: string) => Promise<void>;
  logout: () => Promise<void>;
};

export const AuthContext = createContext<AuthContextValue | null>(null);
