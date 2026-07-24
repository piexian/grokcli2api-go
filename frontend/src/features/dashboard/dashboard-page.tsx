import { useQuery } from "@tanstack/react-query";
import { RefreshCw } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Area, Bar, CartesianGrid, ComposedChart, XAxis, YAxis } from "recharts";

import { Button } from "@/components/ui/button";
import { ChartContainer, ChartTooltip, ChartTooltipContent, type ChartConfig } from "@/components/ui/chart";
import { getDashboard, type DashboardPeriod } from "@/features/dashboard/dashboard-api";
import { DashboardPanel } from "@/features/dashboard/dashboard-panel";
import { EmptyState, ErrorState } from "@/shared/components/data-state";
import { PageHeader } from "@/shared/components/page-header";
import { PeriodSelector } from "@/shared/components/period-selector";
import { formatNumber } from "@/shared/lib/format";
import { toPeriodValue, type PeriodDays } from "@/shared/lib/period";

function StatCard({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <div className="rounded-lg bg-card p-4">
      <p className="text-xs text-muted-foreground">{label}</p>
      <p className="mt-2 text-2xl font-medium tabular-nums">{value}</p>
      {hint ? <p className="mt-1 text-xs text-muted-foreground">{hint}</p> : null}
    </div>
  );
}

export function DashboardPage() {
  const { t } = useTranslation();
  const [periodDays, setPeriodDays] = useState<PeriodDays>(1);
  const period: DashboardPeriod = toPeriodValue(periodDays);

  const query = useQuery({
    queryKey: ["dashboard", period],
    queryFn: () => getDashboard(period),
    placeholderData: (previous) => previous,
    staleTime: 15_000,
    refetchInterval: 60_000,
  });

  const dashboard = query.data;
  const chartConfig: ChartConfig = {
    requests: { label: t("dashboard.trendRequests"), theme: { light: "oklch(0.68 0.15 245)", dark: "oklch(0.74 0.13 245)" } },
    tokens: { label: t("dashboard.trendTokens"), theme: { light: "oklch(0.7 0.11 160)", dark: "oklch(0.73 0.1 160)" } },
  };
  const chartData = (dashboard?.series ?? []).map((point) => ({
    bucket: point.bucket.length > 10 ? point.bucket.slice(5, 16) : point.bucket,
    requests: point.requests,
    tokens: point.totalTokens,
  }));

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={t("dashboard.title")}
        description={t("dashboard.description")}
        actions={
          <div className="flex items-center gap-2">
            <PeriodSelector value={periodDays} onChange={setPeriodDays} ariaLabel={t("dashboard.period")} />
            <Button variant="outline" size="sm" onClick={() => void query.refetch()} disabled={query.isFetching}>
              <RefreshCw className={query.isFetching ? "size-4 animate-spin" : "size-4"} />
              {t("common.refresh")}
            </Button>
          </div>
        }
      />

      {query.isError ? (
        <ErrorState message={t("dashboard.loadFailed")} onRetry={() => void query.refetch()} />
      ) : (
        <>
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
            <StatCard label={t("dashboard.resources.totalAccounts")} value={formatNumber(dashboard?.resources.totalAccounts ?? 0)} />
            <StatCard label={t("dashboard.resources.usableAccounts")} value={formatNumber(dashboard?.resources.usableAccounts ?? 0)} />
            <StatCard label={t("dashboard.resources.coolingAccounts")} value={formatNumber(dashboard?.resources.coolingAccounts ?? 0)} />
            <StatCard label={t("dashboard.resources.disabledAccounts")} value={formatNumber(dashboard?.resources.disabledAccounts ?? 0)} />
            <StatCard label={t("dashboard.resources.paidAccounts")} value={formatNumber(dashboard?.resources.paidAccounts ?? 0)} />
            <StatCard label={t("dashboard.resources.totalModels")} value={formatNumber(dashboard?.resources.totalModels ?? 0)} />
          </div>

          <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
            <StatCard label={t("dashboard.requests")} value={formatNumber(dashboard?.usage.requests ?? 0)} hint={t("dashboard.successRate", { rate: ((dashboard?.usage.successRate ?? 0) * 100).toFixed(1) })} />
            <StatCard label={t("dashboard.inputTokens")} value={formatNumber(dashboard?.usage.inputTokens ?? 0)} />
            <StatCard label={t("dashboard.outputTokens")} value={formatNumber(dashboard?.usage.outputTokens ?? 0)} />
            <StatCard label={t("dashboard.tokens")} value={formatNumber(dashboard?.usage.totalTokens ?? 0)} />
          </div>

          <DashboardPanel id="dashboard-trend" title={t("dashboard.trend")}>
            {chartData.length === 0 ? (
              <EmptyState message={t("dashboard.noTrendData")} />
            ) : (
              <ChartContainer config={chartConfig} className="h-64 w-full">
                <ComposedChart data={chartData} margin={{ left: 8, right: 8, top: 8 }}>
                  <CartesianGrid vertical={false} />
                  <XAxis dataKey="bucket" tickLine={false} axisLine={false} fontSize={11} />
                  <YAxis yAxisId="left" tickLine={false} axisLine={false} fontSize={11} width={48} />
                  <YAxis yAxisId="right" orientation="right" tickLine={false} axisLine={false} fontSize={11} width={48} />
                  <ChartTooltip content={<ChartTooltipContent />} />
                  <Bar yAxisId="left" dataKey="requests" fill="var(--color-requests)" radius={[2, 2, 0, 0]} />
                  <Area yAxisId="right" dataKey="tokens" stroke="var(--color-tokens)" fill="var(--color-tokens)" fillOpacity={0.15} type="monotone" />
                </ComposedChart>
              </ChartContainer>
            )}
          </DashboardPanel>

          <DashboardPanel id="dashboard-top-models" title={t("dashboard.topModels")}>
            {(dashboard?.byModel ?? []).length === 0 ? (
              <EmptyState message={t("dashboard.noTopModels")} />
            ) : (
              <div className="space-y-2">
                {dashboard!.byModel.slice(0, 10).map((model) => {
                  const max = dashboard!.byModel[0]?.requests || 1;
                  return (
                    <div key={model.model} className="flex items-center gap-3">
                      <span className="w-48 truncate font-mono text-xs">{model.model}</span>
                      <div className="h-2 min-w-0 flex-1 rounded bg-secondary">
                        <div className="h-2 rounded bg-primary" style={{ width: `${Math.max(2, (model.requests / max) * 100)}%` }} />
                      </div>
                      <span className="w-20 text-right text-xs tabular-nums">{formatNumber(model.requests)}</span>
                    </div>
                  );
                })}
              </div>
            )}
          </DashboardPanel>
        </>
      )}
    </div>
  );
}
