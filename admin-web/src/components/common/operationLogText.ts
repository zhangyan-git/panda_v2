import type { OperationLog } from '../../services/operationLog';

/**
 * 操作日志三张码表的公共副本：模块名、动作码、目标类型。
 *
 * 放在 components/common 而不是某个页面里，是因为它现在有两个消费方——平台操作日志页
 * 和设备详情页的「操作日志」tab。抄第二份的代价不是多几十行，而是两张表会分叉：某个
 * 模块在一处认出来了、在另一处还是 `device_drinks`，加模块的人不会知道自己漏改了哪个。
 * 页面上具体怎么摆（表格、抽屉、筛选下拉）各页自己做，码表只有这一份。
 */

/**
 * 模块名 → 中文。取值由后端各服务的审计调用点决定，这里只覆盖已知的那些，
 * 没登记的按原名显示 —— 新模块上线时看到的是 `coupons`，总好过看到一个「未知」。
 *
 * 后端的取值清单（`grep -rhoE 'Module: +"[a-z_]+"' backend/`）：
 * users / merchant_users / merchants / brands / stores / roles / permissions / menus /
 * miniapp_users / manufacturers / drinks / devices / device_balance。
 *
 * `device_drinks` 已经不再产出（设备×饮品那张关系表拆了，饮品行自带 device_id，读写
 * 都落在 `drinks` 上），但**这条映射要留着**：库里已经写下历史审计行，删掉这张码表
 * 只会让那些旧记录在页面上变成一串英文表名。
 */
export const moduleText: Record<string, string> = {
  users: '管理员用户',
  merchant_users: '商户账号',
  merchants: '商户',
  brands: '品牌',
  stores: '门店',
  roles: '角色',
  permissions: '权限',
  menus: '菜单',
  miniapp_users: '小程序用户',
  manufacturers: '设备厂商',
  drinks: '饮品',
  devices: '咖啡机设备',
  device_drinks: '设备饮品',
  device_balance: '设备余额',
};

/** 动作码 → 中文。取值同样是后端各服务的审计调用点写死的。 */
export const actionText: Record<string, string> = {
  create: '新增',
  update: '修改',
  delete: '删除',
  update_status: '修改状态',
  update_scope: '修改数据范围',
  audit: '审核',
  adjust: '调整余额',
  assign_permissions: '分配权限',
  remove_permission: '移除权限',
  assign_roles: '分配角色',
  remove_role: '移除角色',
  assign_menus: '分配菜单',
};

/**
 * 目标类型码 → 中文。同样是后端写死的取值（`grep -rhoE 'TargetType: +"[a-z_]+"'`）：
 * admin_user / brand / device / drink / manufacturer / menu / merchant / merchant_user /
 * miniapp_user / permission / role / store。它和模块名不是一套词（模块是 `devices`，
 * 目标是 `device`），所以不能共用一张表。
 */
export const targetText: Record<string, string> = {
  admin_user: '管理员用户',
  merchant_user: '商户账号',
  miniapp_user: '小程序用户',
  merchant: '商户',
  brand: '品牌',
  store: '门店',
  role: '角色',
  permission: '权限',
  menu: '菜单',
  manufacturer: '设备厂商',
  device: '咖啡机设备',
  drink: '饮品',
};

/** 操作人的展示形式：姓名（用户名），只有一样时显示那样，都没有时是「—」。 */
export function adminUserText(row: OperationLog): string {
  if (!row.adminUsername) return row.adminName || '—';
  return row.adminName ? `${row.adminName}（${row.adminUsername}）` : row.adminUsername;
}
