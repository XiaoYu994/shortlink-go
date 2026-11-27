import { useEffect, useState } from 'react';
import { Check, Copy } from 'lucide-react';
import { toast } from 'sonner';
import { cn } from '@/lib/cn';
import { copyToClipboard } from '@/lib/format';

interface CopyButtonProps {
  value: string;
  label?: string;
  toastMessage?: string;
  size?: 'xs' | 'sm';
  variant?: 'icon' | 'pill';
  className?: string;
}

/*
  高频剪贴板交互：点击后立刻切换为薄荷绿荧光底 + 对勾并保持 1.8 秒，
  触控热区满足 WCAG 2.2 最小尺寸要求，并补齐高对比度焦点环。
*/
export function CopyButton({
  value,
  label = '复制',
  toastMessage,
  size = 'xs',
  variant = 'icon',
  className,
}: CopyButtonProps) {
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    if (!copied) return;
    const timer = window.setTimeout(() => setCopied(false), 1800);
    return () => window.clearTimeout(timer);
  }, [copied]);

  async function handleCopy() {
    const ok = await copyToClipboard(value);
    if (ok) {
      setCopied(true);
      if (toastMessage) toast.success(toastMessage);
    } else {
      toast.info(`请手动复制：${value}`);
    }
  }

  if (variant === 'pill') {
    return (
      <button
        type="button"
        aria-label={label}
        onClick={() => void handleCopy()}
        className={cn(
          'inline-flex items-center gap-1.5 rounded-lg border-[1.25px] border-ink/80 bg-card px-2.5 text-xs font-medium text-ink transition-colors hover:border-ink hover:bg-paper-warm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink cursor-pointer',
          size === 'xs' ? 'h-7' : 'h-8',
          copied && 'border-ink bg-hl-mint text-ink hover:bg-hl-mint',
          className,
        )}
      >
        {copied ? <Check className="size-3.5 text-ink" /> : <Copy className="size-3.5" />}
        {copied ? '已复制' : label}
      </button>
    );
  }

  return (
    <button
      type="button"
      title={copied ? '已复制' : label}
      aria-label={label}
      onClick={() => void handleCopy()}
      className={cn(
        'inline-flex items-center justify-center rounded-lg border border-transparent text-ink-secondary transition-all hover:border-ink/40 hover:bg-paper-warm hover:text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink cursor-pointer',
        size === 'xs' ? 'size-7' : 'size-8',
        copied && 'border-ink bg-hl-mint text-ink hover:bg-hl-mint',
        className,
      )}
    >
      {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
    </button>
  );
}
