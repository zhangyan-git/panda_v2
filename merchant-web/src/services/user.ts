import { request } from '@umijs/max';

export type CurrentUser = {
  id: string;
  username: string;
  name: string;
  avatar?: string;
  merchantId: string;
  roles: string[];
};

/** 获取当前登录商户用户信息 */
export async function fetchCurrentUser(): Promise<CurrentUser> {
  return request<CurrentUser>('/api/v1/merchant/users/me');
}

/** 登录 */
export async function login(params: { username: string; password: string }) {
  return request<{ accessToken: string; refreshToken: string }>(
    '/api/v1/merchant/auth/login',
    {
      method: 'POST',
      data: params,
    },
  );
}

/** 刷新 token */
export async function refreshToken(token: string) {
  return request<{ accessToken: string; refreshToken: string }>(
    '/api/v1/merchant/auth/refresh',
    {
      method: 'POST',
      data: { refreshToken: token },
    },
  );
}
