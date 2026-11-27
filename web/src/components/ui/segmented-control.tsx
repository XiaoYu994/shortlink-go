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
  泛型分段切换器原语：手绘炭墨外框 + 选中态荧光黄高亮，
  补齐 focus-visible 焦点环，统一承载视图切换、时间窗口与图表指标切换。
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
        'inline-flex items-center rounded-lg border-[1.25px] border-ink/80 bg-paper-warm p-0.5 text-xs',
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
              'inline-flex items-center justify-center rounded-md font-medium transition-all focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink cursor-pointer',
              size === 'sm' ? 'px-2.5 py-1' : 'px-3 py-1.5',
              active
                ? 'border border-ink bg-hl-yellow/90 text-ink shadow-[1px_1px_0_#1F1E1B]'
                : 'border border-transparent text-ink-secondary hover:text-ink',
            )}
          >
            {opt.label}
          </button>
        );
      })}
    </div>
  );
}
