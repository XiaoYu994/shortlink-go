import type { ComponentProps } from 'react';
import { ChevronDown } from 'lucide-react';
import { cn } from '@/lib/cn';

export function Input({ className, ...props }: ComponentProps<'input'>) {
  return (
    <input
      className={cn(
        'flex h-9 w-full rounded-lg border-[1.25px] border-ink/80 bg-card px-3 py-1 text-sm text-ink transition-colors placeholder:text-muted-foreground focus-visible:border-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink focus-visible:ring-offset-1 focus-visible:ring-offset-paper disabled:cursor-not-allowed disabled:bg-paper-warm disabled:opacity-60',
        className,
      )}
      {...props}
    />
  );
}

export function Label({ className, ...props }: ComponentProps<'label'>) {
  return (
    <label className={cn('text-sm font-medium leading-none text-ink', className)} {...props} />
  );
}

export function Select({ className, children, ...props }: ComponentProps<'select'>) {
  const fullWidth = Boolean(className?.includes('w-full'));
  return (
    <span className={cn('relative inline-flex items-center', fullWidth && 'w-full')}>
      <select
        className={cn(
          'flex h-9 appearance-none rounded-lg border-[1.25px] border-ink/80 bg-card py-1 pl-3 pr-8 text-sm text-ink transition-colors hover:border-ink focus-visible:border-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink focus-visible:ring-offset-1 focus-visible:ring-offset-paper disabled:cursor-not-allowed disabled:bg-paper-warm disabled:opacity-60 cursor-pointer',
          className,
        )}
        {...props}
      >
        {children}
      </select>
      <ChevronDown
        aria-hidden="true"
        className="pointer-events-none absolute right-2.5 top-1/2 size-4 -translate-y-1/2 text-ink-secondary"
      />
    </span>
  );
}
