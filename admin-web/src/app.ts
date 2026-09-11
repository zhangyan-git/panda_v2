import { history, type RequestConfig } from '@umijs/max';
import { renderMenuIcon } from './menuIcons';
import { loadMyMenus, type MenuNode } from './services/menu';
import { fetchCurrentUser, logout as logoutRequest } from './services/user';
import type { CurrentUser } from './services/user';

const LOGIN_PATH = '/login';

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
      const raw = localStorage.getItem('panda.auth.tokens');
      if (raw) {
        try {
          const { accessToken } = JSON.parse(raw) as { accessToken: string };
          config.headers = {
            ...(config.headers || {}),
            Authorization: `Bearer ${accessToken}`,
          };
        } catch { /* ignore */ }
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
    if (!initialState?.currentUser && !localStorage.getItem('panda.auth.tokens')) {
      history.push(LOGIN_PATH);
    }
  },
  // plugin-layout 内置的右上角下拉：hover 头像/用户名后显示「退出登录」
  logout: async () => {
    try {
      await logoutRequest();
    } finally {
      localStorage.removeItem('panda.auth.tokens');
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
  const raw = localStorage.getItem('panda.auth.tokens');
  if (!raw) return {};
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
    localStorage.removeItem('panda.auth.tokens');
    return {};
  }
}
