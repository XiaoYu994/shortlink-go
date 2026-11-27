import { AlertCircle } from 'lucide-react';
import { cn } from '@/lib/cn';
import { Button } from './ui/button';

interface ErrorAlertProps {
  message: string;
  onRetry?: () => void;
  compact?: boolean;
  className?: string;
}

/*
  统一错误提示组件：手绘印章红描边 + 浅红纸底，确保错误反馈与重试按钮清晰高对比。
*/
export function ErrorAlert({ message, onRetry, compact = false, className }: ErrorAlertProps) {
  return (
    <div
      role="alert"
      className={cn(
        'flex items-center justify-between gap-3 border-[1.25px] border-stamp-red bg-red-50/90 text-sm text-[#991b1b]',
        compact ? 'rounded-lg px-3 py-2' : 'rounded-xl px-4 py-3',
        className,
      )}
    >
      <span className="flex min-w-0 items-center gap-2">
        <AlertCircle className="size-4 shrink-0 text-stamp-red" />
        <span className="break-words">{message}</span>
      </span>
      {onRetry && (
        <Button variant="outline" size="sm" onClick={onRetry} className="shrink-0">
          重试
        </Button>
      )}
    </div>
  );
}
