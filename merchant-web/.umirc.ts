import { defineConfig } from '@umijs/max';

export default defineConfig({
  antd: {},
  access: {},
  model: {},
  initialState: {},
  request: {
    dataField: 'data',
  },
  layout: {
    title: 'Panda 商户',
  },
  // 关掉 MFSU：它会在 src/.umi 之外再落一层持久缓存，改了 app.ts 这类被所有页面引用的
  // 文件时，热更新偶尔给出旧模块（「改了没生效」），排查成本远高于它省下的启动时间。
  mfsu: false,
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
      path: '/stores',
      name: '门店',
      icon: 'ShopOutlined',
      component: 'stores',
    },
    // 详情页不进菜单（hideInMenu）：它们的 path 以列表页为前缀，侧栏已经由列表页点亮。
    { path: '/stores/:id', name: '门店详情', component: 'stores/detail', hideInMenu: true },
    {
      path: '/devices',
      name: '设备',
      icon: 'HddOutlined',
      component: 'devices',
    },
    { path: '/devices/:id', name: '设备详情', component: 'devices/detail', hideInMenu: true },
    {
      path: '/orders',
      name: '订单',
      icon: 'ProfileOutlined',
      component: 'orders',
    },
    { path: '/orders/:id', name: '订单详情', component: 'orders/detail', hideInMenu: true },
  ],
  npmClient: 'pnpm',
  proxy: {
    // 与 admin-web 一致：打网关（18080），由网关按路径前缀分发到各业务服务。
    // pathRewrite 不能省——网关认的是不带 /api 的路径（见 gateway-service 的
    // normalizePath），少了它每个请求都会变成 404。商户端**不直连**任何业务服务：
    // 数据范围是各服务自己按网关转发过来的令牌算的，绕开网关就绕开了那条链。
    '/api': {
      target: 'http://localhost:18080',
      changeOrigin: true,
      pathRewrite: { '^/api': '' },
    },
  },
});
