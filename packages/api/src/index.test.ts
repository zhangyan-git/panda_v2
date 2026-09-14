import { describe, expect, it, vi } from 'vitest';
import { ApiClient, ApiError, MemoryTokenStorage } from './index.js';

describe('ApiClient', () => {
  it('decodes successful responses and sends bearer token', async () => {
    const fetch = vi.fn(async (_url: string, init?: RequestInit) =>
      new Response(
        JSON.stringify({ success: true, data: { id: 1 } }),
        { status: 200, headers: { 'content-type': 'application/json' } },
      ),
    );
    const storage = new MemoryTokenStorage();
    storage.set({ accessToken: 'a', refreshToken: 'r' });
    await expect(
      new ApiClient({ fetch: fetch as typeof globalThis.fetch, storage }).get('/me'),
    ).resolves.toEqual({ id: 1 });
    expect((fetch.mock.calls[0][1]?.headers as Headers).get('Authorization')).toBe('Bearer a');
  });

  it('refreshes once on 401 and retries', async () => {
    let calls = 0;
    const fetch = vi.fn(async () => {
      calls++;
      return calls === 1
        ? new Response('{}', { status: 401 })
        : new Response(JSON.stringify({ success: true, data: 2 }), { status: 200 });
    });
    const storage = new MemoryTokenStorage();
    storage.set({ accessToken: 'old', refreshToken: 'refresh' });
    await expect(
      new ApiClient({
        fetch: fetch as typeof globalThis.fetch,
        storage,
        onRefresh: async () => ({ accessToken: 'new', refreshToken: 'refresh' }),
      }).get('/x'),
    ).resolves.toBe(2);
    expect(calls).toBe(2);
    expect(storage.get()?.accessToken).toBe('new');
  });

  it('throws ApiError for error responses', async () => {
    const fetch = async () =>
      new Response(
        JSON.stringify({ success: false, errorCode: 'INVALID_REQUEST', errorMessage: 'no' }),
        { status: 400 },
      );
    const err = await new ApiClient({ fetch: fetch as typeof globalThis.fetch })
      .get('/x')
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(400);
    expect((err as ApiError).errorCode).toBe('INVALID_REQUEST');
  });

  it('throws ApiError with errorCode from non-ok response without body', async () => {
    const fetch = async () => new Response('not json', { status: 500 });
    const err = await new ApiClient({ fetch: fetch as typeof globalThis.fetch })
      .get('/x')
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(500);
  });
});
