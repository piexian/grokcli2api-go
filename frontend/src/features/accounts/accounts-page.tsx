import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { FileUp, RefreshCw, Search, Trash2, Upload } from "lucide-react";
import { useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { ApiError } from "@/shared/api/client";
import { EmptyState, ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { formatDateTime, formatNumber } from "@/shared/lib/format";
import {
  deleteCredential,
  listCredentials,
  uploadCredentialFile,
  uploadCredentialJson,
  type CredentialDTO,
} from "@/features/accounts/accounts-api";

const queryKey = ["credentials"] as const;

function statusVariant(status: string): "default" | "secondary" | "destructive" | "outline" {
  switch (status) {
    case "ready":
      return "default";
    case "cooling_down":
    case "pending_models":
      return "secondary";
    case "disabled":
    case "needs_refresh":
      return "destructive";
    default:
      return "outline";
  }
}

function BillingCell({ credential }: { credential: CredentialDTO }) {
  const { t } = useTranslation();
  const billing = credential.billing;
  if (!billing) {
    return <span className="text-xs text-muted-foreground">{t("credentials.noBilling")}</span>;
  }
  if (billing.exhausted) {
    return <Badge variant="destructive">{t("credentials.billingExhausted")}</Badge>;
  }
  if (billing.usagePercent !== undefined) {
    const remaining = Math.max(0, 100 - billing.usagePercent);
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span className="text-xs tabular-nums">{t("credentials.billingRemaining", { percent: remaining.toFixed(0) })}</span>
        </TooltipTrigger>
        <TooltipContent>{formatDateTime(billing.updatedAt)}</TooltipContent>
      </Tooltip>
    );
  }
  return <span className="text-xs text-muted-foreground">—</span>;
}

export function AccountsPage() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [search, setSearch] = useState("");
  const debouncedSearch = useDebouncedValue(search, 300);
  const [uploadOpen, setUploadOpen] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<CredentialDTO | null>(null);
  const [pasteValue, setPasteValue] = useState("");
  const fileInputRef = useRef<HTMLInputElement>(null);

  const query = useQuery({
    queryKey,
    queryFn: listCredentials,
    refetchInterval: 60_000,
  });

  const uploadMutation = useMutation({
    mutationFn: async (input: { json?: string; file?: File }) => {
      if (input.file) return uploadCredentialFile(input.file);
      return uploadCredentialJson(input.json ?? "");
    },
    onSuccess: (result) => {
      toast.success(t("credentials.uploaded", { status: result.modelDiscovery }));
      setUploadOpen(false);
      setPasteValue("");
      void queryClient.invalidateQueries({ queryKey });
    },
    onError: (error) => {
      toast.error(error instanceof ApiError ? error.message : t("credentials.uploadFailed"));
    },
  });

  const deleteMutation = useMutation({
    mutationFn: (id: string) => deleteCredential(id),
    onSuccess: () => {
      toast.success(t("credentials.deleted"));
      setDeleteTarget(null);
      void queryClient.invalidateQueries({ queryKey });
    },
    onError: (error) => {
      toast.error(error instanceof ApiError ? error.message : t("errors.generic"));
    },
  });

  const filtered = useMemo(() => {
    const items = query.data ?? [];
    const needle = debouncedSearch.trim().toLowerCase();
    if (!needle) return items;
    return items.filter((item) => {
      if (item.id.toLowerCase().includes(needle)) return true;
      if (item.scope?.toLowerCase().includes(needle)) return true;
      if (item.subscriptionTierDisplay?.toLowerCase().includes(needle)) return true;
      return item.models.some((model) => model.toLowerCase().includes(needle));
    });
  }, [query.data, debouncedSearch]);

  const usableCount = useMemo(() => (query.data ?? []).filter((item) => item.usable).length, [query.data]);

  function statusLabel(status: string): string {
    switch (status) {
      case "ready":
        return t("credentials.statusReady");
      case "cooling_down":
        return t("credentials.statusCooling");
      case "disabled":
        return t("credentials.statusDisabled");
      case "needs_refresh":
        return t("credentials.statusNeedsRefresh");
      case "pending_models":
        return t("credentials.statusPendingModels");
      default:
        return status;
    }
  }

  return (
    <>
      <DataTableShell
        toolbar={
          <>
            <div className="flex min-w-0 flex-1 items-center gap-3">
              <div className="relative w-full max-w-sm">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input
                  className="h-9 pl-9"
                  placeholder={t("credentials.search")}
                  value={search}
                  onChange={(event) => setSearch(event.target.value)}
                />
              </div>
              {query.data ? (
                <span className="shrink-0 text-xs text-muted-foreground">
                  {t("credentials.total", { count: formatNumber(query.data.length) })}
                  {" · "}
                  {t("credentials.usableCount", { count: formatNumber(usableCount) })}
                </span>
              ) : null}
            </div>
            <div className="flex items-center gap-2">
              <Button variant="outline" size="sm" onClick={() => void query.refetch()} disabled={query.isFetching}>
                <RefreshCw className={query.isFetching ? "size-4 animate-spin" : "size-4"} />
                {t("credentials.refresh")}
              </Button>
              <Button size="sm" onClick={() => setUploadOpen(true)}>
                <Upload className="size-4" />
                {t("credentials.upload")}
              </Button>
            </div>
          </>
        }
      >
        {query.isError ? (
          <ErrorState message={t("credentials.loadFailed")} onRetry={() => void query.refetch()} />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t("credentials.account")}</TableHead>
                <TableHead>{t("credentials.status")}</TableHead>
                <TableHead>{t("credentials.tier")}</TableHead>
                <TableHead>{t("credentials.models")}</TableHead>
                <TableHead>{t("credentials.billing")}</TableHead>
                <TableHead>{t("credentials.expiry")}</TableHead>
                <TableHead className="w-12 text-right">{t("credentials.actions")}</TableHead>
              </TableRow>
            </TableHeader>
            {query.isLoading ? (
              <TableBody>
                <TableLoadingRow colSpan={7} />
              </TableBody>
            ) : filtered.length === 0 ? (
              <TableBody>
                <TableRow>
                  <TableCell colSpan={7}>
                    <EmptyState />
                  </TableCell>
                </TableRow>
              </TableBody>
            ) : (
              <VirtualTableBody
                items={filtered}
                colSpan={7}
                rowHeight={44}
                renderRow={(item) => (
                  <TableRow key={item.id}>
                    <TableCell>
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <span className="font-mono text-xs">{item.id}</span>
                        </TooltipTrigger>
                        <TooltipContent>{item.scope ?? item.authMode ?? item.id}</TooltipContent>
                      </Tooltip>
                    </TableCell>
                    <TableCell>
                      <Badge variant={statusVariant(item.status)}>{statusLabel(item.status)}</Badge>
                    </TableCell>
                    <TableCell>
                      {item.subscriptionTierDisplay ? (
                        <span className="text-xs">{item.subscriptionTierDisplay}</span>
                      ) : (
                        <span className="text-xs text-muted-foreground">—</span>
                      )}
                    </TableCell>
                    <TableCell>
                      <span className="text-xs tabular-nums">{formatNumber(item.models.length)}</span>
                    </TableCell>
                    <TableCell>
                      <BillingCell credential={item} />
                    </TableCell>
                    <TableCell>
                      <span className="text-xs text-muted-foreground">
                        {item.cooldownUntil && new Date(item.cooldownUntil) > new Date()
                          ? formatDateTime(item.cooldownUntil)
                          : item.expiresAt
                            ? formatDateTime(item.expiresAt)
                            : "—"}
                      </span>
                    </TableCell>
                    <TableCell className="text-right">
                      <Button variant="ghost" size="icon" className="size-8 text-muted-foreground hover:text-destructive" onClick={() => setDeleteTarget(item)}>
                        <Trash2 className="size-4" />
                      </Button>
                    </TableCell>
                  </TableRow>
                )}
              />
            )}
          </Table>
        )}
      </DataTableShell>

      <Dialog open={uploadOpen} onOpenChange={setUploadOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("credentials.uploadTitle")}</DialogTitle>
            <DialogDescription>{t("credentials.uploadDescription")}</DialogDescription>
          </DialogHeader>
          <Textarea
            className="min-h-40 font-mono text-xs"
            placeholder='{"access_token":"…","refresh_token":"…"}'
            value={pasteValue}
            onChange={(event) => setPasteValue(event.target.value)}
          />
          <input
            ref={fileInputRef}
            type="file"
            accept="application/json,.json"
            className="hidden"
            onChange={(event) => {
              const file = event.target.files?.[0];
              if (file) uploadMutation.mutate({ file });
              event.target.value = "";
            }}
          />
          <DialogFooter>
            <Button variant="outline" onClick={() => fileInputRef.current?.click()} disabled={uploadMutation.isPending}>
              <FileUp className="size-4" />
              {t("credentials.uploadFile")}
            </Button>
            <Button onClick={() => uploadMutation.mutate({ json: pasteValue })} disabled={uploadMutation.isPending || !pasteValue.trim()}>
              {uploadMutation.isPending ? t("credentials.uploading") : t("credentials.upload")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <AlertDialog open={deleteTarget !== null} onOpenChange={(open) => !open && setDeleteTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("credentials.deleteTitle")}</AlertDialogTitle>
            <AlertDialogDescription>
              {t("credentials.deleteDescription")}
              {deleteTarget ? <span className="mt-2 block font-mono text-xs">{deleteTarget.id}</span> : null}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
              onClick={() => deleteTarget && deleteMutation.mutate(deleteTarget.id)}
              disabled={deleteMutation.isPending}
            >
              {t("common.confirm")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
