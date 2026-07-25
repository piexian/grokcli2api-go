import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { RefreshCw, Search, X } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { EmptyHint, ErrorHint, SkeletonRows } from "@/components/data";
import { PageHeader } from "@/components/layout";
import { fetchAuditHealth, fetchAudits } from "@/lib/api";
import { formatDateTime, formatDuration, formatNumber } from "@/lib/format";

const PAGE_SIZE = 50;

function statusVariant(code: number): "default" | "secondary" | "destructive" {
  if (code >= 200 && code < 300) return "default";
  if (code >= 500) return "destructive";
  return "secondary";
}

export function AuditsPage() {
  const { t } = useTranslation();
  const [model, setModel] = useState("");
  const [cursors, setCursors] = useState<string[]>([""]);
  const [page, setPage] = useState(0);

  const cursor = cursors[page] ?? "";

  const query = useQuery({
    queryKey: ["audits", cursor, model],
    queryFn: () => fetchAudits({ cursor: cursor || undefined, model: model || undefined, limit: PAGE_SIZE }),
    refetchInterval: 30_000,
    placeholderData: keepPreviousData,
  });

  const health = useQuery({
    queryKey: ["audit-health"],
    queryFn: fetchAuditHealth,
    refetchInterval: 30_000,
  });

  const data = query.data;

  function goNext() {
    if (!data?.nextCursor) return;
    setCursors((current) => {
      const next = current.slice(0, page + 1);
      next[page + 1] = data.nextCursor;
      return next;
    });
    setPage((current) => current + 1);
  }

  const colSpan = 8;

  return (
    <>
      <PageHeader
        title={t("audits.title")}
        description={t("audits.description")}
        actions={
          health.data ? (
            <span className="text-xs text-muted-foreground">
              {t("audits.health", {
                size: formatNumber(health.data.queueSize),
                cap: formatNumber(health.data.queueCap),
                dropped: formatNumber(health.data.droppedTotal),
              })}
            </span>
          ) : undefined
        }
      />

      <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
        <div className="relative w-full max-w-sm">
          <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            className="pl-9 pr-8"
            placeholder={t("audits.filterPlaceholder")}
            value={model}
            onChange={(event) => {
              setModel(event.target.value);
              setCursors([""]);
              setPage(0);
            }}
          />
          {model ? (
            <button
              type="button"
              className="absolute right-2.5 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
              onClick={() => {
                setModel("");
                setCursors([""]);
                setPage(0);
              }}
              aria-label={t("common.clear")}
            >
              <X className="size-3.5" />
            </button>
          ) : null}
        </div>
        <Button variant="outline" size="sm" onClick={() => void query.refetch()} disabled={query.isFetching}>
          <RefreshCw className={query.isFetching ? "size-4 animate-spin" : "size-4"} />
          {t("common.refresh")}
        </Button>
      </div>

      {query.isError ? (
        <ErrorHint message={t("audits.loadFailed")} onRetry={() => void query.refetch()} />
      ) : (
        <>
          <div className="rounded-lg border">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("audits.colTime")}</TableHead>
                  <TableHead>{t("audits.colProtocol")}</TableHead>
                  <TableHead>{t("audits.colModel")}</TableHead>
                  <TableHead>{t("audits.colStatus")}</TableHead>
                  <TableHead>{t("audits.colTokens")}</TableHead>
                  <TableHead>{t("audits.colDuration")}</TableHead>
                  <TableHead>{t("audits.colAttempts")}</TableHead>
                  <TableHead>{t("audits.colAccount")}</TableHead>
                </TableRow>
              </TableHeader>
              {query.isLoading ? (
                <SkeletonRows colSpan={colSpan} />
              ) : (data?.items.length ?? 0) === 0 ? (
                <TableBody>
                  <TableRow>
                    <TableCell colSpan={colSpan}>
                      <EmptyHint />
                    </TableCell>
                  </TableRow>
                </TableBody>
              ) : (
                <TableBody>
                  {data!.items.map((audit) => (
                    <TableRow key={audit.id}>
                      <TableCell>
                        <span className="text-[13px] tabular-nums">{formatDateTime(audit.createdAt)}</span>
                      </TableCell>
                      <TableCell>
                        <span className="text-[13px]">{audit.protocol}</span>
                        {audit.streaming ? (
                          <span className="ml-1 text-xs text-muted-foreground">{t("audits.streaming")}</span>
                        ) : null}
                      </TableCell>
                      <TableCell>
                        <span className="font-mono text-[13px]">{audit.model || "—"}</span>
                      </TableCell>
                      <TableCell>
                        <Badge variant={statusVariant(audit.statusCode)}>{audit.statusCode || "—"}</Badge>
                      </TableCell>
                      <TableCell>
                        <span className="text-[13px] tabular-nums">
                          {formatNumber(audit.inputTokens)} / {formatNumber(audit.outputTokens)}
                        </span>
                      </TableCell>
                      <TableCell>
                        <span className="text-[13px] tabular-nums">{formatDuration(audit.durationMs)}</span>
                      </TableCell>
                      <TableCell>
                        <span className="text-[13px] tabular-nums">{audit.attemptCount}</span>
                      </TableCell>
                      <TableCell>
                        <span className="font-mono text-xs">{audit.accountId ?? "—"}</span>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              )}
            </Table>
          </div>

          <div className="mt-4 flex items-center justify-between">
            <span className="text-[13px] text-muted-foreground">{t("audits.page", { page: page + 1 })}</span>
            <div className="flex gap-2">
              <Button variant="outline" size="sm" onClick={() => setPage((p) => Math.max(0, p - 1))} disabled={page === 0}>
                {t("audits.prev")}
              </Button>
              <Button variant="outline" size="sm" onClick={goNext} disabled={!data?.hasMore}>
                {t("audits.next")}
              </Button>
            </div>
          </div>
        </>
      )}
    </>
  );
}
