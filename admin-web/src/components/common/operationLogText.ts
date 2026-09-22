import type { OperationLog } from '../../services/operationLog';

/**
 * 操作日志三张码表的公共副本：模块名、动作码、目标类型。
 *
 * 放在 components/common 而不是某个页面里，是因为它现在有两个消费方——平台操作日志页
 * 和设备详情页的「操作日志」tab。抄第二份的代价不是多几十行，而是两张表会分叉：某个
 * 模块在一处认出来了、在另一处还是 `device_drinks`，加模块的人不会知道自己漏改了哪个。
 * 页面上具体怎么摆（表格、抽屉、筛选下拉）各页自己做，码表只有这一份。
 *
 * # 与 services/*Labels.ts 那批不是一回事
 *
 * 那批的取值来自迁移里的 CHECK 约束，是**闭集**，所以各自带一份 `.test.ts` 把取值全集钉死，
 * 迁移里加了码而码表没跟就会红。这三张的取值来自**后端各服务的审计调用点**
 * （`audit.Entry{Module: …, Action: …, TargetType: …}`），是**开放集**：新写一处 Record
 * 就可能多一个码，前端没有任何办法自动知道，也就没有一个能钉死它的测试。
 *
 * 所以三张表天生会滞后，页面上的处理是**未登记就原样显示那个码**
 * （`moduleText[row.module] ?? row.module`），不是显示「未知」——看到 `coupons` 至少知道
 * 去哪儿查，看到「未知」就什么都查不了。筛选下拉走 `/facets`，新码的**选项**会自动出现，
 * 滞后只影响它的中文名。
 *
 * 加完一个域之后用这三条重新对一遍（在仓库根目录跑），它们就是下面三张表的取值来源：
 *
 *   grep -rhoE 'Module: +"[a-z_]+"'     backend/services/ | sort -u
 *   grep -rhoE 'Action: +"[a-z_]+"'     backend/services/ | sort -u
 *   grep -rhoE 'TargetType: +"[a-z_]+"' backend/services/ | sort -u
 *
 * **这三条捞不到动态取值的那两处**，得人工看（都写在各自表的注释里）：
 * membership.go 的 `Action: p.changeType` 与 after_sale.go 的 `Action: p.Action`。
 */

/**
 * 模块名 → 中文。
 *
 * 一个域一个模块名，与后端 audit.Entry 的 Module 逐字对应。它和侧边栏的菜单分组**不是
 * 一套词**，也不该硬凑成一套：菜单上一个「优惠券管理」下面四个页面，这里是 `coupons` /
 * `coupon_types` / `coupon_templates` 三个模块。日志要能对回是哪个服务写的，所以跟着服务的
 * 边界走，不跟菜单。
 *
 * **这条规矩对已下线的域同样成立：只要库里还留着那个域写下的历史审计行，它的码就留在表里。**
 * 已经有两条是这么留下来的，都是「服务没了、记录还在」：
 *   * `device_drinks`：设备×饮品那张关系表拆了，饮品行自带 device_id，读写都落在 `drinks` 上；
 *   * `inventory`：整个订货/库存域从 V2 删了（服务、proto、迁移、后台七页一起，见
 *     migrations/identity/036）。这一条留着是因为 admin_operation_logs 里有一条
 *     `inventory / confirm`——删掉它，那条旧记录会在页面上变成一串英文模块名。
 * 新删一个域时照这条判断：先 `SELECT DISTINCT module FROM admin_operation_logs`，有行才留。
 */
export const moduleText: Record<string, string> = {
  // 身份与权限（user-service）
  users: '管理员用户',
  roles: '角色',
  permissions: '权限',
  menus: '菜单',
  miniapp_users: '小程序用户',
  // 商户（merchant-service）
  merchants: '商户',
  brands: '品牌',
  stores: '门店',
  merchant_users: '商户账号',
  // 设备与饮品（coffee-machine-service）
  manufacturers: '设备厂商',
  devices: '咖啡机设备',
  drinks: '饮品',
  device_drinks: '设备饮品',
  device_balance: '设备余额',
  // 优惠券（coupon-service）
  coupons: '优惠券',
  coupon_types: '优惠券类型',
  coupon_templates: '优惠券模板',
  // 订单与售后（order-service）
  orders: '订单',
  after_sales: '售后',
  // 会员（membership-service）
  membership: '会员',
  membership_plans: '会员套餐',
  // 抽奖（lottery-service）
  lottery_activations: '门店抽奖',
  lottery_campaigns: '抽奖活动',
  lottery_rounds: '抽奖期次',
  // 订货 / 库存（inventory-service）。**服务已删，这条映射留着**：库里还有它写下的历史
  // 审计行（见文件头那条规矩）。
  inventory: '库存',
  // 福卡账户（account-service）
  coffee_bean: '咖啡豆',
};

/**
 * 动作码 → 中文。
 *
 * 与模块名不同，动作码**不按域切分**：`create` / `update` / `delete` 这些是各域共用的，
 * 所以这里只能给粗分类的中文，域内区别靠模块那一列。
 *
 * # 三个码的中文会重叠，这是后端词表本来的样子
 *
 * `cancel` / `revoke` / `void` 都落在「取消 / 作废 / 撤销」这一小片意思上，但它们分属各域
 * 各自的取舍（见下面每个码后面的注）。这里按该码**实际覆盖的用法**写，**精确说法在
 * 「操作描述」那一列**——那个中文由后端逐条给，「作废期次」与「取消订单」区别得清清楚楚，
 * 这一列只是给筛选用的粗分类。
 */
export const actionText: Record<string, string> = {
  // 增删改与状态：各域共用
  create: '新增',
  update: '修改',
  delete: '删除',
  update_status: '修改状态',
  update_scope: '修改数据范围',
  audit: '审核',
  // 身份与权限（user-service）
  assign_roles: '分配角色',
  remove_role: '移除用户角色',
  assign_permissions: '分配权限',
  remove_permission: '移除权限',
  assign_menus: '分配菜单',
  // 资产调整：设备余额（coffee-machine-service）与咖啡豆（account-service）共用
  adjust: '调整余额',
  // 订单与售后（order-service）
  // cancel 是订单与抽奖期次共用的码：订单是「取消订单」，期次是「作废期次」。
  cancel: '取消/作废',
  complete: '完成',
  // approve / reject 来自 after_sale.go 的 Action: p.Action，取值是
  // model.AfterSaleActionApprove / AfterSaleActionReject。
  approve: '审核通过',
  reject: '审核驳回',
  // 会员（membership-service）。四个码来自 membership.go 的 Action: p.changeType，
  // 取值是 model.ChangeFreeze / ChangeUnfreeze / ChangeRevoke / ChangeAdminAdjust
  // ——**只有管理员触发的那几个会记审计**，开通与续费走的是用户自己的下单，不记。
  freeze: '冻结',
  unfreeze: '解冻',
  // revoke 是会员与用户券共用的码：会员是「撤销会员」，券是「作废优惠券」。
  revoke: '作废/撤销',
  admin_adjust: '后台调整',
  // 优惠券（coupon-service）
  issue: '发放',
  redeem: '核销',
  // 抽奖（lottery-service）。draw 只记人工开奖那一次，自动开奖不记（它没有操作人）。
  activate: '开通',
  draw: '开奖',
  // confirm 是订货/库存域留下的（入库单确认）。服务已删，这一条与上面 `inventory` 同一个
  // 理由留着：库里那条 `inventory / confirm` 要把动作也显示成中文。
  // 与它同批的 void / ship / deliver / replace / set_alert_rule / delete_alert_rule 六个
  // **一并删了**——它们一条历史行都没有，也没有别的域在用（全仓 grep 过）。
  confirm: '确认',
};

/**
 * 目标类型码 → 中文。
 *
 * 它和模块名不是一套词（模块是 `devices`，目标是 `device`），所以不能共用一张表；也和目标
 * **名字**不是一回事——列表上「目标」那一列显示的是 `targetName`（单号、名字、批次号），
 * 这张表只服务详情抽屉里的「目标类型」那一格。
 *
 * `user` 与 `miniapp_user` 中文相同，**两条都要留**：前者是 account-service 调豆子余额时
 * 写下的目标（in.UserID 就是小程序用户 id），后者是身份域自己的说法。这是两个服务各自的
 * 写法，合成一条的话下次有人只改了一处，另一处就悄悄跟着变了。
 */
export const targetText: Record<string, string> = {
  // 身份与权限
  admin_user: '管理员用户',
  merchant_user: '商户账号',
  miniapp_user: '小程序用户',
  role: '角色',
  permission: '权限',
  menu: '菜单',
  // 商户
  merchant: '商户',
  brand: '品牌',
  store: '门店',
  // 设备与饮品
  manufacturer: '设备厂商',
  device: '咖啡机设备',
  drink: '饮品',
  drink_recipe: '饮品配方',
  // 优惠券
  coupon_type: '优惠券类型',
  coupon_template: '优惠券模板',
  coupon_batch: '发放批次',
  user_coupon: '用户优惠券',
  // 订单与售后（售后单的文案与 orderLabels 的 ORDER_LINE_TYPE.after_sale 一致）
  order: '订单',
  after_sale: '售后单',
  // 会员
  membership: '会员',
  membership_plan: '会员套餐',
  // 抽奖
  lottery_activation: '门店抽奖',
  lottery_campaign: '抽奖活动',
  lottery_round: '抽奖期次',
  // lottery_draw 是「人工开奖」这一次动作留下的那条开奖记录，不是期次本身
  lottery_draw: '开奖记录',
  // 订货 / 库存
  material: '物料',
  warehouse: '仓库',
  stock_receipt: '入库单',
  stock_issue: '出库单',
  stock_alert_rule: '补货线',
  // 福卡账户：调豆子余额时目标是用户本人
  user: '小程序用户',
};

/** 操作人的展示形式：姓名（用户名），只有一样时显示那样，都没有时是「—」。 */
export function adminUserText(row: OperationLog): string {
  if (!row.adminUsername) return row.adminName || '—';
  return row.adminName ? `${row.adminName}（${row.adminUsername}）` : row.adminUsername;
}
