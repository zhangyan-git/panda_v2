import { defineConfig } from '@umijs/max';

export default defineConfig({
  antd: {},
  access: {},
  model: {},
  initialState: {},
  request: {},
  layout: {
    title: 'Panda 后台',
  },
  routes: [
    { path: '/login', component: 'login', layout: false },
    { path: '/', redirect: '/dashboard' },
    {
      path: '/dashboard',
      name: '概览',
      icon: 'DashboardOutlined',
      component: 'dashboard',
    },
    {
      path: '/permissions',
      name: '权限管理',
      icon: 'SafetyCertificateOutlined',
      component: 'permissions',
      access: 'canViewPermissions',
    },
    {
      path: '/roles',
      name: '角色管理',
      icon: 'TeamOutlined',
      component: 'roles',
      access: 'canViewRoles',
    },
    {
      path: '/admin-users',
      name: '管理员用户',
      icon: 'UserOutlined',
      component: 'admin-users',
      access: 'canViewAdminUsers',
    },
    {
      path: '/menus',
      name: '菜单管理',
      icon: 'MenuOutlined',
      component: 'menus',
      access: 'canViewMenus',
    },
    {
      path: '/merchants',
      name: '商户管理',
      icon: 'ShopOutlined',
      component: 'merchants',
      access: 'canViewMerchants',
    },
    {
      path: '/brands',
      name: '品牌管理',
      icon: 'TagOutlined',
      component: 'brands',
      access: 'canViewBrands',
    },
    {
      path: '/stores',
      name: '门店管理',
      icon: 'EnvironmentOutlined',
      component: 'stores',
      access: 'canViewStores',
    },
    {
      path: '/coupons',
      name: '优惠券管理',
      icon: 'GiftOutlined',
      access: 'canViewCoupons',
      routes: [
        { path: '/coupons/types', name: '优惠券类型', component: 'coupon-types', access: 'canViewCouponTypes' },
        { path: '/coupons/templates', name: '优惠券模板', component: 'coupon-templates', access: 'canViewCoupons' },
        { path: '/coupons/batches', name: '发券批次', component: 'coupon-batches', access: 'canManageCouponBatches' },
        { path: '/coupons/user-coupons', name: '用户券', component: 'user-coupons', access: 'canViewCouponUserCoupons' },
      ],
    },
    {
      path: '/coffee-machines',
      name: '设备管理',
      icon: 'CoffeeOutlined',
      access: 'canViewCoffeeMachines',
      routes: [
        { path: '/coffee-machines/devices', name: '咖啡机设备', component: 'coffee-machines/devices', access: 'canViewCoffeeMachines' },
        // 设备详情是本仓库第一条带参数的路由。**不进侧边栏**：菜单由服务端菜单树驱动
        // （app.ts 的 menuDataRender 整个替换掉静态 routes 里的菜单项），而详情页本来就
        // 不该有一个从侧栏直达的入口——它必须带设备 id 才有意义。name 只用于面包屑。
        { path: '/coffee-machines/devices/:id', name: '设备详情', component: 'coffee-machines/devices/detail', access: 'canViewCoffeeMachines' },
        { path: '/coffee-machines/manufacturers', name: '厂商管理', component: 'coffee-machines/manufacturers', access: 'canViewCoffeeMachines' },
        { path: '/coffee-machines/drinks', name: '饮品管理', component: 'coffee-machines/drinks', access: 'canViewCoffeeMachines' },
      ],
    },
    // 订单域的这几条写成平铺的，不合成一个带子路由的目录：侧边栏由服务端菜单树驱动
    // （app.ts 里 menuDataRender 整个替换掉静态 routes 的菜单项），目录层级在那边是
    // 「订单管理 > 订单列表 / 咖啡订单 / 幸运杯套订单 / 会员订单 / 退款申请」；
    // 这里只需要能被路由到。退款申请的路径是 /after-sales 而不是 /orders/after-sales，
    // 静态这边再摆一层嵌套就成了两套说法。
    { path: '/orders', name: '订单列表', icon: 'ProfileOutlined', component: 'orders', access: 'canViewOrders' },
    // 三个分类列表：同一份 OrdersTable 各自钉住一个行类型。**必须排在 /orders/:id 之前**，
    // 不过真正决定匹配的是 react-router 6 的排序规则——静态段优先于 :id，所以
    // /orders/coffee 不会被详情页吃掉。顺序摆在这里只是让读的人不必去推那条规则。
    { path: '/orders/coffee', name: '咖啡订单', icon: 'CoffeeOutlined', component: 'orders/coffee', access: 'canViewOrders' },
    { path: '/orders/cup-sleeve', name: '幸运杯套订单', icon: 'TagOutlined', component: 'orders/cup-sleeve', access: 'canViewOrders' },
    { path: '/orders/membership', name: '会员订单', icon: 'TeamOutlined', component: 'orders/membership', access: 'canViewOrders' },
    // 详情是带参数的路由，与设备详情同一条约定：**不进侧边栏**（它必须带 id 才有意义），
    // name 只用于面包屑。
    { path: '/orders/:id', name: '订单详情', component: 'orders/detail', access: 'canViewOrders' },
    // 这一页审的是用户提交的售后申请，退款单在 payment-service（还没建）。所以叫「退款申请」
    // 而不是老后台的「退款订单」——页面上点的「通过申请」并不会真的把钱退了。
    { path: '/after-sales', name: '退款申请', icon: 'RollbackOutlined', component: 'after-sales', access: 'canViewOrders' },
    // 抽奖域。侧边栏是「抽奖管理 > 开通门店 / 抽奖活动 / 期次 / 中奖记录」，那套目录层级
    // 由服务端菜单树给（migrations/identity/022_lottery_admin.sql，app.ts 的
    // menuDataRender 整个替换掉静态 routes 里的菜单项），这里只需要能被路由到，所以
    // **平铺**着写，与订单域那几条同一条约定。
    { path: '/lottery/activations', name: '开通门店', icon: 'EnvironmentOutlined', component: 'lottery/activations', access: 'canViewLottery' },
    { path: '/lottery/campaigns', name: '抽奖活动', icon: 'GiftOutlined', component: 'lottery/campaigns', access: 'canViewLottery' },
    // 活动详情是带参数的路由，与设备详情、订单详情同一条约定：**不进侧边栏**
    // （它必须带 id 才有意义），name 只用于面包屑。排在 /lottery/campaigns 之后只是
    // 让人读起来顺——真正决定匹配的是 react-router 6 的静态段优先规则。
    { path: '/lottery/campaigns/:id', name: '活动详情', component: 'lottery/campaigns/detail', access: 'canViewLottery' },
    { path: '/lottery/rounds', name: '期次', icon: 'FieldTimeOutlined', component: 'lottery/rounds', access: 'canViewLottery' },
    { path: '/lottery/wins', name: '中奖记录', icon: 'CrownOutlined', component: 'lottery/wins', access: 'canViewLottery' },
    // 会员域。侧边栏是「会员管理 > 会员套餐 / 会员列表」，那套目录层级由服务端菜单树给
    // （migrations/identity/025_membership_admin.sql，app.ts 的 menuDataRender 整个替换掉
    // 静态 routes 里的菜单项），这里只需要能被路由到，所以**平铺**着写，与订单域、抽奖域
    // 那几条同一条约定。
    //
    // 三条都挂 canViewMembership（read）：新建/编辑/上下架在套餐页里判 canManageMembership，
    // 冻结/解冻/撤销/改有效期在详情页里判 canAdjustMembership。挂了写码的话，只该查
    // 「这个人还是不是会员」的客服就进不来——而那正是 read 这一枚存在的理由。
    { path: '/membership/plans', name: '会员套餐', icon: 'TagOutlined', component: 'membership/plans', access: 'canViewMembership' },
    { path: '/membership/members', name: '会员列表', icon: 'ContactsOutlined', component: 'membership/members', access: 'canViewMembership' },
    // 会员详情是带参数的路由，与设备详情、订单详情、入库单详情同一条约定：**不进侧边栏**
    // （它必须带 id 才有意义），name 只用于面包屑。变更流水就挂在这一页里——它是会员身上
    // 的时间线，离开那个会员就没有意义，所以 025 里没有给它单独的菜单。
    { path: '/membership/members/:id', name: '会员详情', component: 'membership/members/detail', access: 'canViewMembership' },
    // 包月订阅。挂 canViewMembership 同上：只读得到是「谁签了连续包月」，客服要答的是这句话。
    // 页面里唯一一个写动作「取消」另外判 canManageMembership（membership:manage）——它与
    // 「接下来卖什么」同属配置，不动任何人的会员有效期，所以不是 adjust 那一枚。
    { path: '/membership/subscriptions', name: '包月订阅', icon: 'SyncOutlined', component: 'membership/subscriptions', access: 'canViewMembership' },
    // 店铺码会员活动。挂 canViewMembership：只读得到「哪家店在做活动、送多少天」，客服要答的
    // 是这句话。增改与启停另外判 canManageMembership（membership:manage）——它与套餐管理同
    // 属「接下来送什么」，不改任何人的会员有效期，所以不是 adjust 那一枚。
    { path: '/membership/campaigns', name: '店铺码活动', icon: 'QrcodeOutlined', component: 'membership/campaigns', access: 'canViewMembership' },
    // 支付域。侧边栏是「支付管理 > 支付单」，那套目录层级由服务端菜单树给
    // （migrations/identity/026_payment_admin.sql，app.ts 的 menuDataRender 整个替换掉静态
    // routes 里的菜单项），这里只需要能被路由到，所以**平铺**着写，与订单域、抽奖域、库存域、
    // 会员域同一条约定。两条都挂 canViewPayments（payment:read）——支付单本身仍然是一条写路由
    // 都没有（关单、重放回调、发起退款都要先有退款与对账的语义）。
    //
    // **「支付方式与渠道」那一页连同它的写接口一起删了**：收钱那四种里有三种（咖啡豆 /
    // 银联商务小程序 / 银联商务 H5）写在 payment-service 的 internal/catalog 常量表里，
    // 密钥走环境变量，运营没有可配的东西；第四种是取货码，它根本不在那张表里——钱在这台
    // 设备的咖啡余额里、不经过支付服务（见 order-service 的 create_pickup.go）。032 迁移把 026 建的
    // 那片叶子（/payments/methods）也从菜单树里摘掉了——菜单来自服务端，删页面不删菜单
    // 只会留下一个点开是空白的入口。
    { path: '/payments', name: '支付单', icon: 'CreditCardOutlined', component: 'payments', access: 'canViewPayments' },
    // 分账三页（侧边栏是「支付管理 > 分账规则 / 分账账户 / 分账明细」，由 identity/035 的服务端
    // 菜单树给，这里只需要能被路由到，所以**平铺**着写，与其余各域同一条约定）。
    //
    // 三条都挂 canViewSettlement（settlement:read）：新建 / 编辑 / 删除在页面里判
    // canManageSettlement，所以只该查「这笔钱分给了谁」的人进得来、只是看不到写按钮——挂了
    // manage 的话他反而打不开这三页，而那正是 read 这一枚存在的理由。
    //
    // **排在 /payments/:id 之前**：真正决定匹配的是 react-router 6 的静态段优先规则，静态段
    // 本来就赢过 :id，摆在这里只是让读的人不必去推那条规则（与订单域那三条同一条注意事项）。
    { path: '/payments/settlement-rules', name: '分账规则', icon: 'PartitionOutlined', component: 'payments/settlement-rules', access: 'canViewSettlement' },
    { path: '/payments/settlement-accounts', name: '分账账户', icon: 'BankOutlined', component: 'payments/settlement-accounts', access: 'canViewSettlement' },
    { path: '/payments/settlement-tasks', name: '分账明细', icon: 'ReconciliationOutlined', component: 'payments/settlement-tasks', access: 'canViewSettlement' },
    // 分账明细详情是带参数的路由，与设备详情、订单详情、支付单详情同一条约定：**不进侧边栏**
    // （它必须带分账任务号才有意义），name 只用于面包屑。
    { path: '/payments/settlement-tasks/:id', name: '分账明细详情', component: 'payments/settlement-tasks/detail', access: 'canViewSettlement' },
    // 详情是带参数的路由，与设备详情、订单详情、入库单详情同一条约定：**不进侧边栏**
    // （它必须带支付单号才有意义），name 只用于面包屑。
    { path: '/payments/:id', name: '支付单详情', component: 'payments/detail', access: 'canViewPayments' },
    // 开放平台（合作方接入）。侧边栏是「开放平台 > 合作方」，那套目录层级由服务端菜单树给
    // （migrations/identity/028_partner_admin.sql，app.ts 的 menuDataRender 整个替换掉静态
    // routes 里的菜单项），这里只需要能被路由到，所以**平铺**着写，与订单域、抽奖域、库存域、
    // 会员域、支付域同一条约定。
    //
    // **只有这一片叶子**：密钥是行上的抽屉、调用日志是同一页页尾的查询区，两者都不进侧边栏
    // （028 里写着这个决定——它们是同一件排查动作的两半，多发一片叶子就会多一个「点开了不知
    // 道该看哪一行」的入口）。
    //
    // 挂 canViewPartners（partner:read）：新增 / 编辑 / 启停 / 签发在页面里判
    // canManagePartners，所以只该查「他家调没调进来」的人进得来、只是看不到写按钮——挂了
    // manage 的话他反而打不开这一页，而那正是 read 这一枚存在的理由。
    { path: '/partners', name: '合作方', icon: 'SafetyCertificateOutlined', component: 'partners', access: 'canViewPartners' },
    {
      path: '/miniapp-users',
      name: '小程序用户',
      icon: 'ContactsOutlined',
      component: 'miniapp-users',
      access: 'canViewMiniappUsers',
    },
    {
      path: '/operation-logs',
      name: '操作日志',
      icon: 'HistoryOutlined',
      component: 'operation-logs',
      access: 'canViewOperationLogs',
    },
  ],
  npmClient: 'pnpm',
  mfsu: false,
  proxy: {
    '/api': {
      target: 'http://localhost:18080',
      changeOrigin: true,
      pathRewrite: { '^/api': '' },
    },
  },
});
