import { request } from '@umijs/max';
import { message } from 'antd';

// ——— 类型 ———

export type MenuNode = {
  id: string;
  parentId: string;
  name: string;
  path: string;
  icon: string;
  sort: number;
  children?: MenuNode[];
};

export type MenuInput = {
  parentId?: string;
  name: string;
  path?: string;
  icon?: string;
  sort?: number;
};

// ——— 菜单树 ———

/** 完整菜单树（菜单管理页用，需要 admin:menus:view） */
export async function listMenuTree() {
  return request<MenuNode[]>('/api/v1/admin/menus');
}

/** 当前登录用户可见的菜单树（侧栏渲染用，仅需登录） */
export async function myMenuTree() {
  return request<MenuNode[]>('/api/v1/admin/menus/me');
}

/** 空数组是有效结果；请求失败仅重试一次，最终隐藏菜单但不影响登录身份。 */
export async function loadMyMenus(): Promise<MenuNode[]> {
  try {
    return await myMenuTree();
  } catch {
    try {
      return await myMenuTree();
    } catch {
      message.error('菜单加载失败，已隐藏菜单，请稍后刷新重试');
      return [];
    }
  }
}

export async function createMenu(data: MenuInput) {
  return request<MenuNode>('/api/v1/admin/menus', { method: 'POST', data });
}

export async function updateMenu(id: string, data: MenuInput) {
  return request<MenuNode>(`/api/v1/admin/menus/${id}`, { method: 'PUT', data });
}

export async function deleteMenu(id: string) {
  return request(`/api/v1/admin/menus/${id}`, { method: 'DELETE' });
}

// ——— 角色-菜单绑定 ———

export async function listRoleMenus(roleId: string) {
  return request<string[]>(`/api/v1/admin/roles/${roleId}/menus`);
}

export async function assignMenusToRole(roleId: string, menuIds: string[]) {
  return request(`/api/v1/admin/roles/${roleId}/menus`, {
    method: 'POST',
    data: { menuIds },
  });
}
