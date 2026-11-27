import { cn } from '@/lib/cn';
import { formatDateTime, isoToLocalDatetime, localDatetimeToISO } from '@/lib/format';
import type { ExpirationSelection } from './expiration-helpers';
import { Input, Label } from './ui/input';

const PRESETS: ReadonlyArray<{ hours: number; label: string }> = [
  { hours: 1, label: '1 小时' },
  { hours: 24, label: '24 小时' },
  { hours: 24 * 7, label: '7 天' },
  { hours: 24 * 30, label: '30 天' },
];

interface ExpirationPickerProps {
  id: string;
  label?: string;
  mode: 'create' | 'edit';
  initialIso?: string;
  value: ExpirationSelection;
  onChange: (next: ExpirationSelection) => void;
}

/*
  统一过期时间选择器：
  用快捷预设胶囊 + 显式时区反馈替代裸露的原生 datetime-local 输入框，
  并在编辑态明确区分「保持当前」「永不过期」与「自定义时间」，根治清空歧义。
*/
export function ExpirationPicker({
  id,
  label = '有效期设置',
  mode,
  initialIso,
  value,
  onChange,
}: ExpirationPickerProps) {
  const minLocal = isoToLocalDatetime(new Date().toISOString());
  const hasExisting = mode === 'edit' && Boolean(initialIso);

  function selectPreset(hours: number) {
    const iso = new Date(Date.now() + hours * 3600_000).toISOString();
    onChange({
      kind: 'preset',
      presetHours: hours,
      iso,
      customLocal: isoToLocalDatetime(iso),
    });
  }

  const activeIso =
    value.kind === 'preset'
      ? value.iso
      : value.kind === 'custom'
        ? localDatetimeToISO(value.customLocal)
        : undefined;

  return (
    <div className="space-y-2">
      {value.kind === 'custom' ? (
        <Label htmlFor={id}>{label}</Label>
      ) : (
        <span className="block text-sm font-medium leading-none text-ink">{label}</span>
      )}

      <div className="flex flex-wrap gap-1.5" role="group" aria-label={`${label}选项`}>
        {hasExisting && (
          <button
            type="button"
            aria-pressed={value.kind === 'keep'}
            onClick={() =>
              onChange({
                kind: 'keep',
                iso: initialIso,
                customLocal: isoToLocalDatetime(initialIso),
              })
            }
            className={cn(
              'rounded-lg border px-2.5 py-1 text-xs font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink cursor-pointer',
              value.kind === 'keep'
                ? 'border-ink bg-hl-yellow text-ink'
                : 'border-border-soft bg-paper-warm/70 text-ink-secondary hover:border-ink/60 hover:text-ink',
            )}
          >
            保持当前
          </button>
        )}

        <button
          type="button"
          aria-pressed={value.kind === 'never'}
          onClick={() => onChange({ kind: 'never', customLocal: '' })}
          className={cn(
            'rounded-lg border px-2.5 py-1 text-xs font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink cursor-pointer',
            value.kind === 'never'
              ? 'border-ink bg-hl-yellow text-ink'
              : 'border-border-soft bg-paper-warm/70 text-ink-secondary hover:border-ink/60 hover:text-ink',
          )}
        >
          永不过期
        </button>

        {PRESETS.map((preset) => {
          const active = value.kind === 'preset' && value.presetHours === preset.hours;
          return (
            <button
              key={preset.hours}
              type="button"
              aria-pressed={active}
              onClick={() => selectPreset(preset.hours)}
              className={cn(
                'rounded-lg border px-2.5 py-1 text-xs font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink cursor-pointer',
                active
                  ? 'border-ink bg-hl-yellow text-ink'
                  : 'border-border-soft bg-paper-warm/70 text-ink-secondary hover:border-ink/60 hover:text-ink',
              )}
            >
              {preset.label}
            </button>
          );
        })}

        <button
          type="button"
          aria-pressed={value.kind === 'custom'}
          onClick={() =>
            onChange({
              kind: 'custom',
              customLocal:
                value.customLocal ||
                isoToLocalDatetime(new Date(Date.now() + 24 * 3600_000).toISOString()),
            })
          }
          className={cn(
            'rounded-lg border px-2.5 py-1 text-xs font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ink cursor-pointer',
            value.kind === 'custom'
              ? 'border-ink bg-hl-yellow text-ink'
              : 'border-border-soft bg-paper-warm/70 text-ink-secondary hover:border-ink/60 hover:text-ink',
          )}
        >
          自定义
        </button>
      </div>

      {value.kind === 'custom' && (
        <Input
          id={id}
          name={id}
          type="datetime-local"
          min={minLocal}
          className="w-full sm:w-64"
          value={value.customLocal}
          onChange={(e) =>
            onChange({
              kind: 'custom',
              customLocal: e.target.value,
            })
          }
        />
      )}

      <p className="text-xs text-muted-foreground">
        {value.kind === 'keep' && initialIso
          ? `当前到期时间：${formatDateTime(initialIso)}（本地时间）`
          : value.kind === 'never'
            ? hasExisting
              ? '保存后将清除原过期时间，改为长期有效。'
              : '不设到期时间，提交后长期有效。'
            : activeIso
              ? `将于 ${formatDateTime(activeIso)}（本地时间）自动失效。`
              : '请选择具体日期与时间（需晚于当前时刻）。'}
      </p>
    </div>
  );
}
