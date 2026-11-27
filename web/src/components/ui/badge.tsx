import type { ComponentProps } from 'react';
import { cva, type VariantProps } from 'class-variance-authority';
import { cn } from '@/lib/cn';

const badgeVariants = cva(
  'inline-flex items-center gap-1 rounded-full border px-2.5 py-0.5 text-xs font-medium transition-colors',
  {
    variants: {
      variant: {
        default: 'border-ink bg-ink text-primary-foreground',
        secondary: 'border-ink/25 bg-paper-warm text-ink-secondary',
        outline: 'border-ink/75 bg-card text-ink',
        success: 'border-ink/70 bg-hl-mint/80 text-ink',
        warning: 'border-ink/70 bg-hl-peach/65 text-ink',
        yellow: 'border-ink/70 bg-hl-yellow/85 text-ink',
        destructive: 'border-stamp-red/70 bg-red-50 text-[#991b1b]',
        info: 'border-ink/60 bg-sky-100 text-ink',
        stamp: 'stamp-banned rounded-md px-2 py-0.5 font-semibold tracking-wide',
      },
    },
    defaultVariants: {
      variant: 'secondary',
    },
  },
);

export type BadgeProps = ComponentProps<'span'> & VariantProps<typeof badgeVariants>;

export function Badge({ className, variant, ...props }: BadgeProps) {
  return <span className={cn(badgeVariants({ variant }), className)} {...props} />;
}
