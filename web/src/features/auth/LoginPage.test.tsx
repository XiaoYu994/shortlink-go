import { describe, expect, it } from 'vitest';
import { sanitizeReturnTo } from './return-to';

describe('sanitizeReturnTo', () => {
  it('accepts safe relative console routes', () => {
    expect(sanitizeReturnTo('/orgs/1/links?sort=clicks')).toBe('/orgs/1/links?sort=clicks');
    expect(sanitizeReturnTo('/onboarding')).toBe('/onboarding');
    expect(sanitizeReturnTo('/')).toBe('/');
  });

  it('rejects open redirects, traversal to reserved prefixes, backslashes, and control chars', () => {
    expect(sanitizeReturnTo(null)).toBe('/');
    expect(sanitizeReturnTo('https://evil.example')).toBe('/');
    expect(sanitizeReturnTo('//evil.example')).toBe('/');
    expect(sanitizeReturnTo('/\\evil.example')).toBe('/');
    expect(sanitizeReturnTo('/orgs\n/1')).toBe('/');
    expect(sanitizeReturnTo('/x/../api/v1/me')).toBe('/');
    expect(sanitizeReturnTo('/x/../r/AbC123xy')).toBe('/');
    expect(sanitizeReturnTo('/api/v1/auth/logout')).toBe('/');
    expect(sanitizeReturnTo('/r/AbC123xy')).toBe('/');
  });
});
