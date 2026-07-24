import { Box, Eye, Languages, LayoutDashboard, LogOut, Menu, Monitor, Moon, MoreHorizontal, Sun, Users } from "lucide-react";
import { useTheme } from "next-themes";
import { useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Link, NavLink, Outlet } from "react-router-dom";

import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSub, DropdownMenuSubContent, DropdownMenuSubTrigger, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle, SheetTrigger } from "@/components/ui/sheet";
import { useAuth } from "@/shared/auth/use-auth";
import { GitHubMark } from "@/shared/components/github-mark";
import { SiteFooter } from "@/shared/components/site-footer";
import { cn } from "@/shared/lib/cn";

const navigation = [
  { href: "/dashboard", label: "nav.dashboard", icon: LayoutDashboard },
  { href: "/accounts", label: "nav.accounts", icon: Users },
  { href: "/models", label: "nav.models", icon: Box },
  { href: "/request-audits", label: "nav.audits", icon: Eye },
] as const;

export function AppShell() {
  const { t, i18n } = useTranslation();
  const { logout } = useAuth();
  const { setTheme } = useTheme();
  const [mobileOpen, setMobileOpen] = useState(false);

  function navigationLinks(): ReactNode {
    return navigation.map(({ href, label, icon: Icon }) => (
      <NavLink
        key={href}
        to={href}
        onClick={() => setMobileOpen(false)}
        className={({ isActive }) => cn(
          "group flex h-8 items-center gap-2 rounded-md px-2.5 text-xs font-normal text-muted-foreground transition-colors hover:bg-secondary/55 hover:text-foreground",
          isActive && "bg-secondary/60 text-foreground",
        )}
      >
        {({ isActive }) => (
          <>
            <span className="flex size-5 shrink-0 items-center justify-center">
              <Icon className={cn("size-4 text-muted-foreground", isActive && "text-foreground")} fill={isActive ? "currentColor" : "none"} fillOpacity={isActive ? 0.14 : 0} strokeWidth={1.8} />
            </span>
            {t(label)}
          </>
        )}
      </NavLink>
    ));
  }

  const navigationContent = (
    <nav className="mt-7 min-h-0 flex-1 overflow-y-auto overscroll-contain pr-2 pb-2" aria-label={t("shell.navigation")}>
      <div className="space-y-1">{navigationLinks()}</div>
    </nav>
  );

  const accountControl = (
    <div className="flex h-9 items-center gap-1 px-2.5">
      <span className="min-w-0 flex-1 truncate text-xs font-normal text-muted-foreground">{t("shell.adminAccount")}</span>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button variant="ghost" size="icon" className="size-7 shrink-0 text-muted-foreground hover:text-foreground" aria-label={t("common.actions")}><MoreHorizontal /></Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" side="top" sideOffset={8} className="w-56 p-1.5">
          <DropdownMenuSub>
            <DropdownMenuSubTrigger className="h-8"><Sun />{t("shell.appearance")}</DropdownMenuSubTrigger>
            <DropdownMenuSubContent>
              <DropdownMenuItem onClick={() => setTheme("light")}><Sun />{t("shell.light")}</DropdownMenuItem>
              <DropdownMenuItem onClick={() => setTheme("dark")}><Moon />{t("shell.dark")}</DropdownMenuItem>
              <DropdownMenuItem onClick={() => setTheme("system")}><Monitor />{t("shell.system")}</DropdownMenuItem>
            </DropdownMenuSubContent>
          </DropdownMenuSub>
          <DropdownMenuSub>
            <DropdownMenuSubTrigger className="h-8"><Languages />{t("shell.language")}</DropdownMenuSubTrigger>
            <DropdownMenuSubContent>
              <DropdownMenuItem onClick={() => void i18n.changeLanguage("zh-CN")}>简体中文</DropdownMenuItem>
              <DropdownMenuItem onClick={() => void i18n.changeLanguage("en")}>English</DropdownMenuItem>
            </DropdownMenuSubContent>
          </DropdownMenuSub>
          <DropdownMenuItem className="h-8" onClick={() => void logout()}><LogOut />{t("auth.signOut")}</DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  );

  return (
    <div className="min-h-screen bg-background">
        <aside className="fixed inset-y-0 left-0 z-30 hidden h-screen w-[288px] flex-col overflow-hidden bg-sidebar px-4 py-6 lg:flex">
          <div className="flex h-7 shrink-0 items-center justify-between px-2.5">
            <Link to="/dashboard" className="flex h-7 items-baseline gap-2 text-base font-semibold text-foreground">
              <span>{t("appName")}</span>
            </Link>
            <Button variant="ghost" size="icon" className="size-7 text-muted-foreground [&_svg]:size-[15px]" asChild>
              <a href="https://github.com/Futureppo/grokcli2api-go" target="_blank" rel="noreferrer" aria-label="GitHub">
                <GitHubMark />
              </a>
            </Button>
          </div>
          {navigationContent}
          <div className="relative z-10 mt-4 shrink-0 bg-sidebar pt-4">{accountControl}</div>
        </aside>

        <div className="flex min-h-screen flex-col lg:pl-[288px]">
          <header className="flex h-12 items-center justify-between border-b px-4 lg:hidden">
            <Sheet open={mobileOpen} onOpenChange={setMobileOpen}>
              <SheetTrigger asChild><Button variant="ghost" size="icon" className="size-8" aria-label={t("shell.openNavigation")}><Menu className="size-4" /></Button></SheetTrigger>
              <SheetContent side="left" className="flex w-72 flex-col gap-0 bg-sidebar px-3 py-4 [&>button]:right-2 [&>button]:top-3.5 [&>button]:flex [&>button]:size-7 [&>button]:items-center [&>button]:justify-center [&>nav]:mt-5 [&>nav]:pr-1">
                <SheetHeader className="h-7 shrink-0 px-2.5 text-left">
                  <SheetTitle className="flex h-7 items-center text-base">{t("appName")}</SheetTitle>
                  <SheetDescription className="sr-only">{t("shell.navigation")}</SheetDescription>
                </SheetHeader>
                {navigationContent}
                <div className="relative z-10 mt-3 shrink-0 bg-sidebar pt-3">{accountControl}</div>
              </SheetContent>
            </Sheet>
            <span className="text-sm font-semibold">{t("appName")}</span>
            <Button variant="ghost" size="icon" className="size-8 text-muted-foreground hover:text-foreground" asChild>
              <a href="https://github.com/Futureppo/grokcli2api-go" target="_blank" rel="noreferrer" aria-label="GitHub">
                <GitHubMark />
              </a>
            </Button>
          </header>

          <main className="mx-auto w-full max-w-[1280px] flex-1 px-5 py-8 sm:px-8 lg:py-20">
            <Outlet />
          </main>
          <SiteFooter />
        </div>
    </div>
  );
}
