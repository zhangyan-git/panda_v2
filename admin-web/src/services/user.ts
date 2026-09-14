import { request } from '@umijs/max';

export type CurrentUser = {
  id: string;
  username: string;
  name: string;
  avatar?: string;
  roles: string[];
  permissions: string[];
};

/** 获取当前登录用户信息 */
export async function fetchCurrentUser(): Promise<CurrentUser> {
  return request<CurrentUser>('/api/v1/admin/users/me');
}

/** 登录 */
export async function login(params: { username: string; password: string }) {
  return request<{ accessToken: string; refreshToken: string }>(
    '/api/v1/admin/auth/login',
    {
      method: 'POST',
      data: params,
    },
  );
}

/** 退出登录 */
export async function logout() {
  return request('/api/v1/admin/auth/logout', {
    method: 'POST',
  });
}

/** 刷新 token */
export async function refreshToken(token: string) {
  return request<{ accessToken: string; refreshToken: string }>(
    '/api/v1/admin/auth/refresh',
    {
      method: 'POST',
      data: { refreshToken: token },
    },
  );
}
