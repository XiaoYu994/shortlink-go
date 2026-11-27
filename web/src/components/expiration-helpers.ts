import { isoToLocalDatetime, localDatetimeToISO } from '@/lib/format';

export type ExpirationKind = 'never' | 'keep' | 'preset' | 'custom';

export interface ExpirationSelection {
  kind: ExpirationKind;
  presetHours?: number;
  iso?: string;
  customLocal: string;
}

export function createInitialExpiration(
  mode: 'create' | 'edit',
  initialIso?: string,
): ExpirationSelection {
  if (mode === 'edit' && initialIso) {
    return {
      kind: 'keep',
      iso: initialIso,
      customLocal: isoToLocalDatetime(initialIso),
    };
  }
  return {
    kind: 'never',
    customLocal: '',
  };
}

export type ResolvedExpiration =
  | { action: 'unchanged' }
  | { action: 'clear' }
  | { action: 'set'; iso: string }
  | { action: 'error'; message: string };

export function resolveExpiration(
  selection: ExpirationSelection,
  initialIso?: string,
): ResolvedExpiration {
  if (selection.kind === 'keep') {
    return { action: 'unchanged' };
  }
  if (selection.kind === 'never') {
    return initialIso ? { action: 'clear' } : { action: 'unchanged' };
  }
  if (selection.kind === 'preset') {
    if (!selection.iso) {
      return { action: 'error', message: '请选择有效的过期时间预设' };
    }
    return { action: 'set', iso: selection.iso };
  }
  if (!selection.customLocal.trim()) {
    return { action: 'error', message: '请选择具体的自定义过期时间' };
  }
  const iso = localDatetimeToISO(selection.customLocal);
  if (!iso) {
    return { action: 'error', message: '过期时间格式无效' };
  }
  return { action: 'set', iso };
}
