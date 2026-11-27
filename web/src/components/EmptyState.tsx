import type { ReactNode } from 'react';
import type { LucideIcon } from 'lucide-react';

interface EmptyStateProps {
  icon: LucideIcon;
  title: string;
  description: string;
  action?: ReactNode;
  illustration?: ReactNode;
}

export function EmptyState({
  icon: Icon,
  title,
  description,
  action,
  illustration,
}: EmptyStateProps) {
  return (
    <div className="flex flex-col items-center justify-center rounded-2xl border-[1.5px] border-dashed border-ink/35 bg-paper-warm/45 px-6 py-12 text-center">
      {illustration ? (
        <div className="mb-3">{illustration}</div>
      ) : (
        <div className="mb-3.5 flex size-11 items-center justify-center rounded-xl border-[1.25px] border-ink bg-card text-ink ink-shadow-sm">
          <Icon className="size-5" />
        </div>
      )}
      <h3 className="text-sm font-semibold text-ink">{title}</h3>
      <p className="mt-1.5 max-w-md text-sm leading-relaxed text-ink-secondary">{description}</p>
      {action && <div className="mt-5">{action}</div>}
    </div>
  );
}
