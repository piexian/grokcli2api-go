import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";

import { TableBody, TableCell, TableRow } from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { AlertCircle, Inbox } from "lucide-react";
import { Button } from "@/components/ui/button";
import { useTranslation } from "react-i18next";

const OVERSCAN = 12;
const MIN_VIRTUAL = 30;

type VirtualRowsProps<T> = {
  items: readonly T[];
  colSpan: number;
  rowHeight: number;
  renderRow: (item: T, index: number) => ReactNode;
};

/**
 * 窗口化表格体：只挂载可视区附近的行，1 万+ 行也保持恒定 DOM 规模。
 * 依赖页面滚动，通过 spacer 行撑开总高度。
 */
export function VirtualRows<T>({ items, colSpan, rowHeight, renderRow }: VirtualRowsProps<T>) {
  const bodyRef = useRef<HTMLTableSectionElement>(null);
  const [range, setRange] = useState({ start: 0, end: Math.min(items.length, 60) });
  const enabled = items.length > MIN_VIRTUAL;

  const update = useCallback(() => {
    if (!enabled || !bodyRef.current) {
      setRange({ start: 0, end: items.length });
      return;
    }
    const rect = bodyRef.current.getBoundingClientRect();
    const viewportTop = 0;
    const viewportBottom = window.innerHeight;
    const top = Math.max(0, viewportTop - rect.top);
    const bottom = Math.min(rect.height, viewportBottom - rect.top);
    setRange({
      start: Math.max(0, Math.floor(top / rowHeight) - OVERSCAN),
      end: Math.min(items.length, Math.ceil(Math.max(top, bottom) / rowHeight) + OVERSCAN),
    });
  }, [enabled, items.length, rowHeight]);

  useEffect(() => {
    update();
    let frame = 0;
    const onScroll = () => {
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(update);
    };
    window.addEventListener("scroll", onScroll, { passive: true });
    window.addEventListener("resize", onScroll);
    return () => {
      cancelAnimationFrame(frame);
      window.removeEventListener("scroll", onScroll);
      window.removeEventListener("resize", onScroll);
    };
  }, [update]);

  if (!enabled) {
    return <TableBody>{items.map((item, i) => renderRow(item, i))}</TableBody>;
  }

  const topPad = range.start * rowHeight;
  const bottomPad = (items.length - range.end) * rowHeight;

  return (
    <TableBody ref={bodyRef}>
      {topPad > 0 ? (
        <TableRow aria-hidden="true" className="hover:bg-transparent">
          <TableCell colSpan={colSpan} style={{ height: topPad, padding: 0, border: 0 }} />
        </TableRow>
      ) : null}
      {items.slice(range.start, range.end).map((item, i) => renderRow(item, range.start + i))}
      {bottomPad > 0 ? (
        <TableRow aria-hidden="true" className="hover:bg-transparent">
          <TableCell colSpan={colSpan} style={{ height: bottomPad, padding: 0, border: 0 }} />
        </TableRow>
      ) : null}
    </TableBody>
  );
}

export function SkeletonRows({ colSpan, rows = 8 }: { colSpan: number; rows?: number }) {
  return (
    <TableBody>
      {Array.from({ length: rows }).map((_, i) => (
        <TableRow key={i} className="hover:bg-transparent">
          {Array.from({ length: colSpan }).map((__, j) => (
            <TableCell key={j}>
              <Skeleton className="h-4 w-3/4" />
            </TableCell>
          ))}
        </TableRow>
      ))}
    </TableBody>
  );
}

export function EmptyHint({ message }: { message?: string }) {
  const { t } = useTranslation();
  return (
    <div className="flex min-h-40 flex-col items-center justify-center gap-2 text-muted-foreground">
      <Inbox className="size-6 stroke-1" />
      <p className="text-[13px]">{message ?? t("common.empty")}</p>
    </div>
  );
}

export function ErrorHint({ message, onRetry }: { message: string; onRetry: () => void }) {
  const { t } = useTranslation();
  return (
    <div className="flex min-h-40 flex-col items-center justify-center gap-3">
      <AlertCircle className="size-6 text-destructive" />
      <p className="text-[13px] text-muted-foreground">{message}</p>
      <Button variant="secondary" size="sm" onClick={onRetry}>
        {t("common.retry")}
      </Button>
    </div>
  );
}
