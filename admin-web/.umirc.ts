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
