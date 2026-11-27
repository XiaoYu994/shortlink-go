import { cn } from '@/lib/cn';

export type CourierCatPose =
  'hero' | 'success' | 'empty' | 'shield' | 'stamp' | 'analytics' | 'toast';

interface CourierCatProps {
  pose: CourierCatPose;
  className?: string;
}

/*
  Ori 折纸信使小猫吉祥物（纯本地内联 SVG，零外网请求）：
  采用一致的炭墨手绘轮廓线 (#1F1E1B) 与三色荧光笔局部点缀，
  分别服务于登录主视觉、建链成功、空列表、权限/风控守护、拦截印章与统计图表。
*/
export function CourierCat({ pose, className }: CourierCatProps) {
  if (pose === 'hero') {
    return (
      <svg
        viewBox="0 0 360 210"
        fill="none"
        aria-hidden="true"
        className={cn('w-full max-w-[360px] select-none', className)}
      >
        {/* 柔和荧光晕染底色 */}
        <ellipse cx="122" cy="128" rx="68" ry="52" fill="#FEF08A" fillOpacity="0.45" />
        <ellipse cx="292" cy="68" rx="44" ry="30" fill="#A7F3D0" fillOpacity="0.48" />

        {/* 左侧长链接纸带（置于纸带下方安全区，避免触碰猫尾或猫身） */}
        <path
          d="M12 162C30 152 54 160 76 150"
          stroke="#1F1E1B"
          strokeWidth="2.2"
          strokeLinecap="round"
        />
        <text x="10" y="182" fill="#6B6860" fontFamily="monospace" fontSize="7.5">
          https://long...
        </text>

        {/* 小猫尾巴 */}
        <path
          d="M84 148C68 142 64 126 74 114"
          stroke="#1F1E1B"
          strokeWidth="2.4"
          strokeLinecap="round"
        />

        {/* 耳机头梁（置于耳朵后方，避免穿过猫耳轮廓） */}
        <path
          d="M82 78C82 22 174 22 174 78"
          stroke="#1F1E1B"
          strokeWidth="2.4"
          strokeLinecap="round"
        />

        {/* 小猫身体 */}
        <path
          d="M90 116C84 134 86 166 92 174H148C154 166 156 134 150 116"
          fill="#FFFFFF"
          stroke="#1F1E1B"
          strokeWidth="2.4"
          strokeLinejoin="round"
        />

        {/* 邮差斜挎包 */}
        <path d="M98 114L142 152" stroke="#1F1E1B" strokeWidth="2.2" strokeLinecap="round" />
        <rect
          x="126"
          y="138"
          width="32"
          height="24"
          rx="5"
          transform="rotate(-8 126 138)"
          fill="#FDBA74"
          stroke="#1F1E1B"
          strokeWidth="2.2"
        />
        <path
          d="M128 146L142 152L156 142"
          stroke="#1F1E1B"
          strokeWidth="1.8"
          strokeLinejoin="round"
        />

        {/* 小猫头部与耳朵 */}
        <path
          d="M86 84C84 68 92 54 102 52L106 38L118 49C124 48 132 48 138 49L150 38L154 52C164 54 172 68 170 84C169 102 152 114 128 114C104 114 87 102 86 84Z"
          fill="#FFFFFF"
          stroke="#1F1E1B"
          strokeWidth="2.4"
          strokeLinejoin="round"
        />

        {/* 耳机耳罩 */}
        <rect
          x="76"
          y="72"
          width="12"
          height="22"
          rx="6"
          fill="#A7F3D0"
          stroke="#1F1E1B"
          strokeWidth="2.2"
        />
        <rect
          x="168"
          y="72"
          width="12"
          height="22"
          rx="6"
          fill="#A7F3D0"
          stroke="#1F1E1B"
          strokeWidth="2.2"
        />

        {/* 表情：黑豆眼、小鼻头与喵嘴 */}
        <circle cx="114" cy="80" r="3" fill="#1F1E1B" />
        <circle cx="142" cy="80" r="3" fill="#1F1E1B" />
        <ellipse cx="105" cy="87" rx="4.5" ry="2.5" fill="#FDBA74" fillOpacity="0.7" />
        <ellipse cx="151" cy="87" rx="4.5" ry="2.5" fill="#FDBA74" fillOpacity="0.7" />
        <path
          d="M125 84L128 87L131 84"
          stroke="#1F1E1B"
          strokeWidth="2"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
        <path
          d="M122 90C124 93 127 92 128 89C129 92 132 93 134 90"
          stroke="#1F1E1B"
          strokeWidth="1.8"
          strokeLinecap="round"
        />

        {/* 拿着小剪刀/纸带的前爪 */}
        <circle cx="155" cy="126" r="8" fill="#FFFFFF" stroke="#1F1E1B" strokeWidth="2.2" />
        <path
          d="M160 116L176 108M161 124L177 122"
          stroke="#1F1E1B"
          strokeWidth="2"
          strokeLinecap="round"
        />

        {/* 虚线飞行轨迹 */}
        <path
          d="M188 110C208 120 228 116 238 98"
          stroke="#1F1E1B"
          strokeWidth="1.8"
          strokeLinecap="round"
          strokeDasharray="4 5"
        />

        {/* 折纸纸飞机 /r/Ori */}
        <g transform="translate(242, 42) rotate(-8)">
          <path
            d="M84 12L4 36L32 48L44 74L84 12Z"
            fill="#FEF08A"
            stroke="#1F1E1B"
            strokeWidth="2.4"
            strokeLinejoin="round"
          />
          <path d="M84 12L32 48" stroke="#1F1E1B" strokeWidth="2" strokeLinecap="round" />
          <path d="M32 48L48 56" stroke="#1F1E1B" strokeWidth="1.8" strokeLinecap="round" />
        </g>
        <text
          x="200"
          y="132"
          fill="#1F1E1B"
          fontFamily="monospace"
          fontSize="10"
          fontWeight="700"
          transform="rotate(-6 200 132)"
        >
          /r/Ori
        </text>

        {/* 小星芒 */}
        <path d="M222 44V52M218 48H226" stroke="#1F1E1B" strokeWidth="1.8" strokeLinecap="round" />
        <path
          d="M334 106V114M330 110H338"
          stroke="#1F1E1B"
          strokeWidth="1.8"
          strokeLinecap="round"
        />
      </svg>
    );
  }

  if (pose === 'success') {
    return (
      <svg
        viewBox="0 0 140 110"
        fill="none"
        aria-hidden="true"
        className={cn('size-24 select-none', className)}
      >
        <circle cx="64" cy="62" r="38" fill="#A7F3D0" fillOpacity="0.55" />
        <path
          d="M38 70C38 36 98 36 98 70"
          stroke="#1F1E1B"
          strokeWidth="2.2"
          strokeLinecap="round"
        />
        <path
          d="M40 94C38 80 42 68 48 64L52 52L60 60C65 59 71 59 76 60L84 52L88 64C94 68 98 80 96 94H40Z"
          fill="#FFFFFF"
          stroke="#1F1E1B"
          strokeWidth="2.2"
          strokeLinejoin="round"
        />
        <rect
          x="33"
          y="64"
          width="8"
          height="15"
          rx="4"
          fill="#FEF08A"
          stroke="#1F1E1B"
          strokeWidth="2"
        />
        <rect
          x="95"
          y="64"
          width="8"
          height="15"
          rx="4"
          fill="#FEF08A"
          stroke="#1F1E1B"
          strokeWidth="2"
        />
        <path
          d="M56 72C58 69 61 69 63 72M73 72C75 69 78 69 80 72"
          stroke="#1F1E1B"
          strokeWidth="2"
          strokeLinecap="round"
        />
        <path
          d="M64 78C66 81 70 81 72 78"
          stroke="#1F1E1B"
          strokeWidth="1.8"
          strokeLinecap="round"
        />
        <path d="M88 52L116 26" stroke="#1F1E1B" strokeWidth="1.6" strokeDasharray="3 3" />
        <path
          d="M128 14L104 24L114 30L118 40L128 14Z"
          fill="#FEF08A"
          stroke="#1F1E1B"
          strokeWidth="2"
          strokeLinejoin="round"
        />
      </svg>
    );
  }

  if (pose === 'empty') {
    return (
      <svg
        viewBox="0 0 160 110"
        fill="none"
        aria-hidden="true"
        className={cn('h-24 w-36 select-none', className)}
      >
        <circle cx="56" cy="60" r="34" fill="#FEF08A" fillOpacity="0.45" />
        {/* 小信箱 */}
        <path
          d="M88 50C88 38 98 32 110 32H126C136 32 142 40 142 52V78H88V50Z"
          fill="#FFFFFF"
          stroke="#1F1E1B"
          strokeWidth="2.2"
          strokeLinejoin="round"
        />
        <path d="M115 78V98" stroke="#1F1E1B" strokeWidth="2.4" strokeLinecap="round" />
        <path d="M90 50C98 50 104 56 104 78" stroke="#1F1E1B" strokeWidth="2" />
        <path d="M128 22V44" stroke="#1F1E1B" strokeWidth="1.8" strokeLinecap="round" />
        <rect
          x="128"
          y="22"
          width="11"
          height="8"
          rx="1.5"
          fill="#FDBA74"
          stroke="#1F1E1B"
          strokeWidth="1.8"
          strokeLinejoin="round"
        />
        {/* 探头的小猫 */}
        <path
          d="M36 96C34 80 38 66 44 62L47 48L56 56C61 55 67 55 72 56L80 48L83 62C88 66 90 80 88 96H36Z"
          fill="#FFFFFF"
          stroke="#1F1E1B"
          strokeWidth="2.2"
          strokeLinejoin="round"
        />
        <circle cx="56" cy="70" r="2.5" fill="#1F1E1B" />
        <circle cx="72" cy="70" r="2.5" fill="#1F1E1B" />
        <path
          d="M62 76C64 78 66 78 68 76"
          stroke="#1F1E1B"
          strokeWidth="1.8"
          strokeLinecap="round"
        />
        <path
          d="M94 13C98 8 104 9 104 14C104 18 99 19 99 22"
          stroke="#1F1E1B"
          strokeWidth="1.8"
          strokeLinecap="round"
        />
        <circle cx="99" cy="26" r="1.2" fill="#1F1E1B" />
      </svg>
    );
  }

  if (pose === 'shield') {
    return (
      <svg
        viewBox="0 0 150 110"
        fill="none"
        aria-hidden="true"
        className={cn('h-24 w-32 select-none', className)}
      >
        <circle cx="76" cy="58" r="36" fill="#A7F3D0" fillOpacity="0.45" />
        {/* 放大镜与内部小盾牌 */}
        <circle cx="48" cy="56" r="22" fill="#FFFFFF" stroke="#1F1E1B" strokeWidth="2.2" />
        <path
          d="M36 47L48 42L60 47V57C60 64 54.5 68.5 48 71C41.5 68.5 36 64 36 57V47Z"
          fill="#A7F3D0"
          stroke="#1F1E1B"
          strokeWidth="2"
          strokeLinejoin="round"
        />
        <path
          d="M43 56L46.5 59.5L53.5 52.5"
          stroke="#1F1E1B"
          strokeWidth="2"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
        <path d="M64 72L74 83" stroke="#1F1E1B" strokeWidth="3" strokeLinecap="round" />
        {/* 小猫 */}
        <path
          d="M78 94C76 78 80 64 86 60L89 46L98 54C103 53 109 53 114 54L122 46L125 60C130 64 132 78 130 94H78Z"
          fill="#FFFFFF"
          stroke="#1F1E1B"
          strokeWidth="2.2"
          strokeLinejoin="round"
        />
        <circle cx="98" cy="68" r="2.5" fill="#1F1E1B" />
        <circle cx="114" cy="68" r="2.5" fill="#1F1E1B" />
        <path d="M104 75H108" stroke="#1F1E1B" strokeWidth="2" strokeLinecap="round" />
      </svg>
    );
  }

  if (pose === 'stamp') {
    return (
      <svg
        viewBox="0 0 140 104"
        fill="none"
        aria-hidden="true"
        className={cn('h-24 w-32 select-none', className)}
      >
        <circle cx="70" cy="56" r="34" fill="#FECACA" fillOpacity="0.55" />
        <path
          d="M38 90C36 74 40 62 46 58L49 44L58 52C63 51 69 51 74 52L82 44L85 58C90 62 92 74 90 90H38Z"
          fill="#FFFFFF"
          stroke="#1F1E1B"
          strokeWidth="2.2"
          strokeLinejoin="round"
        />
        <circle cx="58" cy="66" r="2.5" fill="#1F1E1B" />
        <circle cx="74" cy="66" r="2.5" fill="#1F1E1B" />
        {/* 小印章 */}
        <path d="M104 54V72" stroke="#1F1E1B" strokeWidth="4" strokeLinecap="round" />
        <rect
          x="92"
          y="72"
          width="24"
          height="10"
          rx="2"
          fill="#FDBA74"
          stroke="#1F1E1B"
          strokeWidth="2"
        />
        <rect
          x="88"
          y="82"
          width="32"
          height="5"
          rx="1.5"
          fill="#DC2626"
          stroke="#1F1E1B"
          strokeWidth="1.6"
        />
      </svg>
    );
  }

  if (pose === 'analytics') {
    return (
      <svg
        viewBox="0 0 84 68"
        fill="none"
        aria-hidden="true"
        className={cn('h-12 w-16 select-none', className)}
      >
        <path
          d="M16 62C15 50 18 40 23 36L25 25L33 32C37 31 43 31 47 32L55 25L57 36C62 40 64 50 63 62H16Z"
          fill="#FFFFFF"
          stroke="#1F1E1B"
          strokeWidth="2"
          strokeLinejoin="round"
        />
        {/* 小眼镜 */}
        <circle
          cx="33"
          cy="43"
          r="5"
          fill="#FEF08A"
          fillOpacity="0.4"
          stroke="#1F1E1B"
          strokeWidth="1.6"
        />
        <circle
          cx="47"
          cy="43"
          r="5"
          fill="#FEF08A"
          fillOpacity="0.4"
          stroke="#1F1E1B"
          strokeWidth="1.6"
        />
        <path d="M38 43H42" stroke="#1F1E1B" strokeWidth="1.6" />
        <circle cx="33" cy="43" r="1.5" fill="#1F1E1B" />
        <circle cx="47" cy="43" r="1.5" fill="#1F1E1B" />
        {/* 小记录板 */}
        <rect
          x="52"
          y="42"
          width="20"
          height="22"
          rx="3"
          fill="#A7F3D0"
          stroke="#1F1E1B"
          strokeWidth="1.8"
        />
        <path
          d="M57 49H67M57 54H65M57 59H62"
          stroke="#1F1E1B"
          strokeWidth="1.5"
          strokeLinecap="round"
        />
      </svg>
    );
  }

  // pose === 'toast'
  return (
    <svg
      viewBox="0 0 40 36"
      fill="none"
      aria-hidden="true"
      className={cn('size-6 shrink-0 select-none', className)}
    >
      <path
        d="M7 31C6 22 8 15 11 12L13 5L18 10C20 9.5 22 9.5 24 10L29 5L31 12C34 15 35 22 34 31H7Z"
        fill="#FEF08A"
        stroke="#1F1E1B"
        strokeWidth="2"
        strokeLinejoin="round"
      />
      <circle cx="16.5" cy="19" r="1.6" fill="#1F1E1B" />
      <circle cx="24.5" cy="19" r="1.6" fill="#1F1E1B" />
      <path
        d="M19 23C20 24.5 22 24.5 23 23"
        stroke="#1F1E1B"
        strokeWidth="1.5"
        strokeLinecap="round"
      />
    </svg>
  );
}
