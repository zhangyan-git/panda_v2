/**
 * 订单域那些枚举码的中文文案。
 *
 * 取值来自 order-service model 里的常量（也就是各表的 CHECK 约束），不是接口给的——接口
 * 回的就是库里那些英文码。所以**改枚举必须同时改迁移和这里**，加了新码而没登记，界面上
 * 就退回显示原始码。
 *
 * 集中放一个文件，理由与 storeLabels 相同：同一个码会出现在好几个地方（订单状态在列表、
 * 详情、售后里都有；取杯号在列表与详情里都有），分开写一定漂移，而漂移的表现是同一笔单在
 * 两处显示成两种状态——不报错，只让人怀疑数据本身。
 */

import type { EnumMeta } from './labels';

/** orders.status */
export const ORDER_STATUS: Record<string, EnumMeta> = {
  pending_payment: { text: '待支付', color: 'gold' },
  paid: { text: '已支付', color: 'blue' },
  completed: { text: '已完成', color: 'success' },
  cancelled: { text: '已取消', color: 'default' },
  expired: { text: '已过期', color: 'default' },
  refunding: { text: '退款中', color: 'processing' },
  refunded: { text: '已退款', color: 'warning' },
};

/**
 * orders.fulfillment_status：出杯这件事走到哪一步了。
 *
 * 与订单状态分开：订单可能已经 paid（钱收了）而履约 failed（机器没出货），这两件事不能
 * 合成一个状态——合成之后「收了钱没出杯」这个最需要被看见的组合就消失了。
 */
export const FULFILLMENT_STATUS: Record<string, EnumMeta> = {
  none: { text: '无需履约', color: 'default' },
  pending: { text: '待制作', color: 'default' },
  making: { text: '制作中', color: 'processing' },
  ready: { text: '待取杯', color: 'cyan' },
  completed: { text: '已完成', color: 'success' },
  failed: { text: '履约失败', color: 'error' },
  cancelled: { text: '已取消', color: 'default' },
};

/** orders.source：这单从哪来的。**没有 device**：设备回调建单是小程序/后台那条路的事实，商户端这一层不会看到。 */
export const ORDER_SOURCE: Record<string, EnumMeta> = {
  miniapp: { text: '小程序', color: 'blue' },
  screen_qr: { text: '屏幕扫码', color: 'geekblue' },
};

/** order_lines.line_type */
export const ORDER_LINE_TYPE: Record<string, EnumMeta> = {
  drink: { text: '饮品', color: 'blue' },
  addon: { text: '加购', color: 'geekblue' },
  membership: { text: '会员套餐', color: 'purple' },
};

/** order_payment_lines.status：一次出资分摊走到哪一步 */
export const PAYMENT_LINE_STATUS: Record<string, EnumMeta> = {
  reserved: { text: '已预占', color: 'processing' },
  succeeded: { text: '已成功', color: 'success' },
  failed: { text: '已失败', color: 'error' },
  released: { text: '已释放', color: 'default' },
  // 「冲正」不是「退款」：它是把一笔已成功的出资从账上反向记一笔。
  reversed: { text: '已冲正', color: 'warning' },
};

/**
 * order_after_sales.status。
 *
 * approved 的文案是「已通过」而不是「已退款」：后端 approve 只把申请标成 approved，钱由
 * payment-service 退。写「已退款」是在说一件没发生的事。
 */
export const AFTER_SALE_STATUS: Record<string, EnumMeta> = {
  pending: { text: '待审核', color: 'gold' },
  approved: { text: '已通过', color: 'processing' },
  rejected: { text: '已驳回', color: 'red' },
  refunding: { text: '退款中', color: 'processing' },
  refunded: { text: '已退款', color: 'success' },
  failed: { text: '退款失败', color: 'error' },
  cancelled: { text: '已撤销', color: 'default' },
};

/** order_after_sales.scope：退多少 */
export const AFTER_SALE_SCOPE: Record<string, EnumMeta> = {
  all: { text: '整单', color: 'blue' },
  drink: { text: '饮品行', color: 'cyan' },
  addon: { text: '加购行', color: 'geekblue' },
  membership: { text: '会员套餐', color: 'purple' },
};

/** order_state_transitions.actor_type：是谁做的这次迁移 */
export const ACTOR_TYPE: Record<string, EnumMeta> = {
  user: { text: '用户', color: 'blue' },
  merchant: { text: '商户', color: 'geekblue' },
  admin: { text: '管理员', color: 'purple' },
  system: { text: '系统', color: 'default' },
};

/** order_state_transitions.aggregate_type：这条流水记的是谁的迁移 */
export const AGGREGATE_TYPE: Record<string, EnumMeta> = {
  order: { text: '订单', color: 'blue' },
  order_line: { text: '订单行', color: 'cyan' },
  after_sale: { text: '售后单', color: 'orange' },
  payment_line: { text: '出资行', color: 'geekblue' },
};

// 下面这个不是枚举表的用法，单独说明。

/**
 * 支付方式的显示文案。
 *
 * 这一列上，`orders.payment_method`、`order_payment_lines.line_type`（门店详情「出资分摊」
 * 那张表）以及支付单那边的四列存的是**同一个值**：payment-service 目录（catalog）里的 code。
 * 从前它们各存一套「出资渠道」词表，加一种支付方式要改两处 DDL，而支付宝在那套词表里没有
 * 档位、只能落成 other——那套词表已经整个退场（migrations/order/008 与 payment/012），历史
 * 行按支付单号回填订正过了，所以这里不再列 wechat / unionpay / wallet / other 那些字面量。
 *
 * 六条 code 抄自 payment-service/internal/catalog 的 Code* 常量，名字抄自那边的 Name；商户端
 * 拿不到那份目录（它是支付服务的代码，不是接口给的），那边改名这里不会跟着变。
 *
 * 另外两条是**订单侧自己写的标签**，不是支付方式：设备单没有支付单（刷卡机走 partner 回调、
 * 取货码扣的是这台设备的咖啡余额，两条路都不经过 payment-service），订单侧就在这一列上写
 * card_pay / pickup_code 当标记（见 order-service repository/device_order.go）。
 *
 * 认不出来的一律原样回显——一张写着 wechat_v3 的单子比一张写着「其他」的单子有用得多。
 */
const PAYMENT_METHOD_TEXT: Record<string, string> = {
  ums_miniapp_wechat: '微信小程序（银联商务）',
  ums_h5_alipay: '支付宝（银联商务 H5）',
  ums_h5_wechat: '微信（银联商务 H5）',
  ums_h5_upqr: '云闪付（银联商务 H5）',
  ums_h5_wechat_minipay: '微信转小程序（银联商务 H5）',
  coffee_bean: '咖啡豆',
  card_pay: '刷卡机',
  pickup_code: '取货码（设备余额）',
};

/** 支付方式 → 文案。空值给占位符，认不出来的原样返回。 */
export function paymentMethodLabel(value?: string | null): string {
  const raw = value?.trim();
  if (!raw) return '—';
  return PAYMENT_METHOD_TEXT[raw] ?? raw;
}

/** 订单上那三个「有没有某类行」的标记。列表行与详情都有这三个字段。 */
export type LineFlags = {
  hasDrinkLine: boolean;
  hasAddonLine: boolean;
  hasMembershipLine: boolean;
};

/**
 * 这一单由哪几类行组成，顺序固定为饮品 / 加购 / 会员。
 *
 * V2 的订单是**合并单**——一杯饮品、若干加购品、一个会员套餐可以在一单里，所以「这单算
 * 哪种订单」没有唯一答案，口径是**按含哪类行归**：含几类就显示几类。列表里「构成」那一
 * 列就是它。
 */
export function orderComposition(row: LineFlags): string[] {
  const fields: [keyof LineFlags, string][] = [
    ['hasDrinkLine', 'drink'],
    ['hasAddonLine', 'addon'],
    ['hasMembershipLine', 'membership'],
  ];
  return fields.filter(([field]) => row[field]).map(([, lineType]) => lineType);
}
