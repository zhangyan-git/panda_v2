/**
 * 订单域那些枚举码的中文文案。
 *
 * 取值来自 order-service model 里的常量（也就是各表的 CHECK 约束），不是接口给的——接口
 * 回的就是库里那些英文码。所以**改枚举必须同时改迁移和这里**，加了新码而没登记，界面上
 * 就退回显示原始码。
 *
 * 集中放一个文件，理由与 couponLabels 相同：同一个码会出现在好几个页面（订单状态在列表、
 * 详情、售后的订单里都有），分开写一定漂移，而漂移的表现是同一笔单在两处显示成两种状态——
 * 不报错，只让人怀疑数据本身。
 *
 * 表在这里，翻码的那两个函数在 services/labels.ts。
 *
 * **支付方式不在这里**：那一列的值（catalog 的 code 与订单侧两个设备标签）与支付域共用一个
 * 来源，所以文案在 services/paymentMethodLabels.ts。同一个码在订单页和支付页必须叫同一个
 * 名字，两边各留一份就是漂移的起点。
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
 * 与订单状态分开：订单可能已经 paid（钱收了）而履约 failed（机器没出货），这两件事
 * 不能合成一个状态——合成之后「收了钱没出杯」这个最需要被看见的组合就消失了。
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

/** orders.source：这单从哪来的 */
export const ORDER_SOURCE: Record<string, EnumMeta> = {
  miniapp: { text: '小程序', color: 'blue' },
  screen_qr: { text: '屏幕扫码', color: 'geekblue' },
  // 线下刷卡机设备回调建的单（partner-service 验签后经 gRPC 建单，直接落成已支付，没有
  // 下单用户）。取值与 order/005 放宽后的 orders_source_check 逐字对应——那张表漏登记
  // 时，来源列会退回显示原始码 `device`，筛选下拉里也没有这一项。
  device: { text: '设备下单', color: 'purple' },
  // 会员续费代扣建的单（membership-service 收到渠道扣款成功的通知后经 gRPC 建单，同样是
  // 钱已在别处收过、直接落成已支付）。文案照老后台那一列：它在业务阶段显示的就是
  // 「自动续费」。取值与 order/009 放宽后的 orders_source_check 逐字对应。
  renewal: { text: '自动续费', color: 'gold' },
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
  // 「冲正」不是「退款」：它是把一笔已成功的出资从账上反向记一笔。退款单在
  // payment-service，这一列说的是这笔出资自己有没有被反做。
  reversed: { text: '已冲正', color: 'warning' },
};

/**
 * order_after_sales.status。
 *
 * 「已通过」与「退款中」「已退款」是三件事，不能省掉中间那两个：通过是**同意退**，通过之后
 * 服务端才去向支付域发起退款（refunding），钱真回去了才是 refunded。发起没成时会停在
 * 「已通过」——那不是卡住，而是一句准确的描述（同意退、退款单还没建成），等人在列表里点
 * 「发起退款」重试。把「已通过」写成「已退款」就是在这三件事里跳过了两件。
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

/** 订单上那三个「有没有某类行」的标记。列表行与详情都有这三个字段。 */
export type LineFlags = {
  hasDrinkLine: boolean;
  hasAddonLine: boolean;
  hasMembershipLine: boolean;
};

/**
 * 这一单由哪几类行组成，顺序固定为饮品 / 加购 / 会员。
 *
 * 后台的订单列表按行类型分成了四个页面（全部 + 咖啡 / 幸运杯套 / 会员），而 V2 的订单是
 * **合并单**——一杯饮品、若干加购品、一个会员套餐可以在一单里。于是「这单算哪种订单」
 * 没有唯一答案，用户已定的口径是**按含哪类行归**：含几类就出现在几个列表里。
 * 这个函数是那个口径在前端这一侧的唯一定义，列表里「构成」那一列就是它。
 *
 * 不返回文案而返回行类型：文案表在 ORDER_LINE_TYPE 里，同一个东西在列表和详情里
 * 必须叫同一个名字，所以只能有一个来源。
 */
export function orderComposition(row: LineFlags): string[] {
  const fields: [keyof LineFlags, string][] = [
    ['hasDrinkLine', 'drink'],
    ['hasAddonLine', 'addon'],
    ['hasMembershipLine', 'membership'],
  ];
  return fields.filter(([field]) => row[field]).map(([, lineType]) => lineType);
}
