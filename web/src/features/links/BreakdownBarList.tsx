import type { LucideIcon } from 'lucide-react';
import { cn } from '@/lib/cn';
import { formatNumber } from '@/lib/format';

export interface BreakdownItem {
  key: string;
  label: string;
  count: number;
  icon?: LucideIcon;
}

interface BreakdownBarListProps {
  items: BreakdownItem[];
  emptyText?: string;
  tone?: 'mint' | 'peach' | 'yellow';
}

const BAR_TONES = {
  mint: 'bg-hl-mint/65',
  peach: 'bg-hl-peach/55',
  yellow: 'bg-hl-yellow/70',
} as const;

/*
  荧光笔行内占比条：
  保留高可读性的文字与右侧等宽数字，自动收起 0 点击项避免空行稀释重点。
*/
export function BreakdownBarList({
  items,
  emptyText = '该时间范围内暂无数据。',
  tone = 'mint',
}: BreakdownBarListProps) {
  const nonZero = items.filter((item) => item.count > 0);
  const hiddenCount = items.length - nonZero.length;
  const total = nonZero.reduce((sum, item) => sum + item.count, 0);
  const max = Math.max(...nonZero.map((item) => item.count), 1);

  if (nonZero.length === 0 || total === 0) {
    return <p className="py-6 text-center text-sm text-muted-foreground">{emptyText}</p>;
  }

  return (
    <div className="space-y-2">
      <ul className="space-y-1.5">
        {nonZero.map((item) => {
          const Icon = item.icon;
          const barPct = Math.max(5, Math.round((item.count / max) * 100));
          const sharePct = Math.round((item.count / total) * 100);
          return (
            <li
              key={item.key}
              className="relative flex items-center justify-between gap-3 overflow-hidden rounded-lg px-3 py-2 text-sm"
            >
              <div
                aria-hidden="true"
                className={cn(
                  'pointer-events-none absolute inset-y-0 left-0 rounded-lg transition-all',
                  BAR_TONES[tone],
                )}
                style={{ width: `${barPct}%` }}
              />
              <span className="relative z-10 flex min-w-0 items-center gap-2 font-medium text-ink">
                {Icon && <Icon className="size-3.5 shrink-0 text-ink-secondary" />}
                <span className="truncate text-xs font-medium">{item.label}</span>
              </span>
              <span className="relative z-10 flex shrink-0 items-center gap-2 font-mono text-xs tabular-nums">
                <span className="font-bold text-ink">{formatNumber(item.count)}</span>
                <span className="w-10 text-right text-muted-foreground">{sharePct}%</span>
              </span>
            </li>
          );
        })}
      </ul>
      {hiddenCount > 0 && (
        <p className="px-1 text-[11px] text-muted-foreground">
          另有 {hiddenCount} 个分类暂无点击记录
        </p>
      )}
    </div>
  );
}
