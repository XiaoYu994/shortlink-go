import { cn } from '@/lib/cn';

const STAMP_PALETTE = [
  'bg-hl-yellow/65',
  'bg-hl-mint/65',
  'bg-hl-peach/65',
  'bg-sky-100',
  'bg-rose-100',
  'bg-violet-100',
] as const;

function hashString(input: string): number {
  let h = 0;
  for (let i = 0; i < input.length; i++) {
    h = (h * 31 + input.charCodeAt(i)) | 0;
  }
  return Math.abs(h);
}

interface StampAvatarProps {
  seed: string;
  label?: string;
  size?: 'sm' | 'md';
  statusDot?: 'active' | 'warning' | 'banned';
  className?: string;
}

/*
  手绘邮票徽标：
  用带锯齿齿孔的邮票方形框替代普通圆球头像，根据域名/邮箱生成柔和荧光底色，
  在短链列表、组织切换器与成员页形成统一的品牌视觉锚点。
*/
export function StampAvatar({ seed, label, size = 'md', statusDot, className }: StampAvatarProps) {
  const clean = (label || seed || '?').trim();
  const initial = clean.slice(0, 1).toUpperCase();
  const tone =
    STAMP_PALETTE[hashString(seed.toLowerCase()) % STAMP_PALETTE.length] ?? STAMP_PALETTE[0];
  const boxSize = size === 'sm' ? 'size-8' : 'size-10';
  const textSize = size === 'sm' ? 'text-xs' : 'text-sm';

  return (
    <span
      aria-hidden="true"
      className={cn(
        'relative inline-flex shrink-0 items-center justify-center select-none',
        boxSize,
        className,
      )}
    >
      <svg viewBox="0 0 40 40" className="absolute inset-0 size-full" fill="none">
        <path
          d="M4 2H9A2.2 2.2 0 0 0 13 2H18A2.2 2.2 0 0 0 22 2H27A2.2 2.2 0 0 0 31 2H36A2 2 0 0 1 38 4V9A2.2 2.2 0 0 0 38 13V18A2.2 2.2 0 0 0 38 22V27A2.2 2.2 0 0 0 38 31V36A2 2 0 0 1 36 38H31A2.2 2.2 0 0 0 27 38H22A2.2 2.2 0 0 0 18 38H13A2.2 2.2 0 0 0 9 38H4A2 2 0 0 1 2 36V31A2.2 2.2 0 0 0 2 27V22A2.2 2.2 0 0 0 2 18V13A2.2 2.2 0 0 0 2 9V4A2 2 0 0 1 4 2Z"
          fill="#FFFFFF"
          stroke="#1F1E1B"
          strokeWidth="1.4"
        />
      </svg>
      <span
        className={cn(
          'relative z-10 flex items-center justify-center rounded-[2px] border border-ink/80 font-mono font-bold text-ink',
          size === 'sm' ? 'size-[22px]' : 'size-[26px]',
          textSize,
          tone,
        )}
      >
        {initial}
      </span>
      {statusDot && (
        <span
          className={cn(
            'absolute -right-0.5 -top-0.5 z-20 size-2.5 rounded-full border-[1.5px] border-card',
            statusDot === 'active'
              ? 'bg-emerald-500'
              : statusDot === 'warning'
                ? 'bg-amber-500'
                : 'bg-stamp-red',
          )}
        />
      )}
    </span>
  );
}
