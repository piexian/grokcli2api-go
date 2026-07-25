import { AlertCircle, Inbox } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { TableCell, TableRow } from "@/components/ui/table";
import { cn } from "@/shared/lib/cn";

export function LoadingState({ className }: { className?: string }) {
  return (
    <div className={cn("flex min-h-44 items-center justify-center", className)}>
      <Spinner className="size-5" />
    </div>
  );
}

export function TableLoadingRow({ colSpan }: { colSpan: number }) {
  // 骨架行：与真实行等高，避免 1 万账号首屏长时间空白感知。
  return (
    <TableRow className="hover:bg-transparent">
      {Array.from({ length: colSpan }).map((_, i) => (
        <TableCell key={i}>
          <div className="h-4 w-3/4 animate-pulse rounded bg-muted" />
        </TableCell>
      ))}
    </TableRow>
  );
}

export function EmptyState({ message }: { message?: string }) {
  const { t } = useTranslation();
  return (
    <div className="flex min-h-44 flex-col items-center justify-center gap-2 text-center text-muted-foreground">
      <Inbox className="size-7 stroke-1" />
      <p className="text-sm">{message ?? t("common.noData")}</p>
    </div>
  );
}

export function ErrorState({ message, onRetry }: { message: string; onRetry: () => void }) {
  const { t } = useTranslation();
  return (
    <div className="flex min-h-44 flex-col items-center justify-center gap-3 text-center">
      <AlertCircle className="size-7 text-destructive" />
      <p className="max-w-md text-sm text-muted-foreground">{message}</p>
      <Button type="button" variant="secondary" size="sm" onClick={onRetry}>{t("common.retry")}</Button>
    </div>
  );
}
