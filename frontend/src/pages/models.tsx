import { Search, X } from "lucide-react";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { Input } from "@/components/ui/input";
import { Table, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { EmptyHint, ErrorHint, SkeletonRows, VirtualRows } from "@/components/data";
import { PageHeader } from "@/components/layout";
import { formatNumber } from "@/lib/format";
import { useQuery } from "@tanstack/react-query";

// 模型目录从凭证页服务端数据聚合。后端 models/summary API（task-05）就绪后切换。
type ModelRow = { id: string; accounts: number; usable: number };

export function ModelsPage() {
  const { t } = useTranslation();
  const [search, setSearch] = useState("");

  const query = useQuery<{ data: ModelRow[] }>({
    queryKey: ["models-summary"],
    queryFn: async () => {
      const { fetchCredentialPage } = await import("@/lib/api");
      const byModel = new Map<string, ModelRow>();
      let cursor = "";
      // 模型数通常很少（个位数），但需要遍历全量凭证聚合；
      // 等后端 /v1/admin/models/summary 就绪后替换为单次调用。
      for (;;) {
        const page = await fetchCredentialPage({ cursor, limit: 500 });
        for (const credential of page.items) {
          for (const model of credential.models) {
            const row = byModel.get(model) ?? { id: model, accounts: 0, usable: 0 };
            row.accounts += 1;
            if (credential.usable) row.usable += 1;
            byModel.set(model, row);
          }
        }
        if (!page.hasMore) break;
        cursor = page.nextCursor;
      }
      return { data: [...byModel.values()].sort((a, b) => b.usable - a.usable || a.id.localeCompare(b.id)) };
    },
    refetchInterval: 60_000,
    placeholderData: (previous) => previous,
  });

  const models = useMemo(() => query.data?.data ?? [], [query.data]);

  const filtered = useMemo(() => {
    const needle = search.trim().toLowerCase();
    if (!needle) return models;
    return models.filter((model) => model.id.toLowerCase().includes(needle));
  }, [models, search]);

  const colSpan = 3;

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
              </TableRow>
            </TableHeader>
            {query.isLoading ? (
              <SkeletonRows colSpan={colSpan} />
            ) : filtered.length === 0 ? (
              <tbody>
                <TableRow>
                  <TableCell colSpan={colSpan}>
                    <EmptyHint />
                  </TableCell>
                </TableRow>
              </tbody>
            ) : (
              <VirtualRows
                items={filtered}
                colSpan={colSpan}
                rowHeight={45}
                renderRow={(model) => (
                  <TableRow key={model.id}>
                    <TableCell>
                      <span className="font-mono text-[13px]">{model.id}</span>
                    </TableCell>
                    <TableCell>
                      <span className="text-[13px] tabular-nums">{formatNumber(model.accounts)}</span>
                    </TableCell>
                    <TableCell>
                      <span className="text-[13px] tabular-nums">{formatNumber(model.usable)}</span>
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
