import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { clearSession, isTokens, readTokens, saveTokens, TOKEN_KEY } from './session';

describe('merchant session', () => {
  beforeEach(() => {
    const data = new Map<string, string>();
    vi.stubGlobal('localStorage', {
      getItem: (key: string) => data.get(key) ?? null,
      setItem: (key: string, value: string) => data.set(key, value),
      removeItem: (key: string) => data.delete(key),
    });
  });
  afterEach(() => { vi.unstubAllGlobals(); });

  it('restores and clears a valid session', () => {
    const tokens = { accessToken: 'access', refreshToken: 'refresh' };
    saveTokens(tokens);
    expect(readTokens()).toEqual(tokens);
    clearSession();
    expect(readTokens()).toBeUndefined();
  });

  it.each(['broken-json', 'null', '{}', '{"accessToken":""}', '{"success":true,"data":{"accessToken":"a","refreshToken":"r"}}'])('clears invalid storage %s', (raw) => {
    localStorage.setItem(TOKEN_KEY, raw);
    expect(readTokens()).toBeUndefined();
    expect(localStorage.getItem(TOKEN_KEY)).toBeNull();
  });

  it.each([null, {}, { accessToken: ' ', refreshToken: 'r' }, { accessToken: 1, refreshToken: 'r' }])('rejects invalid tokens', (tokens) => {
    expect(isTokens(tokens)).toBe(false);
    expect(() => saveTokens(tokens)).toThrow('登录响应无效');
    expect(readTokens()).toBeUndefined();
  });
});
