import { type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { useTheme } from "next-themes";
import { Link, NavLink, Outlet, useNavigate } from "react-router-dom";
import { Boxes, Gauge, ListChecks, LogOut, Users } from "lucide-react";

import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { useAuth } from "@/lib/use-auth";
import { cn } from "@/lib/cn";
import { i18n } from "@/lib/i18n";

const NAV_ITEMS = [
  { to: "/", key: "nav.dashboard", icon: Gauge, end: true },
  { to: "/accounts", key: "nav.accounts", icon: Users },
  { to: "/models", key: "nav.models", icon: Boxes },
  { to: "/audits", key: "nav.audits", icon: ListChecks },
] as const;

function SideNav() {
  const { t } = useTranslation();
  return (
    <nav className="flex flex-col gap-0.5 px-3">
      {NAV_ITEMS.map(({ to, key, icon: Icon, ...rest }) => (
        <NavLink
          key={to}
          to={to}
          end={"end" in rest}
          className={({ isActive }) =>
            cn(
              "flex h-9 items-center gap-2.5 rounded-md px-3 text-[13px] text-muted-foreground transition-colors hover:bg-accent hover:text-foreground",
              isActive && "bg-accent font-medium text-foreground",
            )
          }
        >
          <Icon className="size-4" strokeWidth={1.8} />
          {t(key)}
        </NavLink>
      ))}
    </nav>
  );
}

export function AppLayout() {
  const { t } = useTranslation();
  const { logout } = useAuth();
  const { theme, setTheme } = useTheme();
  const navigate = useNavigate();

  return (
    <div className="flex min-h-screen">
      {/* 侧边栏 */}
      <aside className="fixed inset-y-0 left-0 z-30 flex w-56 flex-col border-r bg-sidebar">
        <Link to="/" className="flex h-14 items-center border-b px-5">
          <span className="text-[15px] font-semibold tracking-tight">{t("app.name")}</span>
          <span className="ml-1.5 rounded bg-secondary px-1.5 py-0.5 text-[10px] text-muted-foreground">
            {t("app.console")}
          </span>
        </Link>
        <div className="flex-1 overflow-y-auto py-4">
          <SideNav />
        </div>
        <div className="border-t p-3">
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <button className="flex h-9 w-full items-center gap-2 rounded-md px-3 text-[13px] text-muted-foreground hover:bg-accent hover:text-foreground">
                <span className="flex-1 text-left">{t("shell.admin")}</span>
              </button>
            </DropdownMenuTrigger>
            <DropdownMenuContent side="top" align="start" className="w-44">
              <DropdownMenuItem onClick={() => setTheme(theme === "dark" ? "light" : "dark")}>
                {t("shell.theme")}: {t(theme === "dark" ? "shell.dark" : "shell.light")}
              </DropdownMenuItem>
              <DropdownMenuItem onClick={() => void i18n.changeLanguage(i18n.language === "zh-CN" ? "en" : "zh-CN")}>
                {t("shell.language")}: {i18n.language === "zh-CN" ? "简体中文" : "English"}
              </DropdownMenuItem>
              <DropdownMenuItem
                onClick={() => {
                  logout();
                  navigate("/login");
                }}
              >
                <LogOut className="size-4" />
                {t("shell.logout")}
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </aside>

      {/* 主区域 */}
      <main className="ml-56 min-w-0 flex-1 px-8 py-6">
        <Outlet />
      </main>
    </div>
  );
}

export function PageHeader({ title, description, actions }: { title: string; description?: string; actions?: ReactNode }) {
  return (
    <div className="mb-6 flex flex-wrap items-end justify-between gap-3">
      <div>
        <h1 className="text-lg font-semibold tracking-tight">{title}</h1>
        {description ? <p className="mt-1 text-[13px] text-muted-foreground">{description}</p> : null}
      </div>
      {actions ? <div className="flex items-center gap-2">{actions}</div> : null}
    </div>
  );
}

