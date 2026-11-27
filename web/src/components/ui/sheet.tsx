import type { ComponentProps } from 'react';
import * as DialogPrimitive from '@radix-ui/react-dialog';
import { X } from 'lucide-react';
import { cn } from '@/lib/cn';

/*
  侧边抽屉（Sheet）基于 Radix Dialog 封装：
  复用焦点捕获、ESC 关闭、Portal 与背景防滚能力，承载移动端侧栏导航与 Webhook 投递详情抽屉。
*/
export const Sheet = DialogPrimitive.Root;
export const SheetTrigger = DialogPrimitive.Trigger;
export const SheetClose = DialogPrimitive.Close;

interface SheetContentProps extends ComponentProps<typeof DialogPrimitive.Content> {
  side?: 'left' | 'right';
  hideCloseButton?: boolean;
}

export function SheetContent({
  side = 'right',
  hideCloseButton = false,
  className,
  children,
  ...props
}: SheetContentProps) {
  return (
    <DialogPrimitive.Portal>
      <DialogPrimitive.Overlay className="fixed inset-0 z-50 bg-ink/35 backdrop-blur-[2px]" />
      <DialogPrimitive.Content
        className={cn(
          'fixed inset-y-0 z-50 flex w-full flex-col border-ink bg-paper shadow-2xl focus:outline-none',
          side === 'right'
            ? 'ori-sheet-right right-0 max-w-xl border-l-[1.5px]'
            : 'ori-sheet-left left-0 max-w-[260px] border-r-[1.5px]',
          className,
        )}
        {...props}
      >
        {children}
        {!hideCloseButton && (
          <DialogPrimitive.Close
            aria-label="关闭"
            className="absolute right-4 top-4 inline-flex size-7 items-center justify-center rounded-lg border border-transparent text-ink-secondary transition-colors hover:border-ink/40 hover:bg-paper-warm hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink cursor-pointer"
          >
            <X className="size-4" />
          </DialogPrimitive.Close>
        )}
      </DialogPrimitive.Content>
    </DialogPrimitive.Portal>
  );
}

export function SheetHeader({ className, ...props }: ComponentProps<'div'>) {
  return (
    <div
      className={cn(
        'flex flex-col gap-1.5 border-b border-border-soft bg-card px-6 py-4',
        className,
      )}
      {...props}
    />
  );
}

export function SheetTitle({ className, ...props }: ComponentProps<typeof DialogPrimitive.Title>) {
  return (
    <DialogPrimitive.Title
      className={cn('text-base font-semibold tracking-tight text-ink', className)}
      {...props}
    />
  );
}

export function SheetDescription({
  className,
  ...props
}: ComponentProps<typeof DialogPrimitive.Description>) {
  return (
    <DialogPrimitive.Description
      className={cn('text-xs text-ink-secondary', className)}
      {...props}
    />
  );
}
