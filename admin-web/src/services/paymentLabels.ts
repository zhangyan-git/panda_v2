/**
 * 支付域那些枚举码的中文文案。
 *
 * 取值来自 payment-service model 里的常量（也就是 migrations/payment 各表的 CHECK 约束），
 * 不是接口给的——接口回的就是库里那些英文码。所以**改枚举必须同时改迁移和这里**，加了新码
 * 而没登记，界面上就退回显示原始码（见 services/labels.ts 的 enumMeta）。
 *
 * 集中放一个文件，理由与 orderLabels / couponLabels 相同：同一个码会出现在好几处
 * （支付单状态在列表与详情都有，出资类型在出资行、记账流水两处都是同一套），分开写一定
 * 漂移，而漂移的表现是同一笔单在两处显示成两种状态——不报错，只让人怀疑数据本身。
 *
 * **没有支付方式与渠道那几张表**：那两张表已经删了，方式与渠道现在是代码里的常量，
 * 中文名由服务端照目录补在 methodName / channelName 上，不需要在这里再翻一次码。
 *
 * **出资类型那一列不在这里了**：payment_fundings.line_type / payment_transactions.line_type
 * 从前与订单域共用一套「出资渠道」词表，那套词表已经整个退场，
 * 四列今天存的就是支付方式的 code，文案在 services/paymentMethodLabels.ts——与订单页同一份。
 *
 * 表在这里，翻码的那两个函数在 services/labels.ts。
 *
 * **ACTOR_TYPE 不在这里、是重命名借来的**：它与订单域那张同值同义。抄第二份的代价不是多十行，
 * 是两处对同一列慢慢给出两种说法。
 *
 * **故意没有 AGGREGATE_TYPE**：payment_state_transitions 有六个 aggregate_type，但后台只读
 * payment 那一层（见 repository.GetPaymentDetail），列表上摆一列 162 行全是「支付单」的标签
 * 只是噪音，所以那个字段不出现在页面上。真要看别的 aggregate_type 时再补这张表。
 */

import type { EnumMeta } from './labels';
import { ACTOR_TYPE } from './orderLabels';

/** payments.status：一张支付单走到哪一步了。 */
export const PAYMENT_STATUS: Record<string, EnumMeta> = {
  created: { text: '已创建', color: 'default' },
  pending: { text: '待支付', color: 'gold' },
  succeeded: { text: '支付成功', color: 'success' },
  failed: { text: '支付失败', color: 'error' },
  // closed 是**定义了但今天没人写**的状态（CHECK 里有、状态机里留着位置，全仓没有代码路径
  // 会写它）。所以列表上会出现「订单已取消而支付单还是 pending」的行——那是已知空缺
  // （订单取消不关支付单，要等超时收走），不是数据错。真有行就显示这一条文案。
  closed: { text: '已关闭', color: 'default' },
  expired: { text: '已过期', color: 'default' },
};

/** payment_fundings.status：一条出资行走到哪一步。 */
export const FUNDING_STATUS: Record<string, EnumMeta> = {
  reserved: { text: '已预占', color: 'processing' },
  succeeded: { text: '已成功', color: 'success' },
  failed: { text: '已失败', color: 'error' },
  released: { text: '已释放', color: 'default' },
  // 「冲正」不是「退款」：它是把一笔已成功的出资从账上反向记一笔。退款单在 payment_refunds，
  // 这一列说的是这笔出资自己有没有被反做。
  reversed: { text: '已冲正', color: 'warning' },
};

/**
 * payment_state_transitions.actor_type：是谁做的这次迁移。
 *
 * 与订单域那张 ACTOR_TYPE **同值同义**（两张表各有一列，CHECK 都是这四个），借来用。
 */
export { ACTOR_TYPE };

/**
 * payment_provider_calls.operation：这次出网在调渠道的哪件事。
 *
 * 九个值里有四个（agreement_* 与 reconcile）今天没有代码在写——代扣与对账的表建好了、
 * 逻辑没做（见 docs/architecture.md 支付那一段）。它们是最终形态的一部分，所以登记着：
 * 那天真调起来，界面上不会出现一格英文。
 */
export const PROVIDER_OPERATION: Record<string, EnumMeta> = {
  create: { text: '下单', color: 'blue' },
  query: { text: '查单', color: 'cyan' },
  close: { text: '关单', color: 'default' },
  refund: { text: '退款', color: 'orange' },
  query_refund: { text: '查退款', color: 'geekblue' },
  agreement_sign: { text: '签约', color: 'purple' },
  agreement_charge: { text: '代扣', color: 'purple' },
  agreement_terminate: { text: '解约', color: 'default' },
  reconcile: { text: '对账', color: 'gold' },
};

/** payment_provider_calls.result：unknown = 没拿到明确答复（超时后也可能其实成功了）。 */
export const PROVIDER_RESULT: Record<string, EnumMeta> = {
  success: { text: '成功', color: 'success' },
  failed: { text: '失败', color: 'error' },
  timeout: { text: '超时', color: 'warning' },
  unknown: { text: '未知', color: 'default' },
};

/** payment_notifications.status：一条回调通知处理到哪一步。 */
export const NOTIFICATION_STATUS: Record<string, EnumMeta> = {
  received: { text: '已收到', color: 'processing' },
  processed: { text: '已处理', color: 'success' },
  // ignored 是「签名验过、但这条通知与我们无关」（比如重复通知已处理过的单），
  // 与 failed 分开：一个是正常吞掉，一个是没能处理。
  ignored: { text: '已忽略', color: 'default' },
  failed: { text: '处理失败', color: 'error' },
};

/**
 * payment_agreement_charges.status：代扣的某一期扣到哪一步了。
 *
 * 六个取值里后台今天只看得到 `succeeded` / `failed` / `charging` / `pending`——它们出现在
 * 「包月订阅」详情抽屉的续费明细里（那一页走 membership-service 的代理，不要求 payment:read）。
 *
 * **charging 与 pending 分开**：pending 是「到点了、还没发出去」，charging 是「已经发给渠道、
 * 等回音」。前者久了说明扫描没跑，后者久了说明通知没回来——两种毛病该找的人不一样。而
 * **skipped / cancelled 不是失败**：那是这一期根本不该扣（解约了、暂停了），与扣款失败在
 * 对账时是两件事。
 */
export const AGREEMENT_CHARGE_STATUS: Record<string, EnumMeta> = {
  pending: { text: '待扣款', color: 'default' },
  charging: { text: '扣款中', color: 'processing' },
  succeeded: { text: '扣款成功', color: 'success' },
  failed: { text: '扣款失败', color: 'error' },
  skipped: { text: '已跳过', color: 'default' },
  cancelled: { text: '已取消', color: 'default' },
};

/** payment_transactions.kind：这笔账是哪一类。 */
export const TRANSACTION_KIND: Record<string, EnumMeta> = {
  payment: { text: '收款', color: 'success' },
  refund: { text: '退款', color: 'warning' },
  reversal: { text: '冲正', color: 'purple' },
};

/** payment_transactions.direction：钱进还是钱出。 */
export const TRANSACTION_DIRECTION: Record<string, EnumMeta> = {
  in: { text: '入账', color: 'success' },
  out: { text: '出账', color: 'error' },
};
