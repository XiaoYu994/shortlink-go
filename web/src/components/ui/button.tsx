import type { ComponentProps } from 'react';
import { cva, type VariantProps } from 'class-variance-authority';
import { cn } from '@/lib/cn';

/*
  cva (Class Variance Authority) 声明式管理手绘按钮视觉变体：
  主按钮带炭墨边框与轻微按压偏移感，次级/幽灵按钮克制保持高扫描效率。
*/
const buttonVariants = cva(
  'inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-lg text-sm font-medium transition-all focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink focus-visible:ring-offset-2 focus-visible:ring-offset-paper disabled:pointer-events-none disabled:opacity-45 cursor-pointer [&_svg]:pointer-events-none [&_svg]:size-4 [&_svg]:shrink-0',
  {
    variants: {
      variant: {
        default:
          'border-[1.5px] border-ink bg-ink text-primary-foreground ink-shadow-sm ink-pressable hover:bg-ink/90',
        brand:
          'border-[1.5px] border-ink bg-hl-yellow text-ink ink-shadow-sm ink-pressable hover:bg-[#fde047]',
        highlight:
          'border-[1.5px] border-ink bg-hl-yellow text-ink ink-shadow-sm ink-pressable hover:bg-[#fde047]',
        mint: 'border-[1.5px] border-ink bg-hl-mint text-ink ink-shadow-sm ink-pressable hover:bg-[#86efac]',
        destructive:
          'border-[1.5px] border-ink bg-stamp-red text-destructive-foreground ink-shadow-sm ink-pressable hover:bg-[#b91c1c]',
        outline:
          'border-[1.25px] border-ink/85 bg-card text-ink hover:border-ink hover:bg-paper-warm',
        secondary:
          'border border-border-soft bg-paper-warm text-ink hover:border-ink/40 hover:bg-[#ece4d4]',
        ghost: 'text-ink-secondary hover:bg-paper-warm hover:text-ink',
        link: 'text-ink underline decoration-hl-yellow decoration-[3px] underline-offset-4 hover:decoration-ink',
      },
      size: {
        default: 'h-9 px-4 py-2',
        sm: 'h-8 rounded-lg px-3 text-xs',
        lg: 'h-10 rounded-lg px-5',
        icon: 'size-8',
      },
    },
    defaultVariants: {
      variant: 'default',
      size: 'default',
    },
  },
);

export type ButtonProps = ComponentProps<'button'> & VariantProps<typeof buttonVariants>;

export function Button({ className, variant, size, type = 'button', ...props }: ButtonProps) {
  return (
    <button type={type} className={cn(buttonVariants({ variant, size, className }))} {...props} />
  );
}
