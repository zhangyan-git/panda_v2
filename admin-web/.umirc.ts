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
