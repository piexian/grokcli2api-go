import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { useNavigate } from "react-router-dom";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useAuth } from "@/lib/use-auth";

export function LoginPage() {
  const { t } = useTranslation();
  const { login } = useAuth();
  const navigate = useNavigate();
  const [key, setKey] = useState("");
  const [error, setError] = useState("");
  const [pending, setPending] = useState(false);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!key.trim()) {
      setError(t("login.keyRequired"));
      return;
    }
    setPending(true);
    setError("");
    try {
      await login(key);
      navigate("/", { replace: true });
    } catch {
      setError(t("login.keyInvalid"));
    } finally {
      setPending(false);
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-background px-4">
      <div className="w-full max-w-sm">
        <div className="mb-8 text-center">
          <span className="text-xl font-semibold tracking-tight">{t("app.name")}</span>
          <span className="ml-2 rounded bg-secondary px-1.5 py-0.5 text-[10px] text-muted-foreground">
            {t("app.console")}
          </span>
        </div>
        <form onSubmit={submit} className="rounded-lg border bg-card p-6 shadow-sm">
          <h1 className="text-[15px] font-medium">{t("login.title")}</h1>
          <p className="mt-1.5 text-[13px] leading-5 text-muted-foreground">{t("login.subtitle")}</p>
          <div className="mt-5 space-y-2">
            <Label htmlFor="admin-key">{t("login.keyLabel")}</Label>
            <Input
              id="admin-key"
              type="password"
              autoFocus
              autoComplete="off"
              value={key}
              onChange={(event) => setKey(event.target.value)}
              aria-invalid={Boolean(error)}
            />
            {error ? <p className="text-[13px] text-destructive">{error}</p> : null}
          </div>
          <Button type="submit" className="mt-5 w-full" disabled={pending}>
            {pending ? t("login.submitting") : t("login.submit")}
          </Button>
        </form>
      </div>
    </div>
  );
}
