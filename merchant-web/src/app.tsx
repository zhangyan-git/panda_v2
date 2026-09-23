import { DashboardOutlined, HddOutlined, ProfileOutlined, ShopOutlined } from '@ant-design/icons';
import { history, type RequestConfig } from '@umijs/max';
import { message } from 'antd';
import { scopeText } from './services/scopeText';
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
        // 只有两件事会清凭据：401（这枚令牌不认了），以及 /users/me 上的 403（账号被停用、
        // 商户被暂停——重新登录也进不来，但不该把用户留在半开的页面里）。**其余 403 一律
        // 不清**：商户域没有权限码，业务接口上的 403 只可能来自 realm/tenant 不符，清凭据
        // 解决不了，只会把一次可以重试的失败放大成一次登出。
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

/**
 * 侧栏菜单。
 *
 * 写死在这里，不向服务端要：商户域没有菜单表，商户账号也没有权限码（`migrations/009_drop_orphan_merchant_tables.sql` 删掉商户角色表
 * 之后，商户账号只有数据范围、没有角色），所以「这个账号能看见哪几项」在服务端没有答案。
 * 真正决定能看见什么的是**数据范围**——菜单项人人一样，点进去各自只看到自己范围内的行。
 *
 * 这个文件叫 app.tsx 而不是 app.ts，就是因为这一段：菜单项带着图标元素，而 umi 是拿
 * es-module-lexer 按**扩展名**解析这个入口的，`.ts` 里的 JSX 会让它直接解析失败
 * （`max setup` 报 `parse src/app.ts failed`），连类型检查都走不到。
 */
const MENUS = [
  { path: '/dashboard', name: '概览', icon: <DashboardOutlined /> },
  { path: '/stores', name: '门店', icon: <ShopOutlined /> },
  { path: '/devices', name: '设备', icon: <HddOutlined /> },
  { path: '/orders', name: '订单', icon: <ProfileOutlined /> },
];

export const layout = ({
  initialState,
}: {
  initialState?: { currentUser?: CurrentUser };
}) => ({
  // mix 模式才有顶部栏，右上角才会渲染头像/用户名/退出下拉（side 模式会渲染到侧栏底部）
  layout: 'mix' as const,
  navTheme: 'light' as const,
  menuDataRender: () =>
    MENUS.map((item) => ({ key: item.path, path: item.path, name: item.name, icon: item.icon })),
  // 「你的数据范围」放在侧栏底部：它是这个账号的天花板，界面上的每一行都被它框住，所以
  // 它必须常驻可见。用户多半是在「怎么少了几家店」的那一刻才想起还有这么个设置。
  menuFooterRender: (props: { collapsed?: boolean }) => {
    if (props?.collapsed) return undefined;
    const user = initialState?.currentUser;
    if (!user) return undefined;
    return (
      <div style={{ padding: '12px 16px', color: 'rgba(0,0,0,0.45)', fontSize: 12 }}>
        <div>数据范围：{scopeText(user)}</div>
        {user.merchantName ? <div>{user.merchantName}</div> : null}
      </div>
    );
  },
  onPageChange() {
    // 当前用户校验由 getInitialState 完成；布局挂载不能抢在异步恢复前跳转。
    if (!readTokens()) goToLogin();
  },
  // 服务端**没有** /v1/merchant/auth/logout（MerchantAuthService 只有 Login 与 Me，也没有
  // merchant_sessions 表），所以这里只清本地凭据。这不是缺的一半：access token 是 24 小时
  // 的 JWT，服务端本来就没有「作废某一枚」的能力，一个 logout 接口能做的也只有清客户端。
  logout: async () => {
    clearSession();
    history.replace(LOGIN_PATH);
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
