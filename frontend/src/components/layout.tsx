import { useEffect, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { useTheme } from "next-themes";
import { Link, NavLink, Outlet, useNavigate } from "react-router-dom";
import { Boxes, Gauge, ListChecks, LogOut, Menu, PanelLeftClose, PanelLeftOpen, Users, X } from "lucide-react";

import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { useAuth } from "@/lib/use-auth";
import { cn } from "@/lib/cn";
import { i18n } from "@/lib/i18n";

const NAV_ITEMS = [
  { to: "/", key: "nav.dashboard", icon: Gauge, end: true },
  { to: "/accounts", key: "nav.accounts", icon: Users },
  { to: "/models", key: "nav.models", icon: Boxes },
  { to: "/audits", key: "nav.audits", icon: ListChecks },
] as const;

const COLLAPSE_KEY = "grokcli2api:sidebar-collapsed";

function NavLinks({ collapsed, onNavigate }: { collapsed: boolean; onNavigate?: () => void }) {
  const { t } = useTranslation();
  return (
    <nav className={cn("flex flex-col gap-0.5", collapsed ? "px-2" : "px-3")}>
      {NAV_ITEMS.map((item) => {
        const { to, key, icon: Icon } = item;
        const end = "end" in item && item.end;
        const link = (
          <NavLink
            key={to}
            to={to}
            end={end}
            onClick={onNavigate}
            className={({ isActive }) =>
              cn(
                "flex h-9 items-center rounded-md text-[13px] text-muted-foreground transition-colors hover:bg-accent hover:text-foreground",
                collapsed ? "justify-center" : "gap-2.5 px-3",
                isActive && "bg-accent font-medium text-foreground",
              )
            }
          >
            <Icon className="size-4 shrink-0" strokeWidth={1.8} />
            {collapsed ? null : t(key)}
          </NavLink>
        );
        if (!collapsed) return link;
        return (
          <Tooltip key={to}>
            <TooltipTrigger asChild>{link}</TooltipTrigger>
            <TooltipContent side="right">{t(key)}</TooltipContent>
          </Tooltip>
        );
      })}
    </nav>
  );
}

function BrandMark({ collapsed }: { collapsed: boolean }) {
  const { t } = useTranslation();
  if (collapsed) {
    return (
      <span className="flex size-8 items-center justify-center rounded-md bg-primary text-[13px] font-bold text-primary-foreground">
        G
      </span>
    );
  }
  return (
    <>
      <span className="text-[15px] font-semibold tracking-tight">{t("app.name")}</span>
      <span className="ml-1.5 rounded bg-secondary px-1.5 py-0.5 text-[10px] text-muted-foreground">
        {t("app.console")}
      </span>
    </>
  );
}

function UserMenu({ collapsed }: { collapsed: boolean }) {
  const { t } = useTranslation();
  const { logout } = useAuth();
  const { theme, setTheme } = useTheme();
  const navigate = useNavigate();
  return (
    <div className={cn("border-t", collapsed ? "p-2" : "p-3")}>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <button
            className={cn(
              "flex h-9 w-full items-center rounded-md text-[13px] text-muted-foreground hover:bg-accent hover:text-foreground",
              collapsed ? "justify-center" : "px-3",
            )}
          >
            <span className="flex size-6 items-center justify-center rounded-full bg-secondary text-[10px]">A</span>
            {collapsed ? null : <span className="ml-2 flex-1 text-left">{t("shell.admin")}</span>}
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
  );
}

export function AppLayout() {
  const [collapsed, setCollapsed] = useState(() => localStorage.getItem(COLLAPSE_KEY) === "1");
  const [mobileOpen, setMobileOpen] = useState(false);

  function toggleCollapsed() {
    setCollapsed((current) => {
      localStorage.setItem(COLLAPSE_KEY, current ? "0" : "1");
      return !current;
    });
  }

  // 路由切换时关闭移动端抽屉
  useEffect(() => {
    if (!mobileOpen) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") setMobileOpen(false);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [mobileOpen]);

  return (
    <TooltipProvider delayDuration={200}>
      <div className="flex min-h-screen">
        {/* 桌面端侧边栏（lg 及以上显示） */}
        <aside
          className={cn(
            "fixed inset-y-0 left-0 z-30 hidden flex-col border-r bg-sidebar transition-[width] duration-200 lg:flex",
            collapsed ? "w-14" : "w-56",
          )}
        >
          <div className={cn("flex h-14 items-center border-b", collapsed ? "justify-center px-2" : "justify-between px-4")}>
            <Link to="/" className="flex items-center">
              <BrandMark collapsed={collapsed} />
            </Link>
            {!collapsed && (
              <button
                type="button"
                onClick={toggleCollapsed}
                className="text-muted-foreground hover:text-foreground"
                aria-label="collapse"
              >
                <PanelLeftClose className="size-4" />
              </button>
            )}
          </div>
          <div className="flex-1 overflow-y-auto py-4">
            <NavLinks collapsed={collapsed} />
          </div>
          {collapsed && (
            <div className="flex justify-center pb-2">
              <button
                type="button"
                onClick={toggleCollapsed}
                className="text-muted-foreground hover:text-foreground"
                aria-label="expand"
              >
                <PanelLeftOpen className="size-4" />
              </button>
            </div>
          )}
          <UserMenu collapsed={collapsed} />
        </aside>

        {/* 移动端顶栏（lg 以下显示） */}
        <header className="fixed inset-x-0 top-0 z-40 flex h-14 items-center justify-between border-b bg-background px-4 lg:hidden">
          <Link to="/" className="flex items-center">
            <BrandMark collapsed={false} />
          </Link>
          <button
            type="button"
            onClick={() => setMobileOpen(true)}
            className="flex size-9 items-center justify-center rounded-md text-muted-foreground hover:bg-accent hover:text-foreground"
            aria-label="menu"
          >
            <Menu className="size-5" />
          </button>
        </header>

        {/* 移动端抽屉 */}
        {mobileOpen ? (
          <div className="fixed inset-0 z-50 lg:hidden">
            <div className="absolute inset-0 bg-black/40" onClick={() => setMobileOpen(false)} />
            <aside className="absolute inset-y-0 left-0 flex w-64 flex-col border-r bg-sidebar">
              <div className="flex h-14 items-center justify-between border-b px-4">
                <BrandMark collapsed={false} />
                <button
                  type="button"
                  onClick={() => setMobileOpen(false)}
                  className="text-muted-foreground hover:text-foreground"
                  aria-label="close"
                >
                  <X className="size-5" />
                </button>
              </div>
              <div className="flex-1 overflow-y-auto py-4">
                <NavLinks collapsed={false} onNavigate={() => setMobileOpen(false)} />
              </div>
              <UserMenu collapsed={false} />
            </aside>
          </div>
        ) : null}

        {/* 主区域 */}
        <main
          className={cn(
            "min-w-0 flex-1 px-4 pb-6 pt-20 sm:px-6 lg:pt-6",
            collapsed ? "lg:ml-14" : "lg:ml-56",
            "lg:px-8",
          )}
        >
          <Outlet />
        </main>
      </div>
    </TooltipProvider>
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
