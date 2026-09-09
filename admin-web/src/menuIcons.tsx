import {
  AppstoreOutlined,
  DashboardOutlined,
  FileOutlined,
  MenuOutlined,
  SafetyCertificateOutlined,
  SettingOutlined,
  ShopOutlined,
  TagOutlined,
  EnvironmentOutlined,
  TeamOutlined,
  UserOutlined,
} from '@ant-design/icons';
import type { ReactNode } from 'react';

/**
 * 菜单图标名 → 组件映射。
 * 后端 admin_menus.icon 存组件名；新增图标时在此登记，
 * 菜单管理页的图标下拉与侧栏渲染共用这份映射。
 */
export const MENU_ICONS: Record<string, ReactNode> = {
  DashboardOutlined: <DashboardOutlined />,
  SettingOutlined: <SettingOutlined />,
  SafetyCertificateOutlined: <SafetyCertificateOutlined />,
  TeamOutlined: <TeamOutlined />,
  UserOutlined: <UserOutlined />,
  MenuOutlined: <MenuOutlined />,
  AppstoreOutlined: <AppstoreOutlined />,
  FileOutlined: <FileOutlined />,
  ShopOutlined: <ShopOutlined />,
  TagOutlined: <TagOutlined />,
  EnvironmentOutlined: <EnvironmentOutlined />,
};

export const MENU_ICON_NAMES = Object.keys(MENU_ICONS);

export function renderMenuIcon(name?: string): ReactNode | undefined {
  return name ? MENU_ICONS[name] : undefined;
}
