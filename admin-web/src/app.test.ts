import React from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('@umijs/max', () => ({
  request: vi.fn(),
  history: { location: { pathname: '/dashboard' }, push: vi.fn() },
  useModel: vi.fn(),
}));
vi.mock('antd', () => ({ message: { error: vi.fn(), success: vi.fn() } }));
vi.mock('@ant-design/icons', () => ({ LockOutlined: 'LockOutlined', UserOutlined: 'UserOutlined' }));
vi.mock('@ant-design/pro-components', () => ({
  LoginForm: 'LoginForm', ProFormText: Object.assign(() => null, { Password: () => null }),
}));
vi.mock('./menuIcons', () => ({ renderMenuIcon: (name: string) => name }));
vi.mock('./services/user', () => ({ fetchCurrentUser: vi.fn(), login: vi.fn(), logout: vi.fn() }));

import { history, request, useModel } from '@umijs/max';
import { message } from 'antd';
import { getInitialState, layout, request as requestConfig } from './app';
import LoginPage from './pages/login';
import { fetchCurrentUser, login } from './services/user';

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
