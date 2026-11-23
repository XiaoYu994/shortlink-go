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
  短链控制台里最高频的微交互：点击后剪贴板图标立刻切为绿色对勾并保持 1.8 秒。
  用 useEffect 清理定时器，避免组件提前卸载后触发多余状态更新。
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
          'inline-flex items-center gap-1.5 rounded-md border border-border bg-white px-2.5 text-xs font-medium text-zinc-700 shadow-2xs transition-colors hover:bg-zinc-50 hover:text-zinc-950 cursor-pointer',
          size === 'xs' ? 'h-7' : 'h-8',
          copied && 'border-emerald-200 bg-emerald-50 text-emerald-700 hover:bg-emerald-50',
          className,
        )}
      >
        {copied ? <Check className="size-3.5 text-emerald-600" /> : <Copy className="size-3.5" />}
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
        'inline-flex items-center justify-center rounded-full border border-transparent text-zinc-400 transition-all hover:border-zinc-200 hover:bg-zinc-100 hover:text-zinc-700 cursor-pointer',
        size === 'xs' ? 'size-6' : 'size-7',
        copied && 'border-emerald-200 bg-emerald-50 text-emerald-600 hover:bg-emerald-50 hover:text-emerald-600',
        className,
      )}
    >
      {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
    </button>
  );
}
