import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import { Search } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { EmptyState, ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { formatNumber } from "@/shared/lib/format";
import { listCredentials } from "@/features/accounts/accounts-api";

type ModelRow = {
  id: string;
  accountCount: number;
  usableAccountCount: number;
};

// 模型目录从凭证列表聚合（grokcli2api-go 的 /v1/models 走 API key 鉴权，
// 管理台使用 admin key，因此基于 /v1/admin/credentials 的 models 字段聚合）。
export function ModelsPage() {
  const { t } = useTranslation();
  const [search, setSearch] = useState("");
  const debouncedSearch = useDebouncedValue(search, 300);

  const query = useQuery({ queryKey: ["credentials"], queryFn: listCredentials, refetchInterval: 60_000, placeholderData: keepPreviousData });

  const models = useMemo<ModelRow[]>(() => {
    const byModel = new Map<string, ModelRow>();
    for (const credential of query.data ?? []) {
      for (const model of credential.models) {
        const row = byModel.get(model) ?? { id: model, accountCount: 0, usableAccountCount: 0 };
        row.accountCount += 1;
        if (credential.usable) row.usableAccountCount += 1;
        byModel.set(model, row);
      }
    }
    return [...byModel.values()].sort((a, b) => b.usableAccountCount - a.usableAccountCount || a.id.localeCompare(b.id));
  }, [query.data]);

  const filtered = useMemo(() => {
    const needle = debouncedSearch.trim().toLowerCase();
    if (!needle) return models;
    return models.filter((model) => model.id.toLowerCase().includes(needle));
  }, [models, debouncedSearch]);

  return (
    <DataTableShell
      toolbar={
        <div className="flex min-w-0 flex-1 items-center gap-3">
          <div className="relative w-full max-w-sm">
            <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input className="h-9 pl-9" placeholder={t("models.search")} value={search} onChange={(event) => setSearch(event.target.value)} />
          </div>
          {query.data ? (
            <span className="shrink-0 text-xs text-muted-foreground">{t("models.total", { count: formatNumber(models.length) })}</span>
          ) : null}
        </div>
      }
    >
      {query.isError ? (
        <ErrorState message={t("models.loadFailed")} onRetry={() => void query.refetch()} />
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t("models.model")}</TableHead>
              <TableHead>{t("models.accounts")}</TableHead>
              <TableHead>{t("models.usableAccounts")}</TableHead>
            </TableRow>
          </TableHeader>
          {query.isLoading ? (
            <TableBody>
              <TableLoadingRow colSpan={3} />
            </TableBody>
          ) : filtered.length === 0 ? (
            <TableBody>
              <TableRow>
                <TableCell colSpan={3}>
                  <EmptyState />
                </TableCell>
              </TableRow>
            </TableBody>
          ) : (
            <VirtualTableBody
              items={filtered}
              colSpan={3}
              rowHeight={44}
              renderRow={(model) => (
                <TableRow key={model.id}>
                  <TableCell>
                    <span className="font-mono text-xs">{model.id}</span>
                  </TableCell>
                  <TableCell>
                    <span className="text-xs tabular-nums">{formatNumber(model.accountCount)}</span>
                  </TableCell>
                  <TableCell>
                    <span className="text-xs tabular-nums">{formatNumber(model.usableAccountCount)}</span>
                  </TableCell>
                </TableRow>
              )}
            />
          )}
        </Table>
      )}
    </DataTableShell>
  );
}
