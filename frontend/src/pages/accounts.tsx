import { keepPreviousData, useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { ArrowDown, ArrowUp, ArrowUpDown, RefreshCw, Search, Trash2, Upload, X } from "lucide-react";
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Table, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { EmptyHint, ErrorHint, SkeletonRows } from "@/components/data";
import { PageHeader } from "@/components/layout";
import { deleteCredential, fetchCredentialPage, splitCredentialPayloads, uploadCredentialsBatch, type CredentialQuery, type UploadItemResult } from "@/lib/api";
import { formatDateTime, formatNumber } from "@/lib/format";
import type { Credential } from "@/lib/types";

const QUERY_KEY = ["credentials"] as const;
const PAGE_SIZE = 100;

type SortField = NonNullable<CredentialQuery["sort"]>;
type SortOrder = NonNullable<CredentialQuery["order"]>;

const STATUS_OPTIONS = ["ready", "cooling_down", "disabled", "needs_refresh", "pending_models"] as const;

const SORTABLE_COLUMNS: Array<{ field: SortField; labelKey: string }> = [
  { field: "id", labelKey: "accounts.colId" },
  { field: "status", labelKey: "accounts.colStatus" },
  { field: "tier", labelKey: "accounts.colTier" },
  { field: "models_count", labelKey: "accounts.colModels" },
];

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
    <span className="inline-flex items-center gap-1.5 text-[13px] whitespace-nowrap">
      <span className={`size-1.5 rounded-full ${tone}`} aria-hidden="true" />
      {label}
    </span>
  );
}

function centsToUsd(cents: number): string {
  return `$${(cents / 100).toFixed(2)}`;
}

function QuotaCell({ credential }: { credential: Credential }) {
  const { t } = useTranslation();
  const billing = credential.billing;
  if (!billing) return <span className="text-[13px] text-muted-foreground">{t("accounts.quotaNone")}</span>;
  if (billing.exhausted) return <Badge variant="destructive">{t("accounts.quotaExhausted")}</Badge>;

  const parts: string[] = [];
  if (billing.usagePercent !== undefined) {
    parts.push(t("accounts.quotaLeft", { percent: Math.max(0, 100 - billing.usagePercent).toFixed(0) }));
  }
  const amounts: string[] = [];
  if (billing.onDemandRemainingCents) amounts.push(centsToUsd(billing.onDemandRemainingCents));
  if (billing.prepaidBalanceCents) amounts.push(centsToUsd(billing.prepaidBalanceCents));

  const text = parts.length > 0 ? parts.join(" ") : "—";
  const tooltip = [text, ...amounts, billing.periodEnd ? formatDateTime(billing.periodEnd) : ""]
    .filter(Boolean)
    .join(" · ");

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className="text-[13px] tabular-nums whitespace-nowrap">{text}</span>
      </TooltipTrigger>
      <TooltipContent>{tooltip}</TooltipContent>
    </Tooltip>
  );
}

function useDebounced<T>(value: T, delay: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setDebounced(value), delay);
    return () => clearTimeout(timer);
  }, [value, delay]);
  return debounced;
}

/** 无限滚动哨兵：进入视口时触发加载下一页。 */
function useLoadMore(ref: React.RefObject<HTMLElement | null>, enabled: boolean, onLoadMore: () => void) {
  useEffect(() => {
    const el = ref.current;
    if (!el || !enabled) return;
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries.some((entry) => entry.isIntersecting)) onLoadMore();
      },
      { rootMargin: "400px" },
    );
    observer.observe(el);
    return () => observer.disconnect();
  }, [ref, enabled, onLoadMore]);
}

export function AccountsPage() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [search, setSearch] = useState("");
  const debouncedSearch = useDebounced(search, 300);
  const [status, setStatus] = useState<string>("all");
  const [usable, setUsable] = useState<string>("all");
  const [sort, setSort] = useState<SortField>("id");
  const [order, setOrder] = useState<SortOrder>("asc");
  const [uploadOpen, setUploadOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<Credential | null>(null);
  const [paste, setPaste] = useState("");
  const [uploadProgress, setUploadProgress] = useState<{ done: number; total: number } | null>(null);
  const [uploadResults, setUploadResults] = useState<UploadItemResult[] | null>(null);
  const fileRef = useRef<HTMLInputElement>(null);
  const sentinelRef = useRef<HTMLTableRowElement>(null);

  const filters = useMemo<CredentialQuery>(
    () => ({
      q: debouncedSearch.trim() || undefined,
      status: status === "all" ? undefined : status,
      usable: usable === "all" ? undefined : usable === "usable",
      sort,
      order,
    }),
    [debouncedSearch, status, usable, sort, order],
  );

  const query = useInfiniteQuery({
    queryKey: [...QUERY_KEY, filters],
    queryFn: ({ pageParam }) => fetchCredentialPage({ ...filters, cursor: pageParam, limit: PAGE_SIZE }),
    initialPageParam: "",
    getNextPageParam: (lastPage) => (lastPage.hasMore ? lastPage.nextCursor : undefined),
    placeholderData: keepPreviousData,
  });

  const items = useMemo(() => query.data?.pages.flatMap((page) => page.items) ?? [], [query.data]);
  const total = query.data?.pages[0]?.total ?? 0;

  const loadMore = () => {
    if (query.hasNextPage && !query.isFetchingNextPage) void query.fetchNextPage();
  };
  useLoadMore(sentinelRef, Boolean(query.hasNextPage), loadMore);

  const uploadMutation = useMutation({
    mutationFn: (items: Array<{ name: string; content: string | File }>) =>
      uploadCredentialsBatch(items, (done, total) => setUploadProgress({ done, total })),
    onSuccess: (results) => {
      const succeeded = results.filter((r) => r.ok).length;
      const failed = results.length - succeeded;
      if (failed === 0) {
        toast.success(t("accounts.batchUploaded", { count: succeeded }));
        setUploadOpen(false);
        setPaste("");
        setUploadResults(null);
      } else {
        toast.warning(t("accounts.batchPartial", { succeeded, failed }));
        setUploadResults(results);
      }
      setUploadProgress(null);
      void queryClient.invalidateQueries({ queryKey: QUERY_KEY });
    },
    onError: (error) => {
      setUploadProgress(null);
      toast.error(error instanceof Error ? error.message : t("accounts.uploadFailed"));
    },
  });

  function submitUpload() {
    const items: Array<{ name: string; content: string }> = splitCredentialPayloads(paste).map((content, i) => ({
      name: t("accounts.pasteItem", { index: i + 1 }),
      content,
    }));
    if (items.length === 0) {
      toast.error(t("accounts.uploadParseFailed"));
      return;
    }
    setUploadResults(null);
    uploadMutation.mutate(items);
  }

  function submitFiles(files: FileList) {
    const items = [...files].map((file) => ({ name: file.name, content: file }));
    if (items.length === 0) return;
    setUploadResults(null);
    uploadMutation.mutate(items);
  }

  const deleteMutation = useMutation({
    mutationFn: (id: string) => deleteCredential(id),
    onMutate: async (id) => {
      setDeleteTarget(null);
      await queryClient.cancelQueries({ queryKey: QUERY_KEY });
      queryClient.setQueriesData<{ pages: Array<{ items: Credential[] }>; pageParams: string[] }>(
        { queryKey: QUERY_KEY },
        (old) => old ? { ...old, pages: old.pages.map((p) => ({ ...p, items: p.items.filter((c) => c.id !== id) })) } : old,
      );
    },
    onSuccess: () => toast.success(t("accounts.deleted")),
    onError: (error) => {
      toast.error(error instanceof Error ? error.message : t("errors.generic"));
      void queryClient.invalidateQueries({ queryKey: QUERY_KEY });
    },
    onSettled: () => void queryClient.invalidateQueries({ queryKey: QUERY_KEY }),
  });

  function toggleSort(field: SortField) {
    if (sort === field) setOrder(order === "asc" ? "desc" : "asc");
    else {
      setSort(field);
      setOrder("asc");
    }
  }

  function sortIcon(field: SortField) {
    if (sort !== field) return <ArrowUpDown className="size-3 text-muted-foreground/50" />;
    return order === "asc" ? <ArrowUp className="size-3" /> : <ArrowDown className="size-3" />;
  }

  const colSpan = 8;
  const searching = debouncedSearch.trim().length > 0 || status !== "all" || usable !== "all";

  return (
    <>
      <PageHeader title={t("accounts.title")} description={t("accounts.description")} />

      <div className="mb-4 flex flex-wrap items-center gap-3">
        <div className="relative w-full max-w-xs">
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

        <Select value={status} onValueChange={setStatus}>
          <SelectTrigger className="w-32">
            <SelectValue placeholder={t("accounts.filterStatus")} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">{t("accounts.filterAll")}</SelectItem>
            {STATUS_OPTIONS.map((value) => (
              <SelectItem key={value} value={value}>
                {t(`accounts.status.${value}`)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>

        <Select value={usable} onValueChange={setUsable}>
          <SelectTrigger className="w-28">
            <SelectValue placeholder={t("accounts.filterUsable")} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">{t("accounts.filterAll")}</SelectItem>
            <SelectItem value="usable">{t("accounts.filterUsableOnly")}</SelectItem>
            <SelectItem value="unusable">{t("accounts.filterUnusableOnly")}</SelectItem>
          </SelectContent>
        </Select>

        <span className="text-[13px] text-muted-foreground">
          {formatNumber(items.length)} / {formatNumber(total)}
        </span>

        <div className="ml-auto flex items-center gap-2">
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
                {SORTABLE_COLUMNS.map(({ field, labelKey }) => (
                  <TableHead key={field}>
                    <button
                      type="button"
                      className="inline-flex items-center gap-1 hover:text-foreground"
                      onClick={() => toggleSort(field)}
                    >
                      {t(labelKey)}
                      {sortIcon(field)}
                    </button>
                  </TableHead>
                ))}
                <TableHead>{t("accounts.colQuota")}</TableHead>
                <TableHead>{t("accounts.colExpiry")}</TableHead>
                <TableHead>{t("accounts.colCooldown")}</TableHead>
                <TableHead className="w-12" />
              </TableRow>
            </TableHeader>
            {query.isLoading ? (
              <SkeletonRows colSpan={colSpan} />
            ) : items.length === 0 ? (
              <tbody>
                <TableRow>
                  <TableCell colSpan={colSpan}>
                    <EmptyHint message={searching ? t("accounts.noResult") : t("accounts.noData")} />
                  </TableCell>
                </TableRow>
              </tbody>
            ) : (
              <tbody>
                {items.map((item) => (
                  <TableRow key={item.id}>
                    <TableCell>
                      <span className="font-mono text-[13px]">{item.id}</span>
                    </TableCell>
                    <TableCell>
                      <StatusBadge status={item.status} />
                    </TableCell>
                    <TableCell>
                      <span className="text-[13px] whitespace-nowrap">{item.subscriptionTierDisplay ?? "—"}</span>
                    </TableCell>
                    <TableCell>
                      <span className="text-[13px] tabular-nums">{formatNumber(item.models.length)}</span>
                    </TableCell>
                    <TableCell>
                      <QuotaCell credential={item} />
                    </TableCell>
                    <TableCell>
                      <span className="text-[13px] text-muted-foreground whitespace-nowrap">{formatDateTime(item.expiresAt)}</span>
                    </TableCell>
                    <TableCell>
                      <span className="text-[13px] text-muted-foreground whitespace-nowrap">
                        {item.status === "cooling_down" && item.cooldownUntil ? formatDateTime(item.cooldownUntil) : "—"}
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
                ))}
                {/* 无限滚动哨兵 */}
                <TableRow ref={sentinelRef} className="hover:bg-transparent">
                  <TableCell colSpan={colSpan} className="h-10 text-center">
                    {query.isFetchingNextPage ? (
                      <span className="text-xs text-muted-foreground">{t("common.loading")}</span>
                    ) : query.hasNextPage ? null : items.length > 0 ? (
                      <span className="text-xs text-muted-foreground">{formatNumber(items.length)} / {formatNumber(total)}</span>
                    ) : null}
                  </TableCell>
                </TableRow>
              </tbody>
            )}
          </Table>
        </div>
      )}

      <Dialog
        open={uploadOpen}
        onOpenChange={(open) => {
          if (uploadMutation.isPending) return;
          setUploadOpen(open);
          if (!open) {
            setPaste("");
            setUploadResults(null);
            setUploadProgress(null);
          }
        }}
      >
        <DialogContent className="max-w-lg">
          <DialogHeader>
            <DialogTitle>{t("accounts.uploadTitle")}</DialogTitle>
            <DialogDescription>{t("accounts.uploadHint")}</DialogDescription>
          </DialogHeader>
          <Textarea
            className="min-h-36 font-mono text-xs"
            placeholder={t("accounts.uploadPlaceholder")}
            value={paste}
            onChange={(event) => setPaste(event.target.value)}
            disabled={uploadMutation.isPending}
          />
          <input
            ref={fileRef}
            type="file"
            accept="application/json,.json"
            multiple
            className="hidden"
            onChange={(event) => {
              const files = event.target.files;
              if (files && files.length > 0) submitFiles(files);
              event.target.value = "";
            }}
          />
          {uploadProgress ? (
            <p className="text-[13px] text-muted-foreground">
              {t("accounts.uploadProgress", { done: uploadProgress.done, total: uploadProgress.total })}
            </p>
          ) : null}
          {uploadResults ? (
            <div className="max-h-40 overflow-y-auto rounded-md border p-2 text-xs">
              {uploadResults.map((result, index) => (
                <div key={`${result.name}-${index}`} className={result.ok ? "text-emerald-600" : "text-destructive"}>
                  {result.ok
                    ? t("accounts.uploadItemOk", { name: result.name })
                    : t("accounts.uploadItemFail", { name: result.name, error: result.error ?? "" })}
                </div>
              ))}
            </div>
          ) : null}
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => fileRef.current?.click()}
              disabled={uploadMutation.isPending}
            >
              {t("accounts.uploadFiles")}
            </Button>
            <Button onClick={submitUpload} disabled={uploadMutation.isPending || !paste.trim()}>
              {uploadMutation.isPending
                ? t("accounts.uploading")
                : t("accounts.upload")}
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
