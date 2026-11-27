import { useEffect, useId, useRef, useState } from 'react';
import { CopyButton } from '@/components/CopyButton';
import { CourierCat } from '@/components/CourierCat';
import { SegmentedControl } from '@/components/ui/segmented-control';
import { formatCompactNumber, formatNumber } from '@/lib/format';

interface AreaTrendChartProps {
  shortUrl: string;
  clicks: Array<{ day: string; clicks: number }>;
  visitors: Array<{ day: string; visitors: number }>;
}

type ChartMode = 'both' | 'clicks' | 'visitors';

interface ChartPoint {
  x: number;
  yClicks: number;
  yVisitors: number;
  day: string;
  clicks: number;
  visitors: number;
}

/*
  手写平滑三次贝塞尔曲线路径：使用水平控制点 (midX, y0) -> (midX, y1)，
  保证曲线永远不会向下越过 0 轴（常见三次样条容易出现负数下冲）。
*/
function buildSmoothLinePath(coords: Array<{ x: number; y: number }>): string {
  const first = coords[0];
  if (!first) return '';
  if (coords.length === 1) return `M ${first.x} ${first.y}`;
  let d = `M ${first.x} ${first.y}`;
  for (let i = 1; i < coords.length; i++) {
    const prev = coords[i - 1];
    const curr = coords[i];
    if (!prev || !curr) continue;
    const midX = (prev.x + curr.x) / 2;
    d += ` C ${midX} ${prev.y}, ${midX} ${curr.y}, ${curr.x} ${curr.y}`;
  }
  return d;
}

function computeNiceAxis(maxVal: number): { ceiling: number; ticks: number[] } {
  if (maxVal <= 0) {
    return { ceiling: 4, ticks: [0, 1, 2, 3, 4] };
  }
  const rawStep = Math.max(1, Math.ceil((maxVal * 1.1) / 4));
  const mag = Math.pow(10, Math.floor(Math.log10(rawStep)));
  const norm = rawStep / mag;
  const niceNorm = norm <= 1 ? 1 : norm <= 2 ? 2 : norm <= 5 ? 5 : 10;
  const step = niceNorm * mag;
  return {
    ceiling: step * 4,
    ticks: [0, step, step * 2, step * 3, step * 4],
  };
}

export function AreaTrendChart({ shortUrl, clicks, visitors }: AreaTrendChartProps) {
  const hatchId = useId();
  const containerRef = useRef<HTMLDivElement>(null);
  const [chartWidth, setChartWidth] = useState(760);
  const [mode, setMode] = useState<ChartMode>('both');
  const [activeIdx, setActiveIdx] = useState<number | null>(null);

  useEffect(() => {
    const el = containerRef.current;
    if (!el || typeof ResizeObserver === 'undefined') return;
    const updateWidth = (rawW: number) => {
      if (rawW <= 0) return;
      const clamped = Math.min(Math.max(Math.round(rawW), 300), 960);
      setChartWidth((prev) => (prev === clamped ? prev : clamped));
    };
    updateWidth(el.getBoundingClientRect().width);
    const observer = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) updateWidth(entry.contentRect.width);
    });
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  const width = chartWidth;
  const height = width < 480 ? 200 : 240;
  const padTop = 20;
  const padRight = width < 480 ? 14 : 22;
  const padBottom = 30;
  const padLeft = width < 480 ? 36 : 46;
  const plotW = width - padLeft - padRight;
  const plotH = height - padTop - padBottom;
  const baseY = padTop + plotH;

  const series = clicks.map((item, idx) => ({
    day: item.day,
    clicks: item.clicks,
    visitors: visitors[idx]?.visitors ?? 0,
  }));

  const maxVal = Math.max(
    ...series.map((s) => {
      if (mode === 'clicks') return s.clicks;
      if (mode === 'visitors') return s.visitors;
      return Math.max(s.clicks, s.visitors);
    }),
    0,
  );
  const niceAxis = computeNiceAxis(maxVal);
  const yCeiling = niceAxis.ceiling;
  const hasAnyTraffic = clicks.some((c) => c.clicks > 0) || visitors.some((v) => v.visitors > 0);

  const points: ChartPoint[] = series.map((item, i) => {
    const x =
      series.length <= 1 ? padLeft + plotW / 2 : padLeft + (i / (series.length - 1)) * plotW;
    const yClicks = padTop + plotH - (item.clicks / yCeiling) * plotH;
    const yVisitors = padTop + plotH - (item.visitors / yCeiling) * plotH;
    return { ...item, x, yClicks, yVisitors };
  });

  const showClicks = mode === 'both' || mode === 'clicks';
  const showVisitors = mode === 'both' || mode === 'visitors';

  const clicksLine = showClicks
    ? buildSmoothLinePath(points.map((p) => ({ x: p.x, y: p.yClicks })))
    : '';
  const visitorsLine = showVisitors
    ? buildSmoothLinePath(points.map((p) => ({ x: p.x, y: p.yVisitors })))
    : '';

  const firstPt = points[0];
  const lastPt = points[points.length - 1];
  const clicksArea =
    clicksLine && firstPt && lastPt
      ? `${clicksLine} L ${lastPt.x} ${baseY} L ${firstPt.x} ${baseY} Z`
      : '';
  const visitorsArea =
    mode === 'visitors' && visitorsLine && firstPt && lastPt
      ? `${visitorsLine} L ${lastPt.x} ${baseY} L ${firstPt.x} ${baseY} Z`
      : '';

  const yTicks = niceAxis.ticks.map((value) => ({
    value,
    y: padTop + plotH * (1 - value / yCeiling),
  }));

  // 防止末两个日期刻度重叠：当倒数刻度与最后一个刻度间距小于 56px 时跳过，优先保留首尾日期。
  const maxXTickCount = width < 480 ? 5 : 7;
  const xStep = Math.max(1, Math.ceil(points.length / maxXTickCount));
  const xLabels: ChartPoint[] = [];
  for (let i = 0; i < points.length; i++) {
    const pt = points[i];
    if (!pt) continue;
    const isLast = i === points.length - 1;
    if (i % xStep === 0 || isLast) {
      const prev = xLabels[xLabels.length - 1];
      if (isLast && prev && pt.x - prev.x < 48) {
        xLabels.pop();
      }
      xLabels.push(pt);
    }
  }

  const activePoint = activeIdx !== null ? (points[activeIdx] ?? null) : null;
  const activeAnchorY = activePoint
    ? mode === 'visitors'
      ? activePoint.yVisitors
      : activePoint.yClicks
    : 0;
  const tooltipBelow = activeAnchorY < padTop + plotH * 0.42;
  const tooltipXRatio = activePoint ? activePoint.x / width : 0.5;
  const tooltipTransform =
    tooltipXRatio < 0.22
      ? 'translateX(-12%)'
      : tooltipXRatio > 0.78
        ? 'translateX(-88%)'
        : 'translateX(-50%)';

  function handleKeyDown(e: React.KeyboardEvent<SVGSVGElement>) {
    if (points.length === 0) return;
    if (e.key === 'ArrowRight') {
      e.preventDefault();
      setActiveIdx((prev) => (prev === null ? 0 : Math.min(points.length - 1, prev + 1)));
    } else if (e.key === 'ArrowLeft') {
      e.preventDefault();
      setActiveIdx((prev) => (prev === null ? points.length - 1 : Math.max(0, prev - 1)));
    } else if (e.key === 'Escape') {
      setActiveIdx(null);
    }
  }

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-wrap items-center gap-4 text-xs text-ink-secondary">
          {showClicks && (
            <span className="inline-flex items-center gap-1.5 font-medium text-ink">
              <span className="inline-block h-0.5 w-4 rounded bg-ink" />
              点击次数
            </span>
          )}
          {showVisitors && (
            <span className="inline-flex items-center gap-1.5 font-medium text-ink">
              <span className="inline-block h-0.5 w-4 border-t-2 border-dashed border-emerald-600" />
              独立访客 (UV)
            </span>
          )}
          <span className="text-muted-foreground">按每日零点（UTC）统计</span>
        </div>

        <SegmentedControl<ChartMode>
          ariaLabel="切换趋势指标"
          value={mode}
          onChange={setMode}
          options={[
            { value: 'both', label: '双线对比' },
            { value: 'clicks', label: '仅点击' },
            { value: 'visitors', label: '仅访客' },
          ]}
        />
      </div>

      <div ref={containerRef} className="relative">
        <svg
          tabIndex={0}
          viewBox={`0 0 ${width} ${height}`}
          style={{ width: '100%', height: `${height}px` }}
          className="block overflow-visible rounded-lg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink select-none"
          role="img"
          aria-label="短链每日点击与访客趋势图，可用左右方向键查看每日详情"
          onMouseLeave={() => setActiveIdx(null)}
          onKeyDown={handleKeyDown}
        >
          <defs>
            {/* 手绘斜线铅笔填充纹理 */}
            <pattern
              id={hatchId}
              width="7"
              height="7"
              patternUnits="userSpaceOnUse"
              patternTransform="rotate(40)"
            >
              <line
                x1="0"
                y1="0"
                x2="0"
                y2="7"
                stroke={mode === 'visitors' ? '#059669' : '#1F1E1B'}
                strokeWidth="1.2"
                strokeOpacity="0.14"
              />
            </pattern>
          </defs>

          {/* 水平虚线网格 + Y 轴刻度 */}
          {yTicks.map((tick) => (
            <g key={tick.y}>
              <line
                x1={padLeft}
                x2={width - padRight}
                y1={tick.y}
                y2={tick.y}
                stroke={tick.value === 0 ? '#1F1E1B' : '#E5DFD3'}
                strokeDasharray={tick.value === 0 ? undefined : '4 4'}
                strokeWidth={tick.value === 0 ? '1.25' : '1'}
              />
              <text
                x={padLeft - 8}
                y={tick.y + 4}
                textAnchor="end"
                className="fill-muted-foreground font-mono text-[10px]"
              >
                {formatCompactNumber(tick.value)}
              </text>
            </g>
          ))}

          {/* 手绘斜线面积填充 */}
          {clicksArea && <path d={clicksArea} fill={`url(#${hatchId})`} />}
          {visitorsArea && <path d={visitorsArea} fill={`url(#${hatchId})`} />}

          {/* 点击实线 + UV 虚线 */}
          {visitorsLine && (
            <path
              d={visitorsLine}
              fill="none"
              stroke="#059669"
              strokeWidth="2.2"
              strokeDasharray="5 4"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          )}
          {clicksLine && (
            <path
              d={clicksLine}
              fill="none"
              stroke="#1F1E1B"
              strokeWidth="2.4"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          )}

          {/* X 轴日期刻度 */}
          {xLabels.map((pt) => (
            <text
              key={pt.day}
              x={pt.x}
              y={height - 8}
              textAnchor="middle"
              className="fill-muted-foreground font-mono text-[10px]"
            >
              {pt.day.slice(5)}
            </text>
          ))}

          {/* 激活日期垂直引导线与高亮点 */}
          {activePoint && (
            <g>
              <line
                x1={activePoint.x}
                x2={activePoint.x}
                y1={padTop}
                y2={baseY}
                stroke="#1F1E1B"
                strokeDasharray="3 3"
                strokeWidth="1.2"
              />
              {showVisitors && (
                <circle
                  cx={activePoint.x}
                  cy={activePoint.yVisitors}
                  r="4.5"
                  fill="#A7F3D0"
                  stroke="#1F1E1B"
                  strokeWidth="2"
                />
              )}
              {showClicks && (
                <circle
                  cx={activePoint.x}
                  cy={activePoint.yClicks}
                  r="5"
                  fill="#FEF08A"
                  stroke="#1F1E1B"
                  strokeWidth="2.2"
                />
              )}
            </g>
          )}

          {/* 透明交互列：支持鼠标悬停与移动端触摸/点击 */}
          {points.map((pt, i) => {
            const colW = points.length <= 1 ? plotW : plotW / (points.length - 1);
            const left = Math.max(padLeft, pt.x - colW / 2);
            const right = Math.min(width - padRight, pt.x + colW / 2);
            return (
              <rect
                key={pt.day}
                x={left}
                y={padTop}
                width={Math.max(6, right - left)}
                height={plotH}
                fill="transparent"
                className="cursor-crosshair"
                onMouseEnter={() => setActiveIdx(i)}
                onPointerDown={() => setActiveIdx(i)}
              />
            );
          })}
        </svg>

        {/* 智能避让气泡 Tooltip（波峰处自动下方展示，避免遮挡与边界溢出） */}
        {activePoint && (
          <div
            style={{
              left: `${(activePoint.x / width) * 100}%`,
              transform: tooltipTransform,
              top: tooltipBelow ? `${((activeAnchorY + 18) / height) * 100}%` : undefined,
              bottom: !tooltipBelow
                ? `${((height - activeAnchorY + 14) / height) * 100}%`
                : undefined,
            }}
            className="pointer-events-none absolute z-10 whitespace-nowrap rounded-xl border-[1.5px] border-ink bg-card px-3 py-2 text-xs text-ink ink-shadow-sm"
          >
            <div className="font-mono text-[11px] font-semibold text-muted-foreground">
              {activePoint.day}
            </div>
            <div className="mt-1 flex items-center gap-2.5 whitespace-nowrap font-mono font-bold tabular-nums">
              <span className="hl-swipe-yellow px-1">{formatNumber(activePoint.clicks)} 点击</span>
              <span className="text-ink/30">·</span>
              <span className="hl-swipe-mint px-1">{formatNumber(activePoint.visitors)} UV</span>
            </div>
          </div>
        )}

        {/* 无障碍屏幕阅读器实时播报 */}
        <div className="sr-only" aria-live="polite">
          {activePoint
            ? `${activePoint.day}：${activePoint.clicks} 次点击，${activePoint.visitors} 独立访客`
            : ''}
        </div>

        {/* 零流量引导浮层 */}
        {!hasAnyTraffic && (
          <div className="absolute inset-0 flex flex-col items-center justify-center rounded-xl bg-paper/85 backdrop-blur-[1px]">
            <CourierCat pose="empty" className="mb-1 h-16 w-24" />
            <p className="text-sm font-semibold text-ink">该时间范围内暂无点击记录</p>
            <p className="mt-1 text-xs text-ink-secondary">
              复制并访问一次短链，稍候即可在这里看到点击趋势。
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
