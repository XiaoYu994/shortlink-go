import { cn } from '@/lib/cn';

const PALETTE = [
  'bg-indigo-50 text-indigo-700 ring-indigo-200/80',
  'bg-sky-50 text-sky-700 ring-sky-200/80',
  'bg-emerald-50 text-emerald-700 ring-emerald-200/80',
  'bg-amber-50 text-amber-800 ring-amber-200/80',
  'bg-rose-50 text-rose-700 ring-rose-200/80',
  'bg-violet-50 text-violet-700 ring-violet-200/80',
  'bg-teal-50 text-teal-700 ring-teal-200/80',
] as const;

function hashString(input: string): number {
  let h = 0;
  for (let i = 0; i < input.length; i++) {
    h = (h * 31 + input.charCodeAt(i)) | 0;
  }
  return Math.abs(h);
}

interface MonogramAvatarProps {
  seed: string;
  label?: string;
  size?: 'sm' | 'md';
  className?: string;
}

/*
  确定性单字母圆形徽标：根据域名或邮箱哈希映射柔和配色，
  在短链列表和成员列表中充当视觉锚点，且完全不依赖第三方 favicon 服务（无隐私泄漏与裂图风险）。
*/
export function MonogramAvatar({ seed, label, size = 'md', className }: MonogramAvatarProps) {
  const clean = (label || seed || '?').trim();
  const initial = clean.slice(0, 1).toUpperCase();
  const tone = PALETTE[hashString(seed.toLowerCase()) % PALETTE.length] ?? PALETTE[0];

  return (
    <span
      aria-hidden="true"
      className={cn(
        'inline-flex shrink-0 items-center justify-center rounded-full font-semibold ring-1 ring-inset select-none',
        size === 'sm' ? 'size-7 text-xs' : 'size-9 text-sm',
        tone,
        className,
      )}
    >
      {initial}
    </span>
  );
}
