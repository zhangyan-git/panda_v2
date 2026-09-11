import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('@umijs/max', () => ({
  history: { location: { pathname: '/dashboard' }, replace: vi.fn() },
}));
vi.mock('antd', () => ({ message: { error: vi.fn() } }));
vi.mock('./services/user', () => ({ fetchCurrentUser: vi.fn() }));

import { history } from '@umijs/max';
import { fetchCurrentUser } from './services/user';
import { getInitialState, request } from './app';
import { readTokens, saveTokens } from './services/session';

const tokens = { accessToken: 'test-access', refreshToken: 'test-refresh' };

beforeEach(() => {
  vi.clearAllMocks();
  const data = new Map<string, string>();
  vi.stubGlobal('localStorage', {
    getItem: (key: string) => data.get(key) ?? null,
    setItem: (key: string, value: string) => data.set(key, value),
    removeItem: (key: string) => data.delete(key),
  });
  saveTokens(tokens);
});
afterEach(() => { vi.unstubAllGlobals(); });

describe('merchant request session failures', () => {
  it.each([
    [401, '/api/v1/merchant/users/me', true],
    [403, '/api/v1/merchant/users/me', true],
    [403, '/api/v1/merchant/users/me?refresh=1', true],
    [403, '/api/v1/merchant/orders', false],
    [403, '/api/v1/merchant/auth/login', false],
    [503, '/api/v1/merchant/users/me', false],
  ])('handles %s from %s without broadening logout', async (status, url, expires) => {
    const error = { response: { status, config: { url } } };
    const interceptor = request.responseInterceptors![0] as [unknown, (error: unknown) => Promise<never>];
    await expect(interceptor[1](error)).rejects.toBe(error);
    expect(readTokens()).toEqual(expires ? undefined : tokens);
    expect(history.replace).toHaveBeenCalledTimes(expires ? 1 : 0);
  });
});

describe('merchant initial identity validation', () => {
  it('keeps credentials but blocks the protected page on dependency failure', async () => {
    vi.mocked(fetchCurrentUser).mockRejectedValueOnce({ response: { status: 503 } });
    expect(await getInitialState()).toEqual({});
    expect(readTokens()).toEqual(tokens);
    expect(history.replace).toHaveBeenCalledWith('/login');
  });

  it('restores a validated user without redirecting', async () => {
    const user = { id: 'user', username: 'operator', name: '', email: '', merchantId: 'merchant', merchantName: 'Test merchant' };
    vi.mocked(fetchCurrentUser).mockResolvedValueOnce(user);
    expect(await getInitialState()).toEqual({ currentUser: user });
    expect(history.replace).not.toHaveBeenCalled();
  });
});
