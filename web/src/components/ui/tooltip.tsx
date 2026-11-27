import type { ComponentProps } from 'react';
import * as TooltipPrimitive from '@radix-ui/react-tooltip';
import { cn } from '@/lib/cn';

export const TooltipProvider = TooltipPrimitive.Provider;
export const Tooltip = TooltipPrimitive.Root;
export const TooltipTrigger = TooltipPrimitive.Trigger;

export function TooltipContent({
  className,
  sideOffset = 6,
  ...props
}: ComponentProps<typeof TooltipPrimitive.Content>) {
  return (
    <TooltipPrimitive.Portal>
      <TooltipPrimitive.Content
        sideOffset={sideOffset}
        className={cn(
          'ori-popover-animate z-50 max-w-xs rounded-lg border-[1.25px] border-ink bg-ink px-2.5 py-1.5 text-xs leading-snug text-primary-foreground ink-shadow-sm',
          className,
        )}
        {...props}
      />
    </TooltipPrimitive.Portal>
  );
}
