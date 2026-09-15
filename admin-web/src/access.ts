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

    // 订单域。三个码的破坏力差两档，与后端 order-service routes/admin.go 里那三个
    // 常量一一对应：
    //   read  只是看；
    //   manage 能替用户把待支付的单关掉（人工干预，且写审计）；
    //   after-sale:audit 直接决定要不要把用户的钱退回去。
    // 「能看」与后面两个分开，是为了让客服能查单而不必同时拿到动单和动钱的权力。
    canViewOrders: has('order:read'),
    canCancelOrders: has('order:manage'),
    canReviewAfterSales: has('order:after-sale:audit'),
    // 标记订单完成。与 canCancelOrders 是**同一个码**（order:manage），两个键是按键名
    // 对应的**动作**起的名，不是按码起的。理由：这两个动作很可能分家（取消是替用户关单，
    // 完成是让这一单的赠品发出去——后者会真的发福卡），到那天只需要改这一行，两个页面
    // 的按钮不用动。合成一个键会逼着那天去改所有调用点。
    canCompleteOrders: has('order:manage'),

    // 福卡账户（account-service）。只有一个读码：福卡**仍然**没有人工发放/调整的接口，
    // 所以这里**刻意没有** canManageFortuneCards——加一个勾了也没有接口认的键，
    // 只会让下一个人以为存在调整能力。豆那一半已经落地了，见下面那两个键。
    //
    // 「能看订单」不等于「能看福卡」：余额与流水是账户库的事实，不随订单接口给出。
    // 页面上因此是「订单详情多一个 tab」而不是拆出新页面（见 orders/detail/index.tsx）。
    canViewFortuneCards: has('account:read'),

    // 咖啡豆账户（同一个 account-service，另一半）。读与福卡共用 account:read——
    // 同一个人同一句「他还有多少」，分开只会让客服要两个码才能回答一个问题。
    // 写是**单独的** account:manage：调整能把余额凭空加大（充值就是正数），
    // 没有审批、没有额度上限，所以「能看的人就能改别人余额」是不能接受的。
    // 后端 routes/admin.go 把 GET 挂在 account:read、POST /adjustments 挂在 account:manage，
    // 权限数据见 migrations/identity/021。只绑了 super_admin。
    canAdjustCoffeeBeans: has('account:manage'),

    // 抽奖域（lottery-service）。三个码的破坏力差三档（差得比订单域还开），与后端
    // routes/admin.go 里那三个常量一一对应：
    //   read   只是看活动、期次、中奖名单。泄露的是运营数据。
    //   manage 能建/改活动、改奖池、开关门店。改奖池就是改**将来中奖的名额与奖品**——
    //          它今天不直接动人资产，等券类奖品能自动发放了就会。
    //   draw   人工开奖与作废。**全系统唯一一个能凭空决定「谁中奖」的动作**，所以后端
    //          只把它绑给 super_admin，前端这里也只是照实反映——把它并进 manage 等于
    //          让任何一个能改活动名字的人也能决定中奖名单。
    // 「能看」与后面两个分开，是为了让客服能查「这个人中没中奖」而不必拿到开奖权。
    canViewLottery: has('lottery:read'),
    canManageLottery: has('lottery:manage'),
    canDrawLottery: has('lottery:draw'),

    // 原始检查——当需要用权限码直接判断时
    can: (code: string) => has(code),
  };
}
