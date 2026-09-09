import type { CurrentUser } from './services/user';

/**
 * Umi Max access 插件
 * 根据 /users/me 返回的 permissions（库中实时绑定）控制页面/按钮可见性
 *
 * 用法（路由配置）：
 *   { path: '/roles', access: 'canViewRoles' }
 *
 * 用法（组件内）：
 *   const { canWriteRoles } = useAccess();
 *   {canWriteRoles && <Button>新增角色</Button>}
 */
export default function access(initialState: {
  currentUser?: CurrentUser;
  permissions?: string[];
}) {
  const { permissions = [] } = initialState ?? {};
  const has = (code: string) => permissions.includes(code);

  return {
    // 角色管理
    canViewRoles:   has('admin:roles:read'),
    canWriteRoles:  has('admin:roles:write'),
    canDeleteRoles: has('admin:roles:delete'),

    // 权限管理
    canViewPermissions:   has('admin:permissions:read'),
    canWritePermissions:  has('admin:permissions:write'),
    canDeletePermissions: has('admin:permissions:delete'),

    // 绑定管理（角色分配权限 / 用户分配角色）
    canViewBindings:  has('admin:bindings:read'),
    canWriteBindings: has('admin:bindings:write'),

    // 管理员用户管理
    canViewAdminUsers:  has('admin:users:read'),
    canWriteAdminUsers: has('admin:users:write'),

    // 菜单管理
    canViewMenus:   has('admin:menus:read'),
    canWriteMenus:  has('admin:menus:write'),
    canDeleteMenus: has('admin:menus:delete'),

    // 商户管理
    canViewMerchants:   has('admin:merchants:read'),
    canWriteMerchants:  has('admin:merchants:write'),
    canDeleteMerchants: has('admin:merchants:delete'),

    // 品牌管理
    canViewBrands:   has('admin:brands:read'),
    canWriteBrands:  has('admin:brands:write'),
    canDeleteBrands: has('admin:brands:delete'),

    // 门店管理
    canViewStores:   has('admin:stores:read'),
    canWriteStores:  has('admin:stores:write'),
    canDeleteStores: has('admin:stores:delete'),

    // 原始检查——当需要用权限码直接判断时
    can: (code: string) => has(code),
  };
}
