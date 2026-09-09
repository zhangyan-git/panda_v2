import { request } from '@umijs/max';

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

/** 完整菜单树（菜单管理页用，需要 admin:menus:read） */
export async function listMenuTree() {
  return request<MenuNode[]>('/api/v1/admin/menus');
}

/** 当前登录用户可见的菜单树（侧栏渲染用，仅需登录） */
export async function myMenuTree() {
  return request<MenuNode[]>('/api/v1/admin/menus/me');
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
