import type { ReactNode } from 'react';
import { cn } from '@/lib/cn';

export interface SegmentedOption<T extends string | number> {
  value: T;
  label: ReactNode;
  ariaLabel?: string;
  title?: string;
}

interface SegmentedControlProps<T extends string | number> {
  value: T;
  options: ReadonlyArray<SegmentedOption<T>>;
  onChange: (value: T) => void;
  ariaLabel: string;
  size?: 'sm' | 'md';
  className?: string;
}

/*
  泛型分段切换器原语：统一承载视图切换（卡片/行）、时间窗口（7天/30天）、
  统计维度（点击/UV、设备/客户端）等高频单选开关。
*/
export function SegmentedControl<T extends string | number>({
  value,
  options,
  onChange,
  ariaLabel,
  size = 'sm',
  className,
}: SegmentedControlProps<T>) {
  return (
    <div
      role="group"
      aria-label={ariaLabel}
      className={cn(
        'inline-flex items-center rounded-lg border border-zinc-200 bg-zinc-50 p-0.5 text-xs',
        className,
      )}
    >
      {options.map((opt) => {
        const active = opt.value === value;
        return (
          <button
            key={String(opt.value)}
            type="button"
            title={opt.title}
            aria-label={opt.ariaLabel}
            aria-pressed={active}
            onClick={() => onChange(opt.value)}
            className={cn(
              'inline-flex items-center justify-center rounded-md font-medium transition-all cursor-pointer',
              size === 'sm' ? 'px-2.5 py-1' : 'px-3 py-1.5',
              active
                ? 'bg-white text-zinc-950 shadow-2xs ring-1 ring-zinc-200/80'
                : 'text-zinc-500 hover:text-zinc-900',
            )}
          >
            {opt.label}
          </button>
        );
      })}
    </div>
  );
}
