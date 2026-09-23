/**
 * 「这一单/这笔支付是哪种支付方式」的中文名。
 *
 * 这是**支付方式唯一的文案来源**：订单列表与详情读 orders.payment_method，支付单列表与详情
 * 读 payments.payment_method，出资行、记账流水读各自的 line_type——它们现在是**同一个值**，
 * 所以中文名也只能有一份。抄第二份的代价是两处对同一列慢慢给出两种说法。
 *
 * # 值是哪来的
 *
 * 六条来自 payment-service/internal/catalog 的 Code* 常量（收钱那几条路都写在那张常量表里，
 * 不是库里的配置行）：
 *
 *   ums_miniapp_wechat / ums_h5_alipay / ums_h5_wechat / ums_h5_upqr /
 *   ums_h5_wechat_minipay / coffee_bean
 *
 * 名字是从 catalog 的 Name 抄过来的——订单服务不持有支付域的目录，而支付单列表页的中文名是
 * 服务端照目录补的（methodName），订单列表拿不到同一份。那边改了名字这里不会跟着变。
 *
 * 另外两条是**订单侧自己写的标签**，不是支付方式：设备单没有支付单（刷卡机走 partner 回调、
 * 取货码扣的是这台设备的咖啡余额，两条路都不经过 payment-service），于是订单侧在 payment_method
 * 这一列上写 card_pay / pickup_code 当标记（见 order-service repository/device_order.go）。
 *
 * # 已经不存在的那些值
 *
 * 从前这里还列着 wechat / unionpay / wallet / other 那一套「出资渠道」，以及 balance / cash /
 * wechat_pay 三个当时猜的。那套词表**整个退场了**：
 * 同一件事在库里有两个落点、两套词，加一种支付方式要改两处 DDL，而支付宝在那套词表里没有档位，
 * 只能落成 other，后台把一笔支付宝单显示成「其他」。历史行已按支付单号回填订正，所以那些字面量
 * 今天不再出现，删掉它们；哪天真在界面上看见一个（本文件认不出来会原样回显），那是回填漏了行，
 * 不是缺文案。
 *
 * 认不出来的一律原样回显——与「未知枚举也要看得见」同一个道理。
 */

import type { EnumMeta } from './labels';

/** 支付方式的 code → 文案。同时给 ProTable 的搜索下拉当选项表用。 */
export const PAYMENT_METHOD: Record<string, EnumMeta> = {
  // payment-service/internal/catalog 的 Code* 常量
  ums_miniapp_wechat: { text: '微信小程序（银联商务）', color: 'green' },
  ums_h5_alipay: { text: '支付宝（银联商务 H5）', color: 'blue' },
  ums_h5_wechat: { text: '微信（银联商务 H5）', color: 'green' },
  ums_h5_upqr: { text: '云闪付（银联商务 H5）', color: 'geekblue' },
  ums_h5_wechat_minipay: { text: '微信转小程序（银联商务 H5）', color: 'cyan' },
  coffee_bean: { text: '咖啡豆', color: 'orange' },
  // 微信直连委托代扣。它在 catalog 里是**一条协议通道而不是收银通道**（Create 会拒绝，
  // 用户永远选不到它），但它确实会出现在 orders.payment_method 上：会员续费代扣建的那张
  // 单写的就是它（order-service service/create_renewal.go）。那一列就是这张表渲染的列，
  // 所以少了这一格，订单管理里的续费单会裸奔一个 wechat_papay。
  wechat_papay: { text: '微信代扣', color: 'purple' },
  // 订单侧自己的标签：设备单与取货码单没有支付单
  card_pay: { text: '刷卡机', color: 'purple' },
  pickup_code: { text: '取货码（设备余额）', color: 'gold' },
};

/** 支付方式 → 文案。空值给占位符，认不出来的原样返回。 */
export function paymentMethodLabel(value?: string | null): string {
  const raw = value?.trim();
  if (!raw) return '—';
  return PAYMENT_METHOD[raw]?.text ?? raw;
}
