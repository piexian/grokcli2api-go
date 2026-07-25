import { useQuery } from "@tanstack/react-query";
import { RefreshCw } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Area, AreaChart, CartesianGrid, XAxis, YAxis } from "recharts";

import { Button } from "@/components/ui/button";
import { ChartContainer, ChartTooltip, ChartTooltipContent, type ChartConfig } from "@/components/ui/chart";
import { ErrorHint, EmptyHint } from "@/components/data";
import { PageHeader } from "@/components/layout";
import { fetchDashboard } from "@/lib/api";
import { formatCompact, formatNumber, formatPercent } from "@/lib/format";
import { cn } from "@/lib/cn";

const PERIODS = [
  { value: "today", key: "dashboard.today" },
  { value: "7d", key: "dashboard.d7" },
  { value: "30d", key: "dashboard.d30" },
] as const;

function Stat({ label, value, hint, className }: { label: string; value: string; hint?: string; className?: string }) {
  return (
    <div className={cn("rounded-lg border bg-card p-4", className)}>
      <p className="text-xs text-muted-foreground">{label}</p>
      <p className="mt-1.5 text-xl font-semibold tabular-nums tracking-tight">{value}</p>
      {hint ? <p className="mt-0.5 text-xs text-muted-foreground">{hint}</p> : null}
    </div>
  );
}

export function DashboardPage() {
  const { t } = useTranslation();
  const [period, setPeriod] = useState<string>("today");

  const query = useQuery({
    queryKey: ["dashboard", period],
    queryFn: () => fetchDashboard(period),
    refetchInterval: 60_000,
    placeholderData: (previous) => previous,
  });

  const data = query.data;

  const chartConfig: ChartConfig = {
    requests: { label: t("dashboard.requests"), color: "oklch(0.55 0.14 250)" },
  };

  const chartData = (data?.series ?? []).map((point) => ({
    bucket: point.bucket.length > 10 ? point.bucket.slice(5, 16) : point.bucket,
    requests: point.requests,
  }));

  const topModels = (data?.byModel ?? []).slice(0, 8);
  const maxRequests = topModels[0]?.requests || 1;

  return (
    <>
      <PageHeader
        title={t("dashboard.title")}
        description={t("dashboard.description")}
        actions={
          <>
            <div className="flex rounded-md border">
              {PERIODS.map((item) => (
                <button
                  key={item.value}
                  type="button"
                  className={cn(
                    "h-8 px-3 text-[13px] text-muted-foreground first:rounded-l-md last:rounded-r-md hover:text-foreground",
                    period === item.value && "bg-accent font-medium text-foreground",
                  )}
                  onClick={() => setPeriod(item.value)}
                >
                  {t(item.key)}
                </button>
              ))}
            </div>
            <Button variant="outline" size="sm" onClick={() => void query.refetch()} disabled={query.isFetching}>
              <RefreshCw className={query.isFetching ? "size-4 animate-spin" : "size-4"} />
            </Button>
          </>
        }
      />

      {query.isError ? (
        <ErrorHint message={t("dashboard.loadFailed")} onRetry={() => void query.refetch()} />
      ) : (
        <div className="space-y-4">
          <div className="grid grid-cols-2 gap-3 md:grid-cols-5">
            <Stat label={t("dashboard.accounts")} value={formatNumber(data?.resources.totalAccounts ?? 0)} />
            <Stat label={t("dashboard.usableAccounts")} value={formatNumber(data?.resources.usableAccounts ?? 0)} />
            <Stat label={t("dashboard.disabledAccounts")} value={formatNumber(data?.resources.disabledAccounts ?? 0)} />
            <Stat label={t("dashboard.paidAccounts")} value={formatNumber(data?.resources.paidAccounts ?? 0)} />
            <Stat label={t("dashboard.models")} value={formatNumber(data?.resources.totalModels ?? 0)} />
          </div>

          <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
            <Stat
              label={t("dashboard.requests")}
              value={formatNumber(data?.usage.requests ?? 0)}
              hint={`${t("dashboard.successRate")} ${formatPercent(data?.usage.successRate ?? 0)}`}
            />
            <Stat label={t("dashboard.inputTokens")} value={formatCompact(data?.usage.inputTokens ?? 0)} />
            <Stat label={t("dashboard.outputTokens")} value={formatCompact(data?.usage.outputTokens ?? 0)} />
            <Stat label={t("dashboard.totalTokens")} value={formatCompact(data?.usage.totalTokens ?? 0)} />
          </div>

          <div className="grid gap-4 lg:grid-cols-2">
            <section className="rounded-lg border bg-card p-4">
              <h2 className="mb-3 text-[13px] font-medium">{t("dashboard.trend")}</h2>
              {chartData.length === 0 ? (
                <EmptyHint message={t("dashboard.noData")} />
              ) : (
                <ChartContainer config={chartConfig} className="h-52 w-full">
                  <AreaChart data={chartData} margin={{ left: 4, right: 4, top: 4 }}>
                    <CartesianGrid vertical={false} strokeDasharray="3 3" />
                    <XAxis dataKey="bucket" tickLine={false} axisLine={false} fontSize={11} />
                    <YAxis tickLine={false} axisLine={false} fontSize={11} width={40} />
                    <ChartTooltip content={<ChartTooltipContent />} />
                    <Area
                      dataKey="requests"
                      type="monotone"
                      stroke="var(--color-requests)"
                      fill="var(--color-requests)"
                      fillOpacity={0.12}
                      strokeWidth={1.5}
                    />
                  </AreaChart>
                </ChartContainer>
              )}
            </section>

            <section className="rounded-lg border bg-card p-4">
              <h2 className="mb-3 text-[13px] font-medium">{t("dashboard.topModels")}</h2>
              {topModels.length === 0 ? (
                <EmptyHint message={t("dashboard.noData")} />
              ) : (
                <div className="space-y-2.5">
                  {topModels.map((model) => (
                    <div key={model.model} className="flex items-center gap-3">
                      <span className="w-36 truncate font-mono text-xs">{model.model}</span>
                      <div className="h-1.5 min-w-0 flex-1 rounded-full bg-muted">
                        <div
                          className="h-1.5 rounded-full bg-primary"
                          style={{ width: `${Math.max(2, (model.requests / maxRequests) * 100)}%` }}
                        />
                      </div>
                      <span className="w-16 text-right text-xs tabular-nums">{formatNumber(model.requests)}</span>
                    </div>
                  ))}
                </div>
              )}
            </section>
          </div>
        </div>
      )}
    </>
  );
}
