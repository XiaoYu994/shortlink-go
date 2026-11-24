import { describe, expect, it } from 'vitest';
import { translateAPIError } from './error-messages';
import { isoToLocalDatetime } from './format';
import { buildShortUrl, formatShortLabel, isExactShortCode } from './short-url';

describe('error-messages & short-url helpers', () => {
  it('translates backend English sentinel errors into user-friendly Chinese', () => {
    expect(translateAPIError('link quota exceeded', 403)).toContain('本月短链创建配额已用完');
    expect(translateAPIError('link is banned', 403)).toContain('封禁');
    expect(translateAPIError('organization must have at least one owner', 409)).toContain(
      '至少需要保留一位所有者',
    );
    expect(translateAPIError('unknown error', 429)).toContain('操作过于频繁');
  });

  it('builds public short URLs using PUBLIC_BASE_URL when configured', () => {
    expect(buildShortUrl('AbC123xy', 'https://s.example.com/')).toBe(
      'https://s.example.com/r/AbC123xy',
    );
    expect(formatShortLabel('AbC123xy', 'https://s.example.com')).toBe('s.example.com/r/AbC123xy');
    expect(formatShortLabel('AbC123xy')).toBe('/r/AbC123xy');
    expect(isExactShortCode('AbC123xy')).toBe(true);
    expect(isExactShortCode('too-short')).toBe(false);
  });

  it('converts ISO timestamps to datetime-local input values', () => {
    expect(isoToLocalDatetime(undefined)).toBe('');
    expect(isoToLocalDatetime('not-a-date')).toBe('');
    expect(isoToLocalDatetime('2026-12-01T00:00:00Z')).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/);
  });
});
