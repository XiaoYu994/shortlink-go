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
  tone?: 'indigo' | 'sky' | 'emerald';
}

const BAR_TONES = {
  indigo: 'bg-indigo-100/75',
  sky: 'bg-sky-100/80',
  emerald: 'bg-emerald-100/75',
} as const;

/*
  Dub.co 标志性的「行内占比背景条」组件：
  进度条直接铺在每一行文字背后，左图标名称、右等宽数字与占比百分比，
  在紧凑空间内同时传达绝对值与相对份额。
*/
export function BreakdownBarList({
  items,
  emptyText = '该时间范围内暂无数据。',
  tone = 'indigo',
}: BreakdownBarListProps) {
  const total = items.reduce((sum, item) => sum + item.count, 0);
  const max = Math.max(...items.map((item) => item.count), 1);

  if (items.length === 0 || total === 0) {
    return <p className="py-6 text-center text-sm text-zinc-400">{emptyText}</p>;
  }

  return (
    <ul className="space-y-1.5">
      {items.map((item) => {
        const Icon = item.icon;
        const barPct = item.count > 0 ? Math.max(4, Math.round((item.count / max) * 100)) : 0;
        const sharePct = total > 0 ? Math.round((item.count / total) * 100) : 0;
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
            <span className="relative z-10 flex min-w-0 items-center gap-2 font-medium text-zinc-800">
              {Icon && <Icon className="size-3.5 shrink-0 text-zinc-500" />}
              <span className="truncate font-mono text-xs">{item.label}</span>
            </span>
            <span className="relative z-10 flex shrink-0 items-center gap-2 font-mono text-xs tabular-nums">
              <span className="font-semibold text-zinc-900">{formatNumber(item.count)}</span>
              <span className="w-9 text-right text-zinc-400">{sharePct}%</span>
            </span>
          </li>
        );
      })}
    </ul>
  );
}
