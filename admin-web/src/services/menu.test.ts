import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('@umijs/max', () => ({ request: vi.fn() }));
vi.mock('antd', () => ({ message: { error: vi.fn() } }));

import { request } from '@umijs/max';
import { message } from 'antd';
import { loadMyMenus, type MenuNode } from './menu';

const menus: MenuNode[] = [
  { id: 'menu', parentId: '', name: '角色', path: '/roles', icon: '', sort: 0 },
];

beforeEach(() => { vi.resetAllMocks(); });

describe('current-user menu loading', () => {
  it.each([{ nodes: [] }, { nodes: menus }])('accepts the server result without retry or warning: %j', async ({ nodes }) => {
    vi.mocked(request).mockResolvedValueOnce(nodes);
    expect(await loadMyMenus()).toEqual(nodes);
    expect(request).toHaveBeenCalledTimes(1);
    expect(request).toHaveBeenCalledWith('/api/v1/admin/menus/me');
    expect(message.error).not.toHaveBeenCalled();
  });

  it.each([{ nodes: [] }, { nodes: menus }])('retries once and accepts the next result: %j', async ({ nodes }) => {
    vi.mocked(request).mockRejectedValueOnce(new Error('temporary failure')).mockResolvedValueOnce(nodes);
    expect(await loadMyMenus()).toEqual(nodes);
    expect(request).toHaveBeenCalledTimes(2);
    expect(message.error).not.toHaveBeenCalled();
  });

  it('hides menus and reports one error after both attempts fail', async () => {
    vi.mocked(request).mockRejectedValue(new Error('unavailable'));
    expect(await loadMyMenus()).toEqual([]);
    expect(request).toHaveBeenCalledTimes(2);
    expect(message.error).toHaveBeenCalledTimes(1);
    expect(message.error).toHaveBeenCalledWith('菜单加载失败，已隐藏菜单，请稍后刷新重试');
  });
});
