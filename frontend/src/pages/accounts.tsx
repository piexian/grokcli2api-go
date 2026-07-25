import { experimental_streamedQuery, keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { RefreshCw, Search, Trash2, Upload, X } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Table, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { EmptyHint, ErrorHint, SkeletonRows, VirtualRows } from "@/components/data";
import { PageHeader } from "@/components/layout";
import { deleteCredential, streamCredentials, uploadCredential } from "@/lib/api";
import { formatDateTime, formatNumber } from "@/lib/format";
import type { Credential } from "@/lib/types";

const QUERY_KEY = ["credentials"] as const;

/** 预计算小写搜索索引：一次构建，避免每次击键对上万项重复 toLowerCase。 */
type Indexed = Credential & { __search: string };

function buildIndex(items: Credential[]): Indexed[] {
  return items.map((item) => ({
    ...item,
    __search: [item.id, item.scope ?? "", item.subscriptionTierDisplay ?? "", ...item.models]
      .join("\n")
      .toLowerCase(),
  }));
}

function StatusBadge({ status }: { status: string }) {
  const { t } = useTranslation();
  const tone =
    status === "ready"
      ? "bg-emerald-500"
      : status === "cooling_down" || status === "pending_models"
        ? "bg-amber-500"
        : "bg-red-500";
  const label = t(`accounts.status.${status}`, { defaultValue: status });
  return (
    <span className="inline-flex items-center gap-1.5 text-[13px]">
      <span className={`size-1.5 rounded-full ${tone}`} aria-hidden="true" />
      {label}
    </span>
  );
}

function QuotaCell({ credential }: { credential: Credential }) {
  const { t } = useTranslation();
  const billing = credential.billing;
  if (!billing) return <span className="text-[13px] text-muted-foreground">{t("accounts.quotaNone")}</span>;
  if (billing.exhausted) return <Badge variant="destructive">{t("accounts.quotaExhausted")}</Badge>;
  if (billing.usagePercent !== undefined) {
    const left = Math.max(0, 100 - billing.usagePercent);
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="text-[13px] tabular-nums">{t("accounts.quotaLeft", { percent: left.toFixed(0) })}</span>
        </TooltipTrigger>
        <TooltipContent>{formatDateTime(billing.updatedAt)}</TooltipContent>
      </Tooltip>
    );
  }
  return <span className="text-[13px] text-muted-foreground">—</span>;
}

function useDebounced<T>(value: T, delay: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setDebounced(value), delay);
    return () => clearTimeout(timer);
  }, [value, delay]);
  return debounced;
}

export function AccountsPage() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [search, setSearch] = useState("");
  const debouncedSearch = useDebounced(search, 300);
  const [uploadOpen, setUploadOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<Credential | null>(null);
  const [paste, setPaste] = useState("");
  const fileRef = useRef<HTMLInputElement>(null);

  const query = useQuery({
    queryKey: QUERY_KEY,
    // 流式：第一页到达即渲染，后台拉完剩余页（1 万+ 账号首屏秒开）。
    queryFn: experimental_streamedQuery<Credential[], Credential[]>({
      streamFn: ({ signal }) => streamCredentials(signal),
      reducer: (_acc, chunk) => chunk,
      initialValue: [],
    }),
    refetchInterval: 60_000,
    placeholderData: keepPreviousData,
  });

  const indexed = useMemo(() => buildIndex(query.data ?? []), [query.data]);

  const filtered = useMemo(() => {
    const needle = debouncedSearch.trim().toLowerCase();
    if (!needle) return indexed;
    return indexed.filter((item) => item.__search.includes(needle));
  }, [indexed, debouncedSearch]);

  const usableCount = useMemo(
    () => (query.data ?? []).reduce((acc, item) => acc + (item.usable ? 1 : 0), 0),
    [query.data],
  );

  const uploadMutation = useMutation({
    mutationFn: (content: string | File) => uploadCredential(content),
    onSuccess: (result) => {
      toast.success(t("accounts.uploaded", { status: result.modelDiscovery }));
      setUploadOpen(false);
      setPaste("");
      void queryClient.invalidateQueries({ queryKey: QUERY_KEY });
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("accounts.uploadFailed")),
  });

  const deleteMutation = useMutation({
    mutationFn: (id: string) => deleteCredential(id),
    // 乐观更新：先移除，失败回滚，避免全量 refetch 等待。
    onMutate: async (id) => {
      setDeleteTarget(null);
      await queryClient.cancelQueries({ queryKey: QUERY_KEY });
      const previous = queryClient.getQueryData<Credential[]>(QUERY_KEY);
      queryClient.setQueryData<Credential[]>(QUERY_KEY, (old) => old?.filter((item) => item.id !== id));
      return { previous };
    },
    onSuccess: () => toast.success(t("accounts.deleted")),
    onError: (error, _id, context) => {
      if (context?.previous) queryClient.setQueryData(QUERY_KEY, context.previous);
      toast.error(error instanceof Error ? error.message : t("errors.generic"));
    },
    onSettled: () => void queryClient.invalidateQueries({ queryKey: QUERY_KEY }),
  });

  const searching = debouncedSearch.trim().length > 0;
  const colSpan = 7;

  return (
    <>
      <PageHeader title={t("accounts.title")} description={t("accounts.description")} />

      <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
        <div className="flex min-w-0 flex-1 items-center gap-3">
          <div className="relative w-full max-w-sm">
            <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              className="pl-9 pr-8"
              placeholder={t("accounts.searchPlaceholder")}
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
            <span className="shrink-0 text-[13px] text-muted-foreground">
              {t("accounts.total", { count: formatNumber(query.data.length) })}
              {" · "}
              {t("accounts.usable", { count: formatNumber(usableCount) })}
              {searching ? ` · ${formatNumber(filtered.length)}` : ""}
            </span>
          ) : null}
        </div>
        <div className="flex items-center gap-2">
          <Button variant="outline" size="sm" onClick={() => void query.refetch()} disabled={query.isFetching}>
            <RefreshCw className={query.isFetching ? "size-4 animate-spin" : "size-4"} />
            {t("common.refresh")}
          </Button>
          <Button size="sm" onClick={() => setUploadOpen(true)}>
            <Upload className="size-4" />
            {t("accounts.upload")}
          </Button>
        </div>
      </div>

      {query.isError ? (
        <ErrorHint message={t("accounts.loadFailed")} onRetry={() => void query.refetch()} />
      ) : (
        <div className="rounded-lg border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("accounts.colId")}</TableHead>
                <TableHead>{t("accounts.colStatus")}</TableHead>
                <TableHead>{t("accounts.colTier")}</TableHead>
                <TableHead>{t("accounts.colModels")}</TableHead>
                <TableHead>{t("accounts.colQuota")}</TableHead>
                <TableHead>{t("accounts.colExpiry")}</TableHead>
                <TableHead className="w-12" />
              </TableRow>
            </TableHeader>
            {query.isLoading ? (
              <SkeletonRows colSpan={colSpan} />
            ) : filtered.length === 0 ? (
              <tbody>
                <TableRow>
                  <TableCell colSpan={colSpan}>
                    <EmptyHint message={searching ? t("accounts.noResult") : t("accounts.noData")} />
                  </TableCell>
                </TableRow>
              </tbody>
            ) : (
              <VirtualRows
                items={filtered}
                colSpan={colSpan}
                rowHeight={45}
                renderRow={(item) => (
                  <TableRow key={item.id}>
                    <TableCell>
                      <span className="font-mono text-[13px]">{item.id}</span>
                    </TableCell>
                    <TableCell>
                      <StatusBadge status={item.status} />
                    </TableCell>
                    <TableCell>
                      <span className="text-[13px]">{item.subscriptionTierDisplay ?? "—"}</span>
                    </TableCell>
                    <TableCell>
                      <span className="text-[13px] tabular-nums">{formatNumber(item.models.length)}</span>
                    </TableCell>
                    <TableCell>
                      <QuotaCell credential={item} />
                    </TableCell>
                    <TableCell>
                      <span className="text-[13px] text-muted-foreground">
                        {item.status === "cooling_down" && item.cooldownUntil
                          ? formatDateTime(item.cooldownUntil)
                          : formatDateTime(item.expiresAt)}
                      </span>
                    </TableCell>
                    <TableCell>
                      <Button
                        variant="ghost"
                        size="icon"
                        className="size-8 text-muted-foreground hover:text-destructive"
                        onClick={() => setDeleteTarget(item)}
                      >
                        <Trash2 className="size-4" />
                      </Button>
                    </TableCell>
                  </TableRow>
                )}
              />
            )}
          </Table>
        </div>
      )}

      <Dialog open={uploadOpen} onOpenChange={setUploadOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("accounts.uploadTitle")}</DialogTitle>
            <DialogDescription>{t("accounts.uploadHint")}</DialogDescription>
          </DialogHeader>
          <Textarea
            className="min-h-36 font-mono text-xs"
            placeholder='{"access_token":"…","refresh_token":"…"}'
            value={paste}
            onChange={(event) => setPaste(event.target.value)}
          />
          <input
            ref={fileRef}
            type="file"
            accept="application/json,.json"
            className="hidden"
            onChange={(event) => {
              const file = event.target.files?.[0];
              if (file) uploadMutation.mutate(file);
              event.target.value = "";
            }}
          />
          <DialogFooter>
            <Button variant="outline" onClick={() => fileRef.current?.click()} disabled={uploadMutation.isPending}>
              {t("accounts.uploadFile")}
            </Button>
            <Button onClick={() => uploadMutation.mutate(paste)} disabled={uploadMutation.isPending || !paste.trim()}>
              {uploadMutation.isPending ? t("accounts.uploading") : t("accounts.upload")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <AlertDialog open={deleteTarget !== null} onOpenChange={(open) => !open && setDeleteTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("accounts.deleteTitle")}</AlertDialogTitle>
            <AlertDialogDescription>
              {t("accounts.deleteHint")}
              {deleteTarget ? <span className="mt-2 block font-mono text-xs">{deleteTarget.id}</span> : null}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleteMutation.isPending}>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
              onClick={() => deleteTarget && deleteMutation.mutate(deleteTarget.id)}
              disabled={deleteMutation.isPending}
            >
              {deleteMutation.isPending ? t("common.deleting") : t("common.confirm")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
