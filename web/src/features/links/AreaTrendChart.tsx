import { useId, useState } from 'react';
import { CopyButton } from '@/components/CopyButton';
import { SegmentedControl } from '@/components/ui/segmented-control';
import { cn } from '@/lib/cn';
import { formatCompactNumber, formatNumber } from '@/lib/format';

interface AreaTrendChartProps {
  shortUrl: string;
  clicks: Array<{ day: string; clicks: number }>;
  visitors: Array<{ day: string; visitors: number }>;
}

type MetricKey = 'clicks' | 'visitors';

interface Point {
  x: number;
  y: number;
  day: string;
  clicks: number;
  visitors: number;
  value: number;
}

/*
  手写平滑三次贝塞尔曲线路径：使用水平控制点 (midX, y0) -> (midX, y1)，
  保证曲线永远不会向下越过 0 轴（常见三次样条容易出现负数下冲）。
*/
function buildSmoothLinePath(points: Point[]): string {
  const first = points[0];
  if (!first) return '';
  if (points.length === 1) return `M ${first.x} ${first.y}`;
  let d = `M ${first.x} ${first.y}`;
  for (let i = 1; i < points.length; i++) {
    const prev = points[i - 1];
    const curr = points[i];
    if (!prev || !curr) continue;
    const midX = (prev.x + curr.x) / 2;
    d += ` C ${midX} ${prev.y}, ${midX} ${curr.y}, ${curr.x} ${curr.y}`;
  }
  return d;
}

export function AreaTrendChart({ shortUrl, clicks, visitors }: AreaTrendChartProps) {
  const gradientId = useId();
  const [metric, setMetric] = useState<MetricKey>('clicks');
  const [hoveredIdx, setHoveredIdx] = useState<number | null>(null);

  const width = 760;
  const height = 230;
  const padTop = 18;
  const padRight = 16;
  const padBottom = 30;
  const padLeft = 44;
  const plotW = width - padLeft - padRight;
  const plotH = height - padTop - padBottom;

  const series = clicks.map((item, idx) => {
    const v = visitors[idx]?.visitors ?? 0;
    return {
      day: item.day,
      clicks: item.clicks,
      visitors: v,
      value: metric === 'clicks' ? item.clicks : v,
    };
  });

  const maxVal = Math.max(...series.map((s) => s.value), 0);
  const yCeiling = maxVal === 0 ? 4 : Math.ceil(maxVal * 1.15);
  const hasAnyTraffic = clicks.some((c) => c.clicks > 0) || visitors.some((v) => v.visitors > 0);

  const points: Point[] = series.map((item, i) => {
    const x =
      series.length <= 1 ? padLeft + plotW / 2 : padLeft + (i / (series.length - 1)) * plotW;
    const y = padTop + plotH - (item.value / yCeiling) * plotH;
    return { ...item, x, y };
  });

  const linePath = buildSmoothLinePath(points);
  const baseY = padTop + plotH;
  const firstPoint = points[0];
  const lastPoint = points[points.length - 1];
  const areaPath =
    firstPoint && lastPoint
      ? `${linePath} L ${lastPoint.x} ${baseY} L ${firstPoint.x} ${baseY} Z`
      : '';

  const yTicks = [0, 0.33, 0.66, 1].map((ratio) => ({
    value: Math.round(yCeiling * ratio),
    y: padTop + plotH * (1 - ratio),
  }));

  // 日期标签最多展示 7 个，防止 30 天模式下横轴拥挤重叠。
  const xStep = Math.max(1, Math.ceil(points.length / 7));
  const hovered = hoveredIdx !== null ? (points[hoveredIdx] ?? null) : null;
  const strokeColor = metric === 'clicks' ? '#4f46e5' : '#059669';

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2 text-xs text-zinc-500">
          <span
            className={cn(
              'inline-block size-2 rounded-full',
              metric === 'clicks' ? 'bg-indigo-600' : 'bg-emerald-600',
            )}
          />
          {metric === 'clicks'
            ? '每日点击趋势（按世界标准时 UTC 分日）'
            : '每日去重访客趋势（按世界标准时 UTC 分日）'}
        </div>

        <SegmentedControl<MetricKey>
          ariaLabel="切换趋势指标"
          value={metric}
          onChange={setMetric}
          options={[
            { value: 'clicks', label: '点击次数' },
            { value: 'visitors', label: '独立访客 (UV)' },
          ]}
        />
      </div>

      <div className="relative">
        <svg
          viewBox={`0 0 ${width} ${height}`}
          className="h-56 w-full overflow-visible select-none"
          role="img"
          aria-label={metric === 'clicks' ? '每日点击趋势图' : '每日访客趋势图'}
          onMouseLeave={() => setHoveredIdx(null)}
        >
          <defs>
            <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={strokeColor} stopOpacity="0.22" />
              <stop offset="100%" stopColor={strokeColor} stopOpacity="0.0" />
            </linearGradient>
          </defs>

          {/* 水平虚线网格 + Y 轴刻度 */}
          {yTicks.map((tick) => (
            <g key={tick.y}>
              <line
                x1={padLeft}
                x2={width - padRight}
                y1={tick.y}
                y2={tick.y}
                stroke="#e4e4e7"
                strokeDasharray={tick.value === 0 ? undefined : '3 3'}
                strokeWidth="1"
              />
              <text
                x={padLeft - 8}
                y={tick.y + 4}
                textAnchor="end"
                className="fill-zinc-400 font-mono text-[10px]"
              >
                {formatCompactNumber(tick.value)}
              </text>
            </g>
          ))}

          {/* 渐变面积 + 折线 */}
          {areaPath && <path d={areaPath} fill={`url(#${gradientId})`} />}
          {linePath && (
            <path
              d={linePath}
              fill="none"
              stroke={strokeColor}
              strokeWidth="2.2"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          )}

          {/* X 轴日期刻度 */}
          {points.map((pt, i) => {
            if (i % xStep !== 0 && i !== points.length - 1) return null;
            return (
              <text
                key={pt.day}
                x={pt.x}
                y={height - 6}
                textAnchor="middle"
                className="fill-zinc-400 font-mono text-[10px]"
              >
                {pt.day.slice(5)}
              </text>
            );
          })}

          {/* Hover 垂直引导线与高亮点 */}
          {hovered && (
            <g>
              <line
                x1={hovered.x}
                x2={hovered.x}
                y1={padTop}
                y2={baseY}
                stroke="#a1a1aa"
                strokeDasharray="3 3"
                strokeWidth="1"
              />
              <circle
                cx={hovered.x}
                cy={hovered.y}
                r="5"
                fill="#ffffff"
                stroke={strokeColor}
                strokeWidth="2.5"
              />
            </g>
          )}

          {/* 透明交互列：鼠标滑过图表任意区域平滑吸附到最近日期 */}
          {points.map((pt, i) => {
            const colW = points.length <= 1 ? plotW : plotW / (points.length - 1);
            const left = Math.max(padLeft, pt.x - colW / 2);
            const right = Math.min(width - padRight, pt.x + colW / 2);
            return (
              <rect
                key={pt.day}
                x={left}
                y={padTop}
                width={Math.max(4, right - left)}
                height={plotH}
                fill="transparent"
                className="cursor-crosshair"
                onMouseEnter={() => setHoveredIdx(i)}
              />
            );
          })}
        </svg>

        {/* Hover 悬浮信息卡 */}
        {hovered && (
          <div
            style={{
              left: `${Math.min(Math.max((hovered.x / width) * 100, 14), 86)}%`,
            }}
            className="pointer-events-none absolute top-2 -translate-x-1/2 rounded-lg bg-zinc-900 px-3 py-2 text-xs text-white shadow-lg"
          >
            <div className="font-mono text-[11px] text-zinc-400">{hovered.day}</div>
            <div className="mt-1 flex items-center gap-3 font-mono tabular-nums">
              <span>{formatNumber(hovered.clicks)} 点击</span>
              <span className="text-zinc-500">·</span>
              <span className="text-emerald-300">{formatNumber(hovered.visitors)} UV</span>
            </div>
          </div>
        )}

        {/* 零流量引导浮层（Dub.co 风格：避免枯燥的一条零底线） */}
        {!hasAnyTraffic && (
          <div className="absolute inset-0 flex flex-col items-center justify-center rounded-lg bg-white/75 backdrop-blur-[1px]">
            <p className="text-sm font-medium text-zinc-700">该时间范围内暂无点击记录</p>
            <p className="mt-1 text-xs text-zinc-500">
              复制并访问一次短链，约 1 秒后即可看到趋势。
            </p>
            <div className="mt-3">
              <CopyButton
                value={shortUrl}
                variant="pill"
                size="sm"
                label="复制短链测试"
                toastMessage={`已复制 ${shortUrl}`}
              />
            </div>
          </div>
        )}
      </div>
    </div>
  );
}
