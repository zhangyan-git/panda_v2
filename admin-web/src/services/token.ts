import { refreshToken as requestRefresh } from './user';

export type AuthTokens = { accessToken: string; refreshToken: string };

// 登录页、请求拦截器、刷新逻辑读的都是这一个 key。这个字面量原来散在登录页和
// app.ts 里好几处，任何一处拼错都表现为「莫名其妙掉登录」，集中到一处。
const STORAGE_KEY = 'panda.auth.tokens';

export function readTokens(): AuthTokens | null {
  const raw = localStorage.getItem(STORAGE_KEY);
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw) as Partial<AuthTokens>;
    if (!parsed?.accessToken) return null;
    // refreshToken 可能是 undefined（更早版本写进去的、或后端没下发）：当空串处理，
    // 刷新时会明确失败，而不是把 undefined 发出去换回一个 400。
    return { accessToken: parsed.accessToken, refreshToken: parsed.refreshToken ?? '' };
  } catch {
    return null;
  }
}

export function saveTokens(tokens: AuthTokens): void {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(tokens));
}

export function clearTokens(): void {
  localStorage.removeItem(STORAGE_KEY);
}

/**
 * 认证接口自身不能走「401 就刷新」那条路：登录失败是密码错，刷新失败是 refresh
 * token 也过期了，两者都不该再触发一次刷新，否则刷新接口自己 401 时就是死循环。
 */
export function isAuthEndpoint(url?: string): boolean {
  return !!url && /\/auth\/(login|refresh|logout)$/.test(url);
}

let inflight: Promise<AuthTokens> | null = null;

/**
 * 单飞刷新：一屏上多个请求同时收到 401 时（打开列表页很常见）只发一次刷新，
 * 其余请求等同一个 Promise，避免拿同一个 refresh token 并发换出多组 token
 * 再互相覆盖。
 */
export function refreshTokens(): Promise<AuthTokens> {
  if (!inflight) {
    inflight = doRefresh().finally(() => {
      inflight = null;
    });
  }
  return inflight;
}

async function doRefresh(): Promise<AuthTokens> {
  const current = readTokens();
  if (!current?.refreshToken) throw new Error('没有可用的 refresh token');
  const next = await requestRefresh(current.refreshToken);
  // 后端每次刷新会连 refresh token 一起换新（滑动窗口），必须整个覆盖存回去，
  // 只存 accessToken 的话下一个 7 天窗口就丢了。
  saveTokens(next);
  return next;
}
