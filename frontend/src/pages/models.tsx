import { Search, X } from "lucide-react";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useQuery } from "@tanstack/react-query";

import { Input } from "@/components/ui/input";
import { Table, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { EmptyHint, ErrorHint, SkeletonRows, VirtualRows } from "@/components/data";
import { PageHeader } from "@/components/layout";
import { apiRequest } from "@/lib/api";
import { formatNumber } from "@/lib/format";

type ModelSummary = {
  model: string;
  accounts: number;
  usableAccounts: number;
  statusCounts: Record<string, number>;
};

async function fetchModelsSummary(q?: string): Promise<ModelSummary[]> {
  const params = q ? `?q=${encodeURIComponent(q)}` : "";
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const raw = await apiRequest<any>(`/v1/admin/models/summary${params}`);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  return (raw.data ?? []).map((item: any) => ({
    model: item.model ?? "",
    accounts: item.accounts ?? 0,
    usableAccounts: item.usable_accounts ?? 0,
    statusCounts: item.status_counts ?? {},
  }));
}

function useDebounced<T>(value: T, delay: number): T {
  const [debounced, setDebounced] = useState(value);
  useMemo(() => {
    const timer = setTimeout(() => setDebounced(value), delay);
    return () => clearTimeout(timer);
  }, [value, delay]);
  return debounced;
}

export function ModelsPage() {
  const { t } = useTranslation();
  const [search, setSearch] = useState("");
  const debouncedSearch = useDebounced(search, 300);

  const query = useQuery({
    queryKey: ["models-summary", debouncedSearch],
    queryFn: () => fetchModelsSummary(debouncedSearch.trim() || undefined),
    refetchInterval: 60_000,
    placeholderData: (previous) => previous,
  });

  const models = useMemo(() => query.data ?? [], [query.data]);

  const colSpan = 4;

  return (
    <>
      <PageHeader title={t("models.title")} description={t("models.description")} />

      <div className="mb-4 flex items-center gap-3">
        <div className="relative w-full max-w-sm">
          <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            className="pl-9 pr-8"
            placeholder={t("models.searchPlaceholder")}
            value={search}
            onChange={(event) => setSearch(event.target.value)}
          />
          {search ? (
            <button
              type="button"
              className="absolute right-2.5 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
              onClick={() => setSearch("")}
              aria-label={t("common.clear")}
            >
              <X className="size-3.5" />
            </button>
          ) : null}
        </div>
        {query.data ? (
          <span className="text-[13px] text-muted-foreground">{t("models.total", { count: formatNumber(models.length) })}</span>
        ) : null}
      </div>

      {query.isError ? (
        <ErrorHint message={t("models.loadFailed")} onRetry={() => void query.refetch()} />
      ) : (
        <div className="rounded-lg border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("models.colModel")}</TableHead>
                <TableHead>{t("models.colAccounts")}</TableHead>
                <TableHead>{t("models.colUsable")}</TableHead>
                <TableHead>{t("models.colStatus")}</TableHead>
              </TableRow>
            </TableHeader>
            {query.isLoading ? (
              <SkeletonRows colSpan={colSpan} />
            ) : models.length === 0 ? (
              <tbody>
                <TableRow>
                  <TableCell colSpan={colSpan}>
                    <EmptyHint />
                  </TableCell>
                </TableRow>
              </tbody>
            ) : (
              <VirtualRows
                items={models}
                colSpan={colSpan}
                rowHeight={45}
                renderRow={(model) => (
                  <TableRow key={model.model}>
                    <TableCell>
                      <span className="font-mono text-[13px]">{model.model}</span>
                    </TableCell>
                    <TableCell>
                      <span className="text-[13px] tabular-nums">{formatNumber(model.accounts)}</span>
                    </TableCell>
                    <TableCell>
                      <span className="text-[13px] tabular-nums">{formatNumber(model.usableAccounts)}</span>
                    </TableCell>
                    <TableCell>
                      <span className="text-xs text-muted-foreground">
                        {Object.entries(model.statusCounts)
                          .map(([status, count]) => `${t(`accounts.status.${status}`, { defaultValue: status })} ${formatNumber(count)}`)
                          .join(" · ") || "—"}
                      </span>
                    </TableCell>
                  </TableRow>
                )}
              />
            )}
          </Table>
        </div>
      )}
    </>
  );
}
