import type { ReactNode } from 'react';
import { cn } from '@/lib/cn';

interface PageHeaderProps {
  title: string;
  description?: string;
  actions?: ReactNode;
}

export function PageHeader({ title, description, actions }: PageHeaderProps) {
  return (
    <div
      className={cn(
        'flex flex-col gap-3 sm:flex-row sm:justify-between',
        description ? 'sm:items-end' : 'sm:items-center',
      )}
    >
      <div className="space-y-1.5">
        <h1 className="inline-block text-2xl font-bold tracking-tight text-ink">
          <span className="underline decoration-hl-yellow decoration-[6px] underline-offset-4 [text-decoration-skip-ink:none]">
            {title}
          </span>
        </h1>
        {description && <p className="text-sm leading-relaxed text-ink-secondary">{description}</p>}
      </div>
      {actions && <div className="flex shrink-0 flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}
