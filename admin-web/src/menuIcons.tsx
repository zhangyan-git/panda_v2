import {
  ApiOutlined,
  AppstoreOutlined,
  BankOutlined,
  CoffeeOutlined,
  ContactsOutlined,
  CreditCardOutlined,
  CrownOutlined,
  DashboardOutlined,
  EnvironmentOutlined,
  FieldTimeOutlined,
  FileOutlined,
  GiftOutlined,
  HistoryOutlined,
  MenuOutlined,
  PartitionOutlined,
  PayCircleOutlined,
  ProfileOutlined,
  ReconciliationOutlined,
  RollbackOutlined,
  SafetyCertificateOutlined,
  SettingOutlined,
  ShopOutlined,
  ShoppingCartOutlined,
  SyncOutlined,
  QrcodeOutlined,
  TagOutlined,
  TagsOutlined,
  TeamOutlined,
  FileTextOutlined,
  TrophyOutlined,
  UnorderedListOutlined,
  UserOutlined,
} from '@ant-design/icons';
import type { ReactNode } from 'react';

/**
 * 菜单图标名 → 组件映射。
 * 后端 admin_menus.icon 存组件名；新增图标时在此登记，
 * 菜单管理页的图标下拉与侧栏渲染共用这份映射。
 *
 * 反过来，整个域下线时登记要跟着删：订货 / 库存域删掉那一次，023 与 024 留下的八个名字
 * （Database/Gold/Experiment/Home/BarChart/Swap/Inbox/Send）一起摘了——服务的菜单行都没了，
 * 留着它们只会在菜单管理页的图标下拉里多出几个谁也选不中的名字。见 migrations/identity/036。
 */
export const MENU_ICONS: Record<string, ReactNode> = {
  DashboardOutlined: <DashboardOutlined />,
  SettingOutlined: <SettingOutlined />,
  SafetyCertificateOutlined: <SafetyCertificateOutlined />,
  TeamOutlined: <TeamOutlined />,
  UserOutlined: <UserOutlined />,
  ContactsOutlined: <ContactsOutlined />,
  HistoryOutlined: <HistoryOutlined />,
  MenuOutlined: <MenuOutlined />,
  AppstoreOutlined: <AppstoreOutlined />,
  FileOutlined: <FileOutlined />,
  ShopOutlined: <ShopOutlined />,
  TagOutlined: <TagOutlined />,
  EnvironmentOutlined: <EnvironmentOutlined />,
  CoffeeOutlined: <CoffeeOutlined />,
  // 订单域（identity/018 那三个菜单用的就是这三个名字）
  ShoppingCartOutlined: <ShoppingCartOutlined />,
  ProfileOutlined: <ProfileOutlined />,
  RollbackOutlined: <RollbackOutlined />,
  // 抽奖域（identity/022）。四个名字当时漏登记了：renderMenuIcon 对认不出的名字返回
  // undefined，侧栏只是图标空着、不报错，所以一直没被发现。
  TrophyOutlined: <TrophyOutlined />,
  GiftOutlined: <GiftOutlined />,
  FieldTimeOutlined: <FieldTimeOutlined />,
  CrownOutlined: <CrownOutlined />,
  // 店铺码会员活动（identity/034）。名字与那条迁移里写的**逐字相同**，改一边就要改另一边。
  QrcodeOutlined: <QrcodeOutlined />,
  // 包月订阅（identity/033）。那条迁移的文件头专门提醒「图标要先在 src/menuIcons.tsx 里
  // 登记」——不登记的表现与 022 那四个一样：renderMenuIcon 认不出就返回 undefined，
  // 侧栏只是图标空着、不报错。
  SyncOutlined: <SyncOutlined />,
  // 支付域（identity/026）：PayCircleOutlined 是目录「支付管理」的，另两个是叶子。
  // 同样**逐字**对齐那条迁移里 admin_menus.icon 写的那三个名字——不登记的表现与 022
  // 那四个一样：renderMenuIcon 对认不出的名字返回 undefined，侧栏只是图标空着、不报错。
  PayCircleOutlined: <PayCircleOutlined />,
  CreditCardOutlined: <CreditCardOutlined />,
  ApiOutlined: <ApiOutlined />,
  // 优惠券管理（identity/007）。那一条里六个名字有三个当时漏登了，表现与 022 那四个一样：
  // renderMenuIcon 对认不出的名字返回 undefined，侧栏只是图标空着、不报错，菜单管理页的
  // 图标下拉里也选不到。名字与迁移里写的**逐字相同**，改一边就要改另一边。
  // （同一条里的 UserOutlined 早就在上面登记过，不必重复。另一项 SendOutlined——第 49 行
  // 那条「发放优惠券」/coupons/issue——菜单已被 identity/008 摘掉，这一次把登记也删了。）
  TagsOutlined: <TagsOutlined />,
  FileTextOutlined: <FileTextOutlined />,
  UnorderedListOutlined: <UnorderedListOutlined />,
  // 分账三页（identity/035）。名字与那条迁移里 admin_menus.icon 写的三个**逐字相同**，
  // 改一边就要改另一边。目录「支付管理」用的 PayCircleOutlined 在 026 那一批里已登记。
  PartitionOutlined: <PartitionOutlined />,
  BankOutlined: <BankOutlined />,
  ReconciliationOutlined: <ReconciliationOutlined />,
};

export const MENU_ICON_NAMES = Object.keys(MENU_ICONS);

export function renderMenuIcon(name?: string): ReactNode | undefined {
  return name ? MENU_ICONS[name] : undefined;
}
