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
  routes: [
    { path: '/login', component: 'login', layout: false },
    { path: '/', redirect: '/dashboard' },
    {
      path: '/dashboard',
      name: '概览',
      icon: 'DashboardOutlined',
      component: 'dashboard',
    },
  ],
  npmClient: 'pnpm',
  proxy: {
    '/api': {
      target: 'http://localhost:8082',
      changeOrigin: true,
    },
  },
});
