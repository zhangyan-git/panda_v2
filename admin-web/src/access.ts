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

    // 会员域（membership-service）。三枚，权限数据见 migrations/identity/025：
    //   read   看套餐、看会员列表与详情、看变更流水。这一档迟早要发给客服——用户问
    //          「我是不是会员、什么时候到期、为什么被冻了」，答这三句只需要它。
    //   manage 建改套餐、上下架。改的是**接下来卖什么**，已购会员的条款在快照列上，
    //          所以这是本域唯一一处「改了也不用怕」的写入口。
    //   adjust 冻结 / 解冻 / 撤销 / 直接改有效期。**它直接白送钱**：把一个人的到期时间
    //          往后挪一年，没有任何订单、支付或流水跟着发生。所以它刻意不与 manage 合并
    //          ——「能改套餐文案的人也能给人加一年会员」是不该发生的事。它也**不在**小程序
    //          那条路上（那条路只能碰调用者自己的会员，且只能开关自动续费）。
    // 「能看」单独一枚，理由同订单域与库存域：客服要能查「这个人还是不是会员」而不必拿到
    // 动他权益的权力。
    canViewMembership: has('membership:read'),
    canManageMembership: has('membership:manage'),
    canAdjustMembership: has('membership:adjust'),

    // 支付域（payment-service）。**只剩一枚**：read 看支付单列表与详情。权限数据见
    // migrations/identity/026（027 发过第二枚，032 又把它删了）。
    //
    // 这里**没有 manage**，别照旧版把 canManagePayments 加回来：它原来管「接下来怎么收钱」
    // ——新增/修改/启停支付方式与渠道——而收钱的方式已经从运营数据变成了 internal/catalog
    // 里的常量表，那两张表连同两个写接口一起删了，032 也就把 `payment:manage` 从
    // admin_permissions 里删了。一枚已不存在的码写在这里**不报错**：它永远算 false，
    // 结果是某个按钮静默不出现——不是 403，控制台也没有一行线索。
    //
    // 「能看订单」不等于「能看支付单」：订单域只有一份快照式的 payment_no，渠道、出资行、
    // 记账流水与回调报文全在这个域里，所以是单独一枚码。
    canViewPayments: has('payment:read'),

    // 分账（同一个 payment-service 的另一半，identity/035 发的两枚）。权限数据见
    // migrations/identity/035_settlement_admin.sql：
    //   read   看分账规则、看收款账户、看分账明细（每条任务分给了谁、分了多少）。
    //          这一档迟早要发给运营——「这笔钱分给谁了」答这一句只需要它。
    //   manage 新增 / 修改 / 删除规则与账户，包括每一档分法的比例、固定额与收款方。
    //          **它决定的是「接下来这笔钱怎么分」**：把一条按门店的规则的比例从 45% 改成 90%，
    //          下一笔订单就开始按新的分，没有任何东西会拦一下。所以它与 read 分开。
    //
    // **它们刻意不并进 payment:read / payment:manage**，这是 008 的文件头（第 11–13 行）定死的
    // 三分：「谁能看支付单」与「谁能改分账配置」是两件事。前者是所有客服都要的（用户问
    // 「我这笔钱扣了没」就得看），后者决定钱分给谁——并进去之后，一次「给他开个支付单查询吧」
    // 会连带把分账的写权限一起送出去，而那是这个后台里最不该顺手送人的一枚。
    //
    // 这里**没有第三枚**（008 那三分里的 settlement:payout）：分账在 V2 是**随支付一次下发**的
    // （下单报文里带 divisionFlag / subOrders，支付成功即分账成功），没有一个「向渠道发起打款」
    // 的动作可以授权。identity/035 里写着为什么不发它——发一个永远勾不出对应行为、勾了也没有
    // 任何接口认的码，只会让权限页在骗人。
    canViewSettlement: has('settlement:read'),
    canManageSettlement: has('settlement:manage'),

    // 开放平台 / 合作方（partner-service）。两枚，权限数据见 migrations/identity/028：
    //   read   看合作方、看密钥（**只有掩码**）、看调用日志。这一档是给客服与排查用的：
    //          「他到底调没调进来、被哪一条挡了」答这一句只需要它。
    //   manage 新增改合作方、签发新密钥、启停、改 IP 白名单与限流。**它动的不是数据，是
    //          「谁能打进来」**：停用一家合作方会让名下所有密钥当场失效，一份写坏的白名单
    //          会让对方从下一个请求起全部 401。所以它与 read 分开，与 payment 那一对同一个
    //          切法。
    //
    // manage **不等于能看到明文密钥**：明文只在签发那一次的响应里出现，之后读接口只回掩码
    // （后端 dto 与 controller 上都钉着这一条）。manage 能做的最后一件事是「再签发一把」，
    // 不是「把已有的那把读出来」——所以页面上的「密钥」按钮不区分这两个码，抽屉里的写按钮
    // 才判 manage。
    canViewPartners: has('partner:read'),
    canManagePartners: has('partner:manage'),

    // 原始检查——当需要用权限码直接判断时
    can: (code: string) => has(code),
  };
}
