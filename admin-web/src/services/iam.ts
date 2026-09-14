import { request } from '@umijs/max';
import type { PageQuery, PageResult } from './pagination';

// ——— 类型 ———

export type Role = {
  id: string;
  code: string;
  name: string;
  description: string;
  createdAt: string;
};

export type Permission = {
  id: string;
  code: string;
  name: string;
  description: string;
  group: string;
  createdAt: string;
};

export type AdminUser = {
  id: string;
  username: string;
  name: string;
  email: string;
  status: 'active' | 'disabled';
  createdAt: string;
};

// ——— 权限 ———

/** 权限列表（服务端分页）。角色授权弹窗要全集，见 FULL_PAGE_PARAMS。 */
export async function listPermissions(params?: PageQuery) {
  return request<PageResult<Permission>>('/api/v1/admin/permissions', { params });
}

export async function createPermission(data: {
  code: string;
  name: string;
  description?: string;
  group?: string;
}) {
  return request<Permission>('/api/v1/admin/permissions', {
    method: 'POST',
    data,
  });
}

export async function updatePermission(
  id: string,
  data: { code: string; name: string; description?: string; group?: string },
) {
  return request<Permission>(`/api/v1/admin/permissions/${id}`, {
    method: 'PUT',
    data,
  });
}

export async function deletePermission(id: string) {
  return request(`/api/v1/admin/permissions/${id}`, { method: 'DELETE' });
}

// ——— 角色 ———

/** 角色列表（服务端分页）。用户授权弹窗要全集，见 FULL_PAGE_PARAMS。 */
export async function listRoles(params?: PageQuery) {
  return request<PageResult<Role>>('/api/v1/admin/roles', { params });
}

export async function createRole(data: {
  code: string;
  name: string;
  description?: string;
}) {
  return request<Role>('/api/v1/admin/roles', { method: 'POST', data });
}

export async function updateRole(
  id: string,
  data: { code: string; name: string; description?: string },
) {
  return request<Role>(`/api/v1/admin/roles/${id}`, { method: 'PUT', data });
}

export async function deleteRole(id: string) {
  return request(`/api/v1/admin/roles/${id}`, { method: 'DELETE' });
}

export async function listRolePermissions(roleId: string) {
  return request<Permission[]>(`/api/v1/admin/roles/${roleId}/permissions`);
}

export async function assignPermissionsToRole(roleId: string, permissionIds: string[]) {
  return request(`/api/v1/admin/roles/${roleId}/permissions`, {
    method: 'POST',
    data: { permissionIds },
  });
}

export async function removePermissionFromRole(roleId: string, permissionId: string) {
  return request(`/api/v1/admin/roles/${roleId}/permissions/${permissionId}`, {
    method: 'DELETE',
  });
}

// ——— 管理员用户 ———

/** 平台管理员列表（服务端分页）。 */
export async function listAdminUsers(params?: PageQuery) {
  return request<PageResult<AdminUser>>('/api/v1/admin/users', { params });
}

export async function createAdminUser(data: {
  username: string;
  password: string;
  name?: string;
  email?: string;
}) {
  return request<AdminUser>('/api/v1/admin/users', { method: 'POST', data });
}

export async function updateAdminUserStatus(id: string, status: 'active' | 'disabled') {
  return request(`/api/v1/admin/users/${id}/status`, {
    method: 'PATCH',
    data: { status },
  });
}

export async function listUserRoles(userId: string) {
  return request<Role[]>(`/api/v1/admin/users/${userId}/roles`);
}

export async function assignRolesToUser(userId: string, roleIds: string[]) {
  return request(`/api/v1/admin/users/${userId}/roles`, {
    method: 'POST',
    data: { roleIds },
  });
}

export async function removeRoleFromUser(userId: string, roleId: string) {
  return request(`/api/v1/admin/users/${userId}/roles/${roleId}`, { method: 'DELETE' });
}
