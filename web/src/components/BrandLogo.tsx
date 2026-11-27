import { cn } from '@/lib/cn';

interface BrandLogoProps {
  showSubtitle?: boolean;
  size?: 'sm' | 'md';
  className?: string;
}

/*
  Ori（折羽）原创折纸纸飞机品牌标识：
  以「长网址折叠成轻巧纸飞机」为核心隐喻，用统一手绘炭墨线与柠檬黄荧光翼面作为全站视觉锚点。
*/
export function BrandLogo({ showSubtitle = true, size = 'sm', className }: BrandLogoProps) {
  const iconBox = size === 'md' ? 'size-9' : 'size-8';
  const titleSize = size === 'md' ? 'text-lg' : 'text-[16px]';

  return (
    <div className={cn('inline-flex items-center gap-2.5 select-none', className)}>
      <div
        className={cn(
          'relative flex shrink-0 items-center justify-center rounded-lg border-[1.5px] border-ink bg-card ink-shadow-sm',
          iconBox,
        )}
      >
        <svg viewBox="0 0 28 28" className="size-5" fill="none" aria-hidden="true">
          <path
            d="M4.5 21.5C6.5 21 8 19.8 9.2 18.5"
            stroke="#FDBA74"
            strokeWidth="2"
            strokeLinecap="round"
            strokeDasharray="2 3"
          />
          <path
            d="M23.5 5.5L5.2 13.1L12.1 16L14.8 22.8L23.5 5.5Z"
            fill="#FEF08A"
            stroke="#1F1E1B"
            strokeWidth="1.65"
            strokeLinejoin="round"
          />
          <path d="M23.5 5.5L12.1 16" stroke="#1F1E1B" strokeWidth="1.45" strokeLinecap="round" />
          <path d="M12.1 16L16.3 18.9" stroke="#1F1E1B" strokeWidth="1.25" strokeLinecap="round" />
        </svg>
      </div>
      <div className="flex items-baseline gap-1.5">
        <span className={cn('font-bold tracking-tight text-ink', titleSize)}>Ori</span>
        {showSubtitle && (
          <span className="hl-swipe-yellow px-1.5 py-0.5 text-[11px] font-semibold text-ink">
            折羽
          </span>
        )}
      </div>
    </div>
  );
}
