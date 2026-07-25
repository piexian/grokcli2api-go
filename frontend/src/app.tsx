import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "next-themes";
import { useState } from "react";
import { createBrowserRouter, Navigate, RouterProvider } from "react-router-dom";
import { Toaster } from "sonner";

import { AppLayout } from "@/components/layout";
import { AuthProvider } from "@/lib/auth";
import { useAuth } from "@/lib/use-auth";
import { i18n } from "@/lib/i18n";
import { LoginPage } from "@/pages/login";
import { Skeleton } from "@/components/ui/skeleton";
import { lazy, Suspense } from "react";

// 页面级代码分割：首屏只加载 login + 当前页所需 chunk。
const AccountsPage = lazy(() => import("@/pages/accounts").then((m) => ({ default: m.AccountsPage })));
const AuditsPage = lazy(() => import("@/pages/audits").then((m) => ({ default: m.AuditsPage })));
const DashboardPage = lazy(() => import("@/pages/dashboard").then((m) => ({ default: m.DashboardPage })));
const ModelsPage = lazy(() => import("@/pages/models").then((m) => ({ default: m.ModelsPage })));

function Deferred({ children }: { children: React.ReactNode }) {
  return (
    <Suspense fallback={<div className="flex min-h-60 items-center justify-center"><Skeleton className="h-8 w-32" /></div>}>
      {children}
    </Suspense>
  );
}

void i18n;

function FullScreenLoading() {
  return (
    <div className="flex min-h-screen items-center justify-center">
      <Skeleton className="h-8 w-32" />
    </div>
  );
}

function RequireAuth({ children }: { children: React.ReactElement }) {
  const { state } = useAuth();
  if (state === "restoring") return <FullScreenLoading />;
  if (state === "guest") return <Navigate to="/login" replace />;
  return children;
}

function RedirectIfAuthed({ children }: { children: React.ReactElement }) {
  const { state } = useAuth();
  if (state === "restoring") return <FullScreenLoading />;
  if (state === "authed") return <Navigate to="/" replace />;
  return children;
}

const router = createBrowserRouter([
  {
    path: "/login",
    element: (
      <RedirectIfAuthed>
        <LoginPage />
      </RedirectIfAuthed>
    ),
  },
  {
    element: (
      <RequireAuth>
        <AppLayout />
      </RequireAuth>
    ),
    children: [
      { index: true, element: <Deferred><DashboardPage /></Deferred> },
      { path: "/accounts", element: <Deferred><AccountsPage /></Deferred> },
      { path: "/models", element: <Deferred><ModelsPage /></Deferred> },
      { path: "/audits", element: <Deferred><AuditsPage /></Deferred> },
    ],
  },
  { path: "*", element: <Navigate to="/" replace /> },
]);

export function App() {
  const [queryClient] = useState(
    () =>
      new QueryClient({
        defaultOptions: {
          queries: { retry: 1, staleTime: 15_000, refetchOnWindowFocus: false },
        },
      }),
  );

  return (
    <ThemeProvider attribute="class" defaultTheme="system" enableSystem disableTransitionOnChange storageKey="grokcli2api:theme">
      <QueryClientProvider client={queryClient}>
        <AuthProvider>
          <RouterProvider router={router} />
          <Toaster richColors closeButton position="top-right" />
        </AuthProvider>
      </QueryClientProvider>
    </ThemeProvider>
  );
}
