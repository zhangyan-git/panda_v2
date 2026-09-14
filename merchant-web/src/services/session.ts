export const TOKEN_KEY = 'panda.auth.tokens';

export type Tokens = { accessToken: string; refreshToken: string };

export function isTokens(value: unknown): value is Tokens {
  if (!value || typeof value !== 'object') return false;
  const tokens = value as Partial<Tokens>;
  return typeof tokens.accessToken === 'string' && !!tokens.accessToken.trim()
    && typeof tokens.refreshToken === 'string' && !!tokens.refreshToken.trim();
}

export function clearSession() {
  localStorage.removeItem(TOKEN_KEY);
}

export function readTokens(): Tokens | undefined {
  const raw = localStorage.getItem(TOKEN_KEY);
  if (!raw) return undefined;
  try {
    const tokens: unknown = JSON.parse(raw);
    if (isTokens(tokens)) return tokens;
  } catch { /* 损坏的存储按未登录处理 */ }
  clearSession();
  return undefined;
}

export function saveTokens(tokens: unknown) {
  if (!isTokens(tokens)) throw new Error('登录响应无效，请重试');
  localStorage.setItem(TOKEN_KEY, JSON.stringify(tokens));
}

export function requestErrorMessage(error: unknown, fallback: string): string {
  const response = (error as { response?: { data?: { errorMessage?: string } } } | null)?.response;
  return response?.data?.errorMessage || (error instanceof Error ? error.message : fallback);
}
