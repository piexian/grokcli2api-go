import { useQuery } from "@tanstack/react-query";
import { RefreshCw, Search } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { getRequestAudits, type AuditDTO } from "@/features/audits/request-audits-api";
import { EmptyState, ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { CursorPagination } from "@/shared/components/pagination";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { formatNumber } from "@/shared/lib/format";

const pageSize = 50;

function formatTimestamp(ms: number, locale: string): string {
  if (!ms) return "—";
  return new Intl.DateTimeFormat(locale, { dateStyle: "short", timeStyle: "medium" }).format(new Date(ms));
}

export function RequestAuditsPage() {
  const { t, i18n } = useTranslation();
  const [search, setSearch] = useState("");
  const debouncedSearch = useDebouncedValue(search, 300);
  const [cursors, setCursors] = useState<string[]>([""]);
  const [page, setPage] = useState(0);

  const cursor = cursors[page] ?? "";
  const query = useQuery({
    queryKey: ["request-audits", cursor, debouncedSearch],
    queryFn: () =>
      getRequestAudits({
        cursor: cursor || undefined,
        limit: pageSize,
        model: debouncedSearch || undefined,
      }),
    placeholderData: (previous) => previous,
    refetchInterval: 30_000,
  });

  const data = query.data;

  function goNext(): void {
    if (!data?.nextCursor) return;
    setCursors((current) => {
      const next = current.slice(0, page + 1);
      next[page + 1] = data.nextCursor;
      return next;
    });
    setPage((current) => current + 1);
  }

  function goPrevious(): void {
    setPage((current) => Math.max(0, current - 1));
  }

  function goFirst(): void {
    setCursors([""]);
    setPage(0);
  }

  function statusVariant(code: number): "default" | "secondary" | "destructive" {
    if (code >= 200 && code < 300) return "default";
    if (code >= 500) return "destructive";
    return "secondary";
  }

  return (
    <DataTableShell
      toolbar={
        <>
          <div className="flex min-w-0 flex-1 items-center gap-3">
            <div className="relative w-full max-w-sm">
              <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
              <Input
                className="h-9 pl-9"
                placeholder={t("audits.search")}
                value={search}
                onChange={(event) => {
                  setSearch(event.target.value);
                  goFirst();
                }}
              />
            </div>
          </div>
          <Button variant="outline" size="sm" onClick={() => void query.refetch()} disabled={query.isFetching}>
            <RefreshCw className={query.isFetching ? "size-4 animate-spin" : "size-4"} />
            {t("common.refresh")}
          </Button>
        </>
      }
      footer={
        <CursorPagination
          page={page + 1}
          pageSize={pageSize}
          hasMore={data?.hasMore ?? false}
          onFirstPage={goFirst}
          onPreviousPage={goPrevious}
          onNextPage={goNext}
          onPageSizeChange={() => undefined}
        />
      }
    >
      {query.isError ? (
        <ErrorState message={t("audits.loadFailed")} onRetry={() => void query.refetch()} />
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t("audits.time")}</TableHead>
              <TableHead>{t("audits.protocol")}</TableHead>
              <TableHead>{t("audits.model")}</TableHead>
              <TableHead>{t("audits.status")}</TableHead>
              <TableHead>{t("audits.tokens")}</TableHead>
              <TableHead>{t("audits.duration")}</TableHead>
              <TableHead>{t("audits.account")}</TableHead>
            </TableRow>
          </TableHeader>
          {query.isLoading ? (
            <TableBody>
              <TableLoadingRow colSpan={7} />
            </TableBody>
          ) : (data?.items.length ?? 0) === 0 ? (
            <TableBody>
              <TableRow>
                <TableCell colSpan={7}>
                  <EmptyState />
                </TableCell>
              </TableRow>
            </TableBody>
          ) : (
            <TableBody>
              {data!.items.map((audit: AuditDTO) => (
                <TableRow key={audit.id}>
                  <TableCell>
                    <span className="text-xs tabular-nums">{formatTimestamp(audit.createdAt, i18n.language)}</span>
                  </TableCell>
                  <TableCell>
                    <span className="text-xs">{audit.protocol}</span>
                    {audit.streaming ? <span className="ml-1 text-xs text-muted-foreground">SSE</span> : null}
                  </TableCell>
                  <TableCell>
                    <span className="font-mono text-xs">{audit.model || "—"}</span>
                  </TableCell>
                  <TableCell>
                    <Badge variant={statusVariant(audit.statusCode)}>{audit.statusCode || "—"}</Badge>
                    {audit.errorCode ? <span className="ml-1 text-xs text-destructive">{audit.errorCode}</span> : null}
                  </TableCell>
                  <TableCell>
                    <span className="text-xs tabular-nums">
                      {formatNumber(audit.inputTokens)} / {formatNumber(audit.outputTokens)}
                    </span>
                  </TableCell>
                  <TableCell>
                    <span className="text-xs tabular-nums">{formatNumber(audit.durationMs)} ms</span>
                  </TableCell>
                  <TableCell>
                    <span className="font-mono text-xs">{audit.accountId ?? "—"}</span>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          )}
        </Table>
      )}
    </DataTableShell>
  );
}
