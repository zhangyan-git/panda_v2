import React from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('@umijs/max', () => ({
  request: vi.fn(),
  // 重放那条路要它：刷新成功之后拿同一个实例把原请求再发一次。
  getRequestInstance: vi.fn(() => Object.assign(vi.fn(), { defaults: {} }) as any),
  history: { location: { pathname: '/dashboard' }, push: vi.fn() },
  useModel: vi.fn(),
}));
vi.mock('antd', () => ({ message: { error: vi.fn(), success: vi.fn() } }));
vi.mock('@ant-design/icons', () => ({ LockOutlined: 'LockOutlined', UserOutlined: 'UserOutlined' }));
vi.mock('@ant-design/pro-components', () => ({
  LoginForm: 'LoginForm', ProFormText: Object.assign(() => null, { Password: () => null }),
}));
vi.mock('./menuIcons', () => ({ renderMenuIcon: (name: string) => name }));
vi.mock('./services/user', () => ({
  fetchCurrentUser: vi.fn(),
  login: vi.fn(),
  logout: vi.fn(),
  // 刷新走的是它。漏了这个 key 时 refreshTokens 调的是 undefined，抛出来的是 TypeError——
  // 那种「失败」与本用例要区分的几种失败长得一样，会把断言测成假绿。
  refreshToken: vi.fn(),
}));

import { history, request, useModel } from '@umijs/max';
import { message } from 'antd';
import { getInitialState, layout, request as requestConfig } from './app';
import LoginPage from './pages/login';
import { requestErrorCode, requestErrorMessage } from './services/requestError';
import { fetchCurrentUser, login, refreshToken } from './services/user';

const user = { id: 'new-user', username: 'operator', name: '新用户', roles: [], permissions: ['roles:read'] };
const tokens = { accessToken: 'test-access', refreshToken: 'test-refresh' };
const menus = [{ id: 'roles', parentId: '', name: '角色', path: '/roles', icon: '', sort: 0 }];

beforeEach(() => {
  vi.resetAllMocks();
  vi.stubGlobal('React', React);
  const storage = new Map<string, string>();
  vi.stubGlobal('localStorage', {
    getItem: (key: string) => storage.get(key) ?? null,
    setItem: (key: string, value: string) => storage.set(key, value),
    removeItem: (key: string) => storage.delete(key),
  });
  vi.mocked(fetchCurrentUser).mockResolvedValue(user);
});
afterEach(() => { vi.unstubAllGlobals(); });

describe('server-driven layout', () => {
  it('does not expose static routes for absent or empty menus', () => {
    expect(layout({}).menuDataRender()).toEqual([]);
    expect(layout({ initialState: { menus: [] } }).menuDataRender()).toEqual([]);
  });
  it('renders server entries and directory children', () => {
    const result = layout({ initialState: { menus: [{ ...menus[0], id: 'directory', path: '', children: menus }] } }).menuDataRender();
    expect(result[0]).not.toHaveProperty('path');
    expect(result[0].key).toBe('directory');
    expect(result[0].children[0]).toMatchObject({ key: 'roles', path: '/roles' });
  });
});

// 侧栏同时点亮两项的根因在 ProLayout 那一侧：它按 path 做**前缀**匹配，且默认把全部命中项
// 都当成选中项（`getMatchMenu(pathname, menuData, true)`）。`/orders` 因此既命中「订单列表」
// 也命中「咖啡订单」，两个同级菜单一起变灰。这一组把「只留最深的那一个」钉住，顺带钉住详情页
// 与目录节点两种边界。
//
// 样本用的是**库里真有的**一对同级菜单：目录「订单管理」下的 /orders 与 /orders/coffee
// （/orders/cup-sleeve、/orders/membership 也是同一形状）。支付域原来也有一对
// （/payments 与 /payments/methods），那一页删掉之后样本换到这里。
const ordersMenu = [
  { id: 'ord-dir', parentId: '', name: '订单管理', path: '', icon: '', sort: 1 },
  { id: 'ord-list', parentId: 'ord-dir', name: '订单列表', path: '/orders', icon: '', sort: 1 },
  { id: 'ord-coffee', parentId: 'ord-dir', name: '咖啡订单', path: '/orders/coffee', icon: '', sort: 2 },
];

describe('sidebar selection', () => {
  const at = (pathname: string, nodes: any[] = ordersMenu) => {
    (history as any).location = { pathname };
    return layout({ initialState: { menus: nodes } }).selectedKeys;
  };

  it('lights only the deeper entry when one path is a prefix of another', () => {
    expect(at('/orders/coffee')).toEqual(['ord-coffee']);
  });

  it('falls back to the list entry on its own page and on detail pages', () => {
    expect(at('/orders')).toEqual(['ord-list']);
    expect(at('/orders/ORD20260918120000000001')).toEqual(['ord-list']);
  });

  it('lights nothing for a directory node or an unknown path', () => {
    expect(at('/orders', [{ ...ordersMenu[0] }])).toEqual([]);
    expect(at('/not-in-the-menu')).toEqual([]);
  });

  it('finds a nested entry regardless of depth', () => {
    const nested = [{ ...ordersMenu[0], children: ordersMenu.slice(1) }];
    expect(at('/orders/coffee', nested as any)).toEqual(['ord-coffee']);
  });

  // react-router 的路由匹配默认不区分大小写（caseSensitive: false），敲 /ORDERS/COFFEE
  // 页面照常渲染。点亮如果按大小写敏感来，就会出现「页面出来了、侧栏一个都不亮」。
  it('matches regardless of case, like the router does', () => {
    expect(at('/ORDERS/COFFEE')).toEqual(['ord-coffee']);
    expect(at('/Orders')).toEqual(['ord-list']);
  });
});

// 面包屑与侧栏点亮是同一套前缀匹配的两种表现。ProLayout 默认按 URL 段切（`/orders` 与
// `/orders/coffee`），于是把**同级**的「订单列表」当成「咖啡订单」的父级。这一组钉住
// 「按菜单树的 parentId 拼」，顺带钉住目录不出链接、详情页回落、以及没有父级时不出面包屑。
describe('breadcrumb', () => {
  const crumbs = (pathname: string, nodes: any[] = ordersMenu) => {
    (history as any).location = { pathname };
    const render = layout({ initialState: { menus: nodes } }).breadcrumbRender as (items: any[]) => any[];
    return render([]);
  };
  const titles = (pathname: string, nodes: any[] = ordersMenu) => crumbs(pathname, nodes).map((i) => i.title);

  it('follows the menu tree instead of the URL segments', () => {
    // 按 URL 段切的话这一条会变成「订单列表 / 咖啡订单」——把同级当成了父级。
    expect(titles('/orders/coffee')).toEqual(['订单管理', '咖啡订单']);
  });

  it('keeps a genuine parent-child pair as a trail', () => {
    // 目录行的 path 是空串，与库里那八个目录一致；这条真层级靠 parentId 拼出来的
    const coupons = [
      { id: 'cpn', parentId: '', name: '优惠券管理', path: '', icon: '', sort: 1 },
      { id: 'cpn-types', parentId: 'cpn', name: '优惠券类型', path: '/coupons/types', icon: '', sort: 1 },
    ];
    expect(titles('/coupons/types', coupons)).toEqual(['优惠券管理', '优惠券类型']);
  });

  it('links ancestors but not the current page, and never links a directory', () => {
    expect(crumbs('/orders/coffee').map((i) => i.linkPath)).toEqual([undefined, '/orders/coffee']);
  });

  it('falls back to the list entry on detail pages', () => {
    expect(titles('/orders/ORD20260918120000000001')).toEqual(['订单管理', '订单列表']);
  });

  it('yields a single crumb for a top-level page, which ProLayout then hides', () => {
    // 只有一项时 ProLayout 的 breadcrumbProps.minLength（默认 2）会把整条藏掉，
    // 所以「概览」这种没有父级的页面保持今天的样子：不显示面包屑。
    const dashboard = [{ id: 'dash', parentId: '', name: '概览', path: '/dashboard', icon: '', sort: 0 }];
    expect(titles('/dashboard', dashboard)).toEqual(['概览']);
  });

  it('renders nothing when no menu entry matches the path', () => {
    expect(crumbs('/not-in-the-menu')).toEqual([]);
  });

  it('matches regardless of case, like the router does', () => {
    expect(titles('/ORDERS/COFFEE')).toEqual(['订单管理', '咖啡订单']);
  });
});

// 后端信封是 {success, data, errorCode, errorMessage, showType}。解包处曾经读的是
// body.message —— 那个字段根本不存在，于是后端所有措辞过的中文提示（「图片不能超过
// 10MB」「仅支持图片格式」）统统被吞成「请求失败」。这组用例把字段名钉死。
describe('envelope unwrapping', () => {
  const unwrap = (requestConfig.responseInterceptors as Array<(response: any) => any>)[0];

  it('passes non-envelope bodies through', () => {
    const response = { data: [1, 2, 3] };
    expect(unwrap(response)).toBe(response);
    expect(response.data).toEqual([1, 2, 3]);
  });

  it('replaces the envelope with its data on success', () => {
    const response = { data: { success: true, data: { id: 'store-1' }, errorCode: '', errorMessage: '' } };
    expect(unwrap(response)).toBe(response);
    expect(response.data).toEqual({ id: 'store-1' });
  });

  it('rejects with the backend errorMessage', async () => {
    await expect(
      unwrap({ data: { success: false, errorMessage: '图片不能超过 10MB', errorCode: 'INVALID_REQUEST' } }),
    ).rejects.toThrow('图片不能超过 10MB');
  });

  it('ignores a `message` field that the envelope does not have', async () => {
    await expect(
      unwrap({ data: { success: false, message: '不是信封里的字段名' } }),
    ).rejects.toThrow('请求失败');
  });

  // 抛出去的那个错误必须让 requestError.ts 读得到信封：两个辅助函数都是从
  // `error.response.data` 里取 errorMessage / errorCode 的（真实的 axios 错误本来就有这一层）。
  // 只抛一个裸 Error 时，上面那两条 toThrow 照样过——文案在 message 里——但所有按码分支的
  // 地方会静默走兜底那一支，而那是这两种写法唯一的区别。
  it('hands the envelope to the error helpers through error.response.data', async () => {
    const response = {
      status: 400,
      data: { success: false, errorMessage: '图片不能超过 10MB', errorCode: 'INVALID_REQUEST' },
    };
    const rejected = await unwrap(response).then(
      () => null,
      (error: unknown) => error,
    );
    expect(requestErrorMessage(rejected, '兜底')).toBe('图片不能超过 10MB');
    expect(requestErrorCode(rejected)).toBe('INVALID_REQUEST');
    expect((rejected as { response?: { status?: number } }).response?.status).toBe(400);
  });
});

// 「刷新失败」有三种，处置只有一种是对的：
//
//   401（refresh token 过期/吊销）  这枚凭据再也换不回 token 了 → 清凭据、回登录页
//   本机压根没有 refresh token      同上，留着也换不回来          → 清凭据、回登录页
//   网络错 / 网关 502 / 后端重启    凭据还好好的                  → **什么都不动**
//
// 最后一格是这组用例存在的理由：这里原来是个裸 `catch {}`，对任何失败都清凭据跳登录——后端
// 重启期间刷新页面，access token 其实还没过期，用户却已经被踢出去了。而判据放宽（只认 401）
// 不能推到头：真过期时不送回登录页，用户会卡在一个永远 401 的页面上。
describe('refresh failure', () => {
  const retry = (requestConfig.responseInterceptors as any[])[1][1] as (e: any) => Promise<any>;
  const request401 = () => ({ response: { status: 401 }, config: { url: '/api/v1/admin/users', headers: {} } });

  it('keeps credentials when the refresh call fails without a 401', async () => {
    localStorage.setItem('panda.auth.tokens', JSON.stringify(tokens));
    vi.mocked(refreshToken).mockRejectedValue(new Error('Network Error'));
    const error = request401();
    await expect(retry(error)).rejects.toBe(error);
    expect(localStorage.getItem('panda.auth.tokens')).toBe(JSON.stringify(tokens));
    expect(history.push).not.toHaveBeenCalled();
  });

  it('clears credentials when the refresh token itself is rejected', async () => {
    localStorage.setItem('panda.auth.tokens', JSON.stringify(tokens));
    vi.mocked(refreshToken).mockRejectedValue({ response: { status: 401 } });
    await expect(retry(request401())).rejects.toMatchObject({ response: { status: 401 } });
    expect(localStorage.getItem('panda.auth.tokens')).toBeNull();
    expect(history.push).toHaveBeenCalledWith('/login');
  });

  it('clears credentials when there is no refresh token to send at all', async () => {
    // 更早版本写进去的凭据只有 accessToken：换不回来，也就没必要赖在这一页上。
    localStorage.setItem('panda.auth.tokens', JSON.stringify({ accessToken: 'test-access' }));
    await expect(retry(request401())).rejects.toBeTruthy();
    expect(localStorage.getItem('panda.auth.tokens')).toBeNull();
    expect(history.push).toHaveBeenCalledWith('/login');
    expect(refreshToken).not.toHaveBeenCalled();
  });
});

// 全仓原来没有任何客户端超时（axios 的默认值是 0）。网关自己卡住时请求既没有响应也不会断连，
// 页面就一直转圈。这一条钉住「每个请求都被安上超时」以及「调用方自己设的不会被覆盖」。
//
// 它必须在**拦截器**里生效，不能在模块顶层写 `getRequestInstance().defaults.timeout = …`：
// 那行会在 app.ts 求值时跑，那时 umi 的 pluginManager 还没装上，整站白屏——而 tsc 与这一份
// 单测都看不出来（这里 getRequestInstance 是 mock 的）。浏览器里才会现形。
describe('request timeout', () => {
  const intercept = (requestConfig.requestInterceptors as any[])[0];

  it('gives every request a timeout so a hung gateway cannot spin forever', () => {
    expect(intercept({ url: '/api/v1/admin/users', headers: {} }).timeout).toBe(180_000);
  });

  it('keeps a timeout the caller set explicitly', () => {
    expect(intercept({ url: '/api/v1/admin/users', headers: {}, timeout: 5_000 }).timeout).toBe(5_000);
  });
});

describe('cold-start menu loading', () => {
  it.each([{ nodes: [] }, { nodes: menus }])('keeps valid server menus: %j', async ({ nodes }) => {
    localStorage.setItem('panda.auth.tokens', JSON.stringify(tokens));
    vi.mocked(request).mockResolvedValueOnce(nodes);
    expect(await getInitialState()).toMatchObject({ currentUser: user, permissions: user.permissions, menus: nodes });
    expect(request).toHaveBeenCalledTimes(1);
    expect(message.error).not.toHaveBeenCalled();
  });
  it('keeps identity and credentials after menu failure', async () => {
    localStorage.setItem('panda.auth.tokens', JSON.stringify(tokens));
    vi.mocked(request).mockRejectedValue(new Error('menu unavailable'));
    expect(await getInitialState()).toMatchObject({ currentUser: user, permissions: user.permissions, menus: [] });
    expect(request).toHaveBeenCalledTimes(2);
    expect(message.error).toHaveBeenCalledTimes(1);
    expect(localStorage.getItem('panda.auth.tokens')).toBe(JSON.stringify(tokens));
  });
});

describe('login replaces previous identity menus', () => {
  it.each([
    { nodes: [], failed: false },
    { nodes: menus, failed: false },
    { nodes: [], failed: true },
  ])('overwrites previous menus without a page reload: %j', async ({ nodes, failed }) => {
    let state: any = { currentUser: { id: 'previous-user' }, permissions: ['old-permission'], menus, name: '旧用户', avatar: 'old-avatar' };
    const setInitialState = vi.fn(async (update) => { state = update(state); });
    vi.mocked(useModel).mockReturnValue({ setInitialState } as any);
    vi.mocked(login).mockResolvedValue(tokens);
    if (failed) vi.mocked(request).mockRejectedValue(new Error('menu unavailable'));
    else vi.mocked(request).mockResolvedValueOnce(nodes);
    const page = LoginPage({}) as React.ReactElement<any>;
    await page.props.children.props.onFinish({ username: 'operator', password: 'test-only' });
    expect(state).toMatchObject({ currentUser: user, permissions: user.permissions, menus: nodes, name: user.name, avatar: false });
    expect(setInitialState).toHaveBeenCalledTimes(1);
    expect(history.push).toHaveBeenCalledWith('/dashboard');
    expect(localStorage.getItem('panda.auth.tokens')).toBe(JSON.stringify(tokens));
    expect(message.error).toHaveBeenCalledTimes(failed ? 1 : 0);
    expect(message.success).toHaveBeenCalledWith('登录成功');
  });
});
