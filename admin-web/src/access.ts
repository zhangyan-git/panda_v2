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
    canViewRoles:   has('admin:roles:view'),
    canWriteRoles:  has('admin:roles:manage'),
    canDeleteRoles: has('admin:roles:delete'),

    // 权限管理
    canViewPermissions:   has('admin:permissions:view'),
    canWritePermissions:  has('admin:permissions:manage'),
    canDeletePermissions: has('admin:permissions:delete'),

    // 绑定管理（角色分配权限 / 用户分配角色）
    canViewBindings:  has('admin:bindings:view'),
    canWriteBindings: has('admin:bindings:manage'),

    // 管理员用户管理
    canViewAdminUsers:  has('admin:users:view'),
    canWriteAdminUsers: has('admin:users:manage'),

    // 菜单管理
    canViewMenus:   has('admin:menus:view'),
    canWriteMenus:  has('admin:menus:manage'),
    canDeleteMenus: has('admin:menus:delete'),

    // 商户管理
    canViewMerchants:   has('admin:merchants:view'),
    canWriteMerchants:  has('admin:merchants:manage'),
    canDeleteMerchants: has('admin:merchants:delete'),

    // 品牌管理
    canViewBrands:   has('admin:brands:view'),
    canWriteBrands:  has('admin:brands:manage'),
    canDeleteBrands: has('admin:brands:delete'),

    // 门店管理
    canViewStores:   has('admin:stores:view'),
    canWriteStores:  has('admin:stores:manage'),
    canDeleteStores: has('admin:stores:delete'),

    // 优惠券管理
    canViewCoupons: has('coupon:read'),
    canViewCouponTypes: has('coupon:type:manage'),
    canWriteCouponTypes: has('coupon:type:manage'),
    canWriteCouponTemplates: has('coupon:template:manage'),
    canAuditCouponTemplates: has('coupon:template:audit'),
    canIssueCoupons: has('coupon:issue'),
    canOverrideCouponIssue: has('coupon:issue:override'),
    canViewCouponUserCoupons: has('coupon:user-coupon:read'),
    canRedeemCoupon: has('coupon:user-coupon:redeem'),
    canRevokeCoupon: has('coupon:user-coupon:revoke'),
    canManageCouponBatches: has('coupon:batch:manage'),

    // 小程序用户（C 端顾客，users 表）。与上面的「管理员用户」是两批人：
    // 权限码也刻意分开，能管平台管理员不等于能看小程序用户的手机号。
    canViewMiniappUsers: has('admin:miniapp-users:view'),
    canManageMiniappUsers: has('admin:miniapp-users:manage'),

    // 操作日志（admin_operation_logs）。只有 view：这张表是审计证据，页面只读，
    // 后端也没有对应的写接口，加一个 canWrite 键会让人以为存在删除能力。
    canViewOperationLogs: has('admin:operation-logs:view'),

    // 咖啡机设备域（厂商 / 设备 / 饮品）。三个码的破坏力差一档，与后端 routes/admin.go
    // 里那三个常量一一对应：manage 能改设备资料与饮品价格，balance 能直接动设备余额。
    // 读与写分开，是为了让人维护饮品目录时不必连资金操作一起拿到。
    canViewCoffeeMachines: has('coffee_machine:read'),
    canWriteCoffeeMachines: has('coffee_machine:manage'),
    canAdjustCoffeeBalance: has('coffee_machine:balance'),

    // 原始检查——当需要用权限码直接判断时
    can: (code: string) => has(code),
  };
}
