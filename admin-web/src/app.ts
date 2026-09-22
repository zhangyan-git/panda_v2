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

/**
 * 当前路径命中的菜单项——最深的那一个。命中规则是**前缀**（`/orders` 命中 `/orders/coffee`），
 * 与 ProLayout 一致；「取最深」是下面 selectedMenuKeys 与面包屑共用的那一步。
 *
 * 大小写不敏感：react-router 的路由匹配默认就是 `caseSensitive: false`，敲 `/ORDERS/COFFEE`
 * 页面照常渲染；这里跟着它走，否则会出现「页面出来了，侧栏不点亮、面包屑也不显示」。ProLayout
 * 原来那套匹配（`@umijs/route-utils` 的 path-to-regexp，默认 `i` 标志）也是不敏感的，这是对齐。
 *
 * 空 path 的目录（只分组不跳转）不参与命中；`/payments/PAY123` 这种详情页落到 `/payments`
 * 那一项，因为没有任何菜单 path 比它更长。
 */
function matchMenuNode(nodes: MenuNode[], pathname: string): MenuNode | undefined {
  const path = pathname.toLowerCase();
  let deepest: MenuNode | undefined;
  const walk = (list: MenuNode[]) => {
    for (const node of list) {
      const candidate = node.path.toLowerCase();
      if (candidate && (path === candidate || path.startsWith(`${candidate}/`))) {
        if (!deepest || candidate.length > deepest.path.length) deepest = node;
      }
      if (node.children?.length) walk(node.children);
    }
  };
  walk(nodes);
  return deepest;
}

/**
 * 当前路径点亮哪一项菜单——**只留最深的那一个**，返回菜单项的 id。
 *
 * 为什么需要自己算：ProLayout 内部用的是 `getMatchMenu(pathname, menuData, true)`
 * （`@ant-design/pro-layout/es/ProLayout.js:268`），第三个参数的意思是「把**所有**命中的
 * key 都展示出来」，而匹配是按 path 做前缀的——`/orders` 会命中 `/orders/coffee`。
 * 于是「订单列表」和「咖啡订单」这两个**同级**菜单项会同时点亮（见 identity/018、019）。
 *
 * 传 `selectedKeys` 就是接管这件事（BaseMenu 把它当受控值用）。**只影响点亮**：目录的展开
 * 仍归 ProLayout（openKeys 用的是它自己算的全部命中项），所以父级目录照常自动展开。
 *
 * 路径读 `history.location` 而不是某个 state：umi 的 plugin-layout 在**每次渲染**时重新调用
 * 本函数（`.umi/plugin-layout/Layout.tsx` 里 applyPlugins 就在组件体内），而 Layout 订阅了
 * location，所以每次导航这里都会拿到新路径。
 */
function selectedMenuKeys(nodes: MenuNode[], pathname: string): string[] {
  const node = matchMenuNode(nodes, pathname);
  return node ? [node.id] : [];
}

/** 面包屑一项：字段名跟 ProLayout 的 defaultItemRender 走，不是 antd Breadcrumb 的 items 形状 */
type BreadcrumbItem = { linkPath?: string; breadcrumbName: string; title: string };

/**
 * 面包屑的层级——按**菜单树的父子关系**拼，不按 URL 段。
 *
 * ProLayout 那套默认是按 URL 段切的（`urlToList('/orders/coffee')` → `/orders` + `/orders/coffee`），
 * 再拿每一段去面包屑表里查名字。可 `/orders/coffee` 与 `/orders` 在菜单里是**同级**（都挂在
 * 「订单管理」下），于是显出「订单列表 / 咖啡订单」——把同级当成了父级。菜单树里本来就写着
 * 谁是谁的父亲（parentId），照它拼
 * 才是真层级：目录 → … → 当前页。
 *
 * `title` / `breadcrumbName` / `linkPath` 这些字段名是给 umi 生成的那份 `itemRender` 读的
 * （`src/.umi/plugin-layout/Layout.tsx`：把当前项与最后一项的路径比一比，相同就渲染成纯文本、
 * 不同就渲染成可点的 `<Link to={path}>`）。ProLayout 自带的 defaultItemRender 在这里走不到——
 * 那个只在调用方没给 itemRender 时才用，而 umi 每次都自带一份。结果：最后一项（当前位置）是
 * 纯文本，目录项没有 path 可比也落到纯文本；将来若有叶子当了父级，那一项才会渲染成可点的链接。
 *
 * 返回空数组等于「没有面包屑」：ProLayout 的 minLength 默认是 2，不足两项时不渲染——所以
 * 没有父级的顶级页（如「概览」）保持今天的样子，不显示。
 */
function breadcrumbItems(nodes: MenuNode[], pathname: string): BreadcrumbItem[] {
  const node = matchMenuNode(nodes, pathname);
  if (!node) return [];
  const byId = new Map<string, MenuNode>();
  const index = (list: MenuNode[]) => {
    for (const n of list) {
      byId.set(n.id, n);
      if (n.children?.length) index(n.children);
    }
  };
  index(nodes);
  const chain: MenuNode[] = [];
  // includes 是兜环的：菜单表里 parent_id 万一成环，这里不能转不完
  let cur: MenuNode | undefined = node;
  while (cur && !chain.includes(cur)) {
    chain.unshift(cur);
    cur = cur.parentId ? byId.get(cur.parentId) : undefined;
  }
  return chain.map((n) => ({ linkPath: n.path || undefined, breadcrumbName: n.name, title: n.name }));
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
  selectedKeys: selectedMenuKeys(initialState?.menus ?? [], history.location.pathname),
  breadcrumbRender: () => breadcrumbItems(initialState?.menus ?? [], history.location.pathname),
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
  } catch (error) {
    // 只有「这枚 access token 不认了」才清凭据，判据与请求层一致：retryAfterRefresh 只对
    // 401 动手（403 是权限不足，刷新也救不了；5xx 与网络错是后端自己的事），非 401 时它
    // 刻意把凭据留在原地。这里曾经对**任何**错误都 clearTokens——后端重启期间刷新页面，
    // 请求全部失败而 access token 其实还没过期，用户却被无条件踢回登录页。
    //
    // 401 走到这里时拦截器已经清过一遍（refresh 失败那条路），再清一次是幂等的兜底。
    //
    // 这句注释原先写「兜的是 loadMyMenus 之类的其他失败」——那句不成立：loadMyMenus 自己
    // 吞掉异常返回 []，菜单失败根本走不到这个 catch。能落到这里的只有 fetchCurrentUser 失败
    // （以及下面那一步意外抛出的情况）。
    if ((error as { response?: { status?: number } } | null)?.response?.status === 401) {
      clearTokens();
    }
    return {};
  }
}
