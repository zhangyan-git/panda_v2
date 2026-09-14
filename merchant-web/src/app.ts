import { history, type RequestConfig } from '@umijs/max';
import { message } from 'antd';
import { clearSession, readTokens, requestErrorMessage } from './services/session';
import { fetchCurrentUser, type CurrentUser } from './services/user';

const LOGIN_PATH = '/login';

function goToLogin() {
  if (history.location.pathname !== LOGIN_PATH) history.replace(LOGIN_PATH);
}

export const request: RequestConfig = {
  requestInterceptors: [
    (config: any) => {
      const tokens = readTokens();
      if (tokens && config.url?.startsWith('/api/v1/merchant/')) {
        config.headers = {
          ...config.headers,
          Authorization: `Bearer ${tokens.accessToken}`,
        };
      }
      return config;
    },
  ],
  responseInterceptors: [
    [
      (response: any) => {
        const body = response.data;
        if (body && typeof body === 'object' && 'success' in body) {
          if (!body.success) throw new Error(body.errorMessage || '请求失败');
          response.data = body.data;
        }
        return response;
      },
      (error) => {
        const response = (error as { response?: { status?: number; config?: { url?: string } } }).response;
        const isMe = response?.config?.url?.split('?')[0] === '/api/v1/merchant/users/me';
        if (response?.status === 401 || (response?.status === 403 && isMe)) {
          clearSession();
          goToLogin();
        }
        return Promise.reject(error);
      },
    ],
  ],
};

export const layout = () => ({
  onPageChange() {
    // 当前用户校验由 getInitialState 完成；布局挂载不能抢在异步恢复前跳转。
    if (!readTokens()) goToLogin();
  },
});

export async function getInitialState(): Promise<{ currentUser?: CurrentUser }> {
  if (!readTokens()) return {};
  try {
    const currentUser = await fetchCurrentUser();
    return { currentUser };
  } catch (error) {
    // 临时服务故障不删除凭据，但未验证身份时不展示受保护页面。
    if (readTokens()) message.error(requestErrorMessage(error, '获取当前用户失败，请重试'));
    goToLogin();
    return {};
  }
}
