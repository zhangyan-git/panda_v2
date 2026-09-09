// ---- 响应格式（对齐后端 platform/api/envelope.go）----

/** 单资源或操作成功的响应结构 */
export type ApiResponse<T> = {
  success: boolean;
  data?: T;
  errorCode?: string;
  errorMessage?: string;
  /** 0=静默 1=消息条 2=通知 4=错误页 */
  showType?: number;
};

/** 列表接口的 data 字段结构（对应后端 PageData）*/
export type PageData<T> = {
  list: T[];
  total: number;
};

export type PageResponse<T> = ApiResponse<PageData<T>>;

// ---- Auth ----

export type AuthTokens = { accessToken: string; refreshToken: string };
export type LoginRequest = { username: string; password: string };
export type RefreshRequest = { refreshToken: string };
export type AuthResponse = AuthTokens;

// ---- 错误类 ----

export class ApiError extends Error {
  /** HTTP 状态码 */
  readonly status: number;
  /** 后端业务错误码字符串，如 "UNAUTHORIZED" */
  readonly errorCode: string;
  readonly data: unknown;

  constructor(message: string, status: number, errorCode = String(status), data: unknown = null) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.errorCode = errorCode;
    this.data = data;
  }
}

// ---- Token 存储 ----

export interface TokenStorage {
  get(): AuthTokens | null;
  set(tokens: AuthTokens): void;
  clear(): void;
}

export class MemoryTokenStorage implements TokenStorage {
  private tokens: AuthTokens | null = null;
  get() { return this.tokens; }
  set(tokens: AuthTokens) { this.tokens = tokens; }
  clear() { this.tokens = null; }
}

export class LocalStorageTokenStorage implements TokenStorage {
  constructor(
    private readonly key = 'panda.auth.tokens',
    private readonly storage: Storage | undefined = globalThis.localStorage,
  ) {}

  get(): AuthTokens | null {
    if (!this.storage) return null;
    const value = this.storage.getItem(this.key);
    if (!value) return null;
    try { return JSON.parse(value) as AuthTokens; } catch { return null; }
  }

  set(tokens: AuthTokens) { this.storage?.setItem(this.key, JSON.stringify(tokens)); }
  clear() { this.storage?.removeItem(this.key); }
}

// ---- HTTP 客户端 ----

type ClientOptions = {
  baseUrl?: string;
  fetch?: typeof globalThis.fetch;
  storage?: TokenStorage;
  onRefresh?: (refreshToken: string) => Promise<AuthTokens>;
};

export class ApiClient {
  private readonly request: typeof fetch;
  private refreshing?: Promise<AuthTokens>;

  constructor(private readonly options: ClientOptions = {}) {
    this.request = options.fetch ?? globalThis.fetch.bind(globalThis);
  }

  async get<T>(path: string, init?: RequestInit): Promise<T> {
    return this.send<T>(path, { ...init, method: 'GET' });
  }
  async post<T>(path: string, body?: unknown, init?: RequestInit): Promise<T> {
    return this.send<T>(path, { ...init, method: 'POST', body: JSON.stringify(body) });
  }
  async put<T>(path: string, body?: unknown): Promise<T> {
    return this.send<T>(path, { method: 'PUT', body: JSON.stringify(body) });
  }
  async patch<T>(path: string, body?: unknown): Promise<T> {
    return this.send<T>(path, { method: 'PATCH', body: JSON.stringify(body) });
  }
  async delete<T>(path: string): Promise<T> {
    return this.send<T>(path, { method: 'DELETE' });
  }

  private async send<T>(path: string, init: RequestInit, retry = true): Promise<T> {
    const tokens = this.options.storage?.get();
    const headers = new Headers(init.headers);
    headers.set('Accept', 'application/json');
    if (init.body) headers.set('Content-Type', 'application/json');
    if (tokens) headers.set('Authorization', `Bearer ${tokens.accessToken}`);

    const response = await this.request(`${this.options.baseUrl ?? ''}${path}`, { ...init, headers });

    // 401 时尝试刷新一次
    if (response.status === 401 && retry && tokens?.refreshToken && this.options.onRefresh) {
      try {
        this.refreshing ??= this.options.onRefresh(tokens.refreshToken);
        const next = await this.refreshing;
        this.refreshing = undefined;
        this.options.storage?.set(next);
        return this.send<T>(path, init, false);
      } catch (error) {
        this.refreshing = undefined;
        this.options.storage?.clear();
        throw error;
      }
    }

    let payload: ApiResponse<T> | null = null;
    try { payload = await response.json() as ApiResponse<T>; } catch { /* non-JSON */ }

    if (!response.ok || !payload || !payload.success) {
      throw new ApiError(
        payload?.errorMessage ?? response.statusText,
        response.status,
        payload?.errorCode,
        payload?.data,
      );
    }
    return payload.data as T;
  }
}
