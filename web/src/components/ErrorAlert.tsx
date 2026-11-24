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
  统一错误提示组件：收口表单内联错误与页面级请求失败横幅，
  避免在各个页面重复手写 border-red-200 bg-red-50 样式。
*/
export function ErrorAlert({ message, onRetry, compact = false, className }: ErrorAlertProps) {
  return (
    <div
      role="alert"
      className={cn(
        'flex items-center justify-between gap-3 border border-red-200 bg-red-50 text-sm text-red-700',
        compact ? 'rounded-md px-3 py-2' : 'rounded-xl px-4 py-3',
        className,
      )}
    >
      <span className="flex min-w-0 items-center gap-2">
        <AlertCircle className="size-4 shrink-0 text-red-500" />
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
