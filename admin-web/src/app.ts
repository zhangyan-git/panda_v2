import { getRequestInstance, history, type RequestConfig } from '@umijs/max';
import { renderMenuIcon } from './menuIcons';
import { loadMyMenus, type MenuNode } from './services/menu';
import { fetchCurrentUser, logout as logoutRequest } from './services/user';
import type { CurrentUser } from './services/user';
import { clearTokens, isAuthEndpoint, readTokens, refreshTokens } from './services/token';

const LOGIN_PATH = '/login';

/**
 * 401 → 用 refresh token 换新 → 重放原请求，对调用方完全透明。
 *
 * 为什么放在 responseInterceptors 的失败回调里，而不是 errorConfig.errorHandler：
 * umi 的 errorHandler 只是「通知」（生成的 request.ts 里调用完仍然 reject(error)），
 * 它的返回值不会被用上，做不到「换了 token 接着把原请求跑完」。而 axios 响应拦截器
 * 的 rejected 回调返回一个 Promise 时，整条链就换成这个 Promise 的结果。
 */
// 返回类型写 Promise<any> 是被 umi 的类型逼的：它把失败回调定成
// `(error: Error) => Promise<Error>`（见 .umi/plugin-request/request.ts:118），
// 表达不了「重放成功就 resolve」这件事。axios 运行时是支持的——失败回调返回一个
// Promise 就用它的结果替换整条链——所以这里放宽类型，而不是削掉重放能力。
async function retryAfterRefresh(error: any): Promise<any> {
  const config = error?.config;
  // 只有 401 表示「这枚 token 不认了」。403 是权限不足，刷新完还是 403。
  if (error?.response?.status !== 401 || !config) return Promise.reject(error);
  // 登录/刷新接口自己的 401（密码错、或 refresh token 也过期）不能再触发刷新，
  // 否则刷新接口 401 时会自己调自己。
  if (isAuthEndpoint(config.url)) return Promise.reject(error);
  // 重放后的请求又 401 就不再刷新，避免「刷新 → 重放 → 401 → 刷新」转不完。
  if (config.__pandaRetried) return Promise.reject(error);
  config.__pandaRetried = true;

  try {
    await refreshTokens();
  } catch {
    // refresh token 也过期了：清干净回登录页，与 getInitialState 失败时的处理一致。
    clearTokens();
    if (history.location.pathname !== LOGIN_PATH) history.push(LOGIN_PATH);
    return Promise.reject(error);
  }
  // 重放走同一个 axios 实例。不用手动改 Authorization：请求拦截器每次派发都会
  // 从 localStorage 重新读一次，这时它已经是刷新后的那枚。
  return getRequestInstance()(config);
}

/** 用姓名首字生成内联 SVG 头像，避免依赖外部 CDN */
function initialsAvatar(name: string): string {
  const initial = (name || '?').trim().charAt(0).toUpperCase();
  const svg =
    `<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64">` +
    `<rect width="64" height="64" rx="32" fill="#1677ff"/>` +
    `<text x="32" y="43" font-size="28" fill="#fff" text-anchor="middle" font-family="sans-serif">${initial}</text>` +
    `</svg>`;
  return `data:image/svg+xml;utf8,${encodeURIComponent(svg)}`;
}

export const request: RequestConfig = {
  requestInterceptors: [
    (config: any) => {
      const tokens = readTokens();
      if (tokens) {
        config.headers = {
          ...(config.headers || {}),
          Authorization: `Bearer ${tokens.accessToken}`,
        };
      }
      return config;
    },
  ],
  responseInterceptors: [
    (response: any) => {
      // 解包后端统一信封 {success, data, errorCode, errorMessage, showType}
      const body = response.data;
      if (body && typeof body === 'object' && 'success' in body) {
        if (!body.success) {
          // 字段名是 errorMessage：读 body.message 的话这里永远是 undefined，
          // 后端所有措辞过的中文提示（「文件超过 10MB」「非图片格式」）都会被吞成
          // 「请求失败」。requestError.ts 里那几个辅助函数读的也是这个字段名。
          return Promise.reject(new Error(body.errorMessage || '请求失败'));
        }
        response.data = body.data;
      }
      return response;
    },
    // 元组形式 = axios 的 [onFulfilled, onRejected]。成功分支必须是恒等函数：
    // 上面那个拦截器已经把信封拆过了，这里只是把失败分支挂上去。
    [(response: any) => response, retryAfterRefresh],
  ],
};

/** 服务端菜单树 → ProLayout menuData；path 为空的节点是目录，仅分组不跳转 */
function toMenuData(nodes: MenuNode[]): any[] {
  return nodes.map((n) => ({
    // key 必须稳定且存在：目录节点没有 path，缺 key 会被 antd Menu 丢弃
    key: n.id,
    ...(n.path ? { path: n.path } : {}),
    name: n.name,
    icon: renderMenuIcon(n.icon),
    // 子级必须写 children：menuDataRender 的产物会再过一次 transformRoute，
    // 其过滤只认 children/path，写 routes 的无 path 目录会被整棵丢弃
    children: n.children?.length ? toMenuData(n.children) : undefined,
  }));
}

export const layout = ({
  initialState,
}: {
  initialState?: { currentUser?: CurrentUser; menus?: MenuNode[] };
}) => ({
  // mix 模式才有顶部栏，右上角才会渲染头像/用户名/退出下拉（side 模式会渲染到侧栏底部）
  layout: 'mix' as const,
  navTheme: 'light' as const,
  // 始终以服务端结果为准，包括合法空数组；失败时也不显示静态入口。
  menuDataRender: () => toMenuData(initialState?.menus ?? []),
  onPageChange() {
    const { location } = history;
    if (location.pathname === LOGIN_PATH) return;
    // 优先检查 initialState，其次检查 localStorage 是否有 token（登录后 setInitialState 可能还未同步）
    if (!initialState?.currentUser && !readTokens()) {
      history.push(LOGIN_PATH);
    }
  },
  // plugin-layout 内置的右上角下拉：hover 头像/用户名后显示「退出登录」
  logout: async () => {
    try {
      await logoutRequest();
    } finally {
      clearTokens();
      history.push(LOGIN_PATH);
    }
  },
});

export async function getInitialState(): Promise<{
  currentUser?: CurrentUser;
  permissions?: string[];
  menus?: MenuNode[];
  name?: string;
  avatar?: string | false;
}> {
  if (!readTokens()) return {};
  try {
    const currentUser = await fetchCurrentUser();
    const name = currentUser.name || currentUser.username;
    const menus = await loadMyMenus();
    return {
      currentUser,
      permissions: currentUser.permissions ?? [],
      menus,
      // plugin-layout 默认右上角渲染读取顶层 name / avatar
      name,
      avatar: initialsAvatar(name),
    };
  } catch {
    // access token 过期且刷新失败时，拦截器已经清过一遍；这里兜的是
    // loadMyMenus 之类的其他失败，一并当登录态不可用处理。
    clearTokens();
    return {};
  }
}
