/**
 * 会员域那些枚举码的中文文案。
 *
 * 取值来自 membership-service model 里的常量（也就是 migrations/membership/001 的 CHECK 约束），
 * 不是接口给的——接口回的就是库里那些英文码。所以**改枚举必须同时改迁移和这里**，加了新码而
 * 没登记，界面上就退回显示原始码。
 *
 * 集中放一个文件，理由与 lotteryLabels / orderLabels 相同：同一个码会出现在好几个地方
 * （会员状态在列表页、详情页的基本信息、变更流水的 from/to 三处都有），分开写一定漂移，
 * 而漂移的表现是同一个人在两处显示成两种状态——不报错，只让人怀疑数据本身。
 *
 * 表在这里，翻码的那两个函数在 services/labels.ts。
 */

import type { EnumMeta } from './labels';

/**
 * membership_plans.status：这个套餐现在卖不卖。
 *
 * **draft 与 disabled 分开**：draft 是「还没上架过」，disabled 是「上过架又撤下来了」。
 * 对买不到这件事是一样的，但对运营不一样——前者是待办的活，后者是已经做过的决定。
 * 并成「未上架」的话，一个刚建好的套餐和一个刚下架的套餐在列表上长得一模一样。
 */
export const PLAN_STATUS: Record<string, EnumMeta> = {
  draft: { text: '草稿', color: 'default' },
  active: { text: '已上架', color: 'success' },
  // 「已下架」而不是「已停用」：它不影响已经买过的人（会员行上存着成交快照），
  // 说成停用会让人以为买过的人也被停了。
  disabled: { text: '已下架', color: 'warning' },
};

/** membership_plans.period：每期时长的单位。显示上要连 periodCount 一起读，见 periodLabel。 */
export const PLAN_PERIOD: Record<string, EnumMeta> = {
  month: { text: '月', color: 'blue' },
  year: { text: '年', color: 'purple' },
};

/**
 * membership_plans.member_price_mode：会员价从哪来。
 *
 * 这两个值的区别是**用户手机上那句最要紧的话**：auto 的人打开小程序看到「你已享会员价」，
 * coupon 的人看到的是「你有 N 张会员价券」。后台看这一列，问的就是「这个人是怎么享到会员价的」。
 */
export const MEMBER_PRICE_MODE: Record<string, EnumMeta> = {
  auto: { text: '自动享会员价', color: 'cyan' },
  coupon: { text: '会员价体验券', color: 'gold' },
};

/**
 * memberships.status：一个人现在的会员资格处在哪一步。
 *
 * **frozen 与 revoked 分开且不止是措辞**：冻结是可逆的（解冻回 active），撤销不可逆。
 * 把两者显示成同一个词，会让一次手滑与一次可挽回的处理在列表上长得一样。
 *
 * expired 不由后台产生——它只由到期扫描写，所以这一列上出现「已过期」时，那是系统写的。
 */
export const MEMBERSHIP_STATUS: Record<string, EnumMeta> = {
  active: { text: '生效中', color: 'success' },
  frozen: { text: '已冻结', color: 'warning' },
  expired: { text: '已过期', color: 'default' },
  revoked: { text: '已撤销', color: 'error' },
};

/**
 * membership_changes.change_type：会员身上发生过的一件事。
 *
 * 十二个取值后端都在 CHECK 里写全了，但今天写得出来的只有一半——subscribe / unsubscribe 要
 * 签约链路、refund_adjust 要退款单、expire 要到期扫描，这三条路都还没实现。照登的理由与
 * 抽奖那边逐字相同：登记一个今天到不了的码，好过将来那一格原样显示 `unsubscribe`。
 *
 * 「开通」与「续费」的区分是这一列最要紧的地方：一次重投如果没被 `(order_id, change_type)`
 * 那条唯一索引挡住，同一天会出现两条 renew——所以看到续期次数与订单数对不上时，先看这里。
 */
export const CHANGE_TYPE: Record<string, EnumMeta> = {
  activate: { text: '开通', color: 'success' },
  renew: { text: '续费', color: 'processing' },
  expire: { text: '到期失效', color: 'default' },
  freeze: { text: '冻结', color: 'warning' },
  unfreeze: { text: '解冻', color: 'cyan' },
  auto_renew_on: { text: '打开自动续费', color: 'blue' },
  auto_renew_off: { text: '关闭自动续费', color: 'default' },
  subscribe: { text: '签约代扣', color: 'geekblue' },
  unsubscribe: { text: '解约', color: 'default' },
  refund_adjust: { text: '退款调整', color: 'volcano' },
  admin_adjust: { text: '后台调整', color: 'purple' },
  revoke: { text: '撤销', color: 'error' },
};

/**
 * membership_changes.operator_type：这件事是谁做的。
 *
 * system 与 worker 分开：system 是**没有任何人做**（开通那一条是订单支付回调触发的），
 * worker 是到期扫描那个后台任务。两者都不是人，但对账时要分得清是「用户/客服的动作」
 * 还是「机器自己跑的」。
 */
export const CHANGE_OPERATOR: Record<string, EnumMeta> = {
  user: { text: '用户', color: 'blue' },
  admin: { text: '管理员', color: 'purple' },
  system: { text: '系统', color: 'default' },
  worker: { text: '定时任务', color: 'default' },
};

// 下面几个不是枚举表的用法，单独说明。

/**
 * 套餐时长的显示文案：「1 个月」「12 个月」「1 年」。
 *
 * 后端**故意不回一个拼好的中文串**（见 dto.PlanResponse.Period 那段）：拼串会把「几期」
 * 这件事挡在后端，而它是对账时要用的两个数。所以拼接归前端，放这里而不是页面里。
 *
 * periodCount 为零或负数时只显示单位：库上的 CHECK 是 > 0，真出现 0 时说「0 个月」看起来
 * 像这个套餐买不到任何东西，比一个光秃秃的「月」更容易让人以为数据坏了。
 */
export function periodLabel(period: string, periodCount: number): string {
  const unit = PLAN_PERIOD[period]?.text ?? period;
  return periodCount > 0 ? `${periodCount} ${unit}` : unit;
}

/**
 * 会员价的说明文案，给套餐列表那一列用。
 *
 * coupon 模式下把每期张数带上：「会员价体验券」这句话本身答不了运营的下一个问题
 * （「那一个月给几张」），而那个数就在同一行上，没有理由让他点进详情。
 */
export function memberPriceLabel(mode: string, couponsPerPeriod: number): string {
  const meta = MEMBER_PRICE_MODE[mode] ?? { text: mode, color: 'default' };
  if (mode !== 'coupon') return meta.text;
  return couponsPerPeriod > 0 ? `${meta.text} · 每期 ${couponsPerPeriod} 张` : meta.text;
}

/**
 * membership_subscriptions.status：这条连续包月签约走到哪一步了。
 *
 * 五个取值不是一条能从颜色上读出来的直线，所以措辞要各自说清：
 *
 * - `pending_sign` 是**中间态**（已下单、等用户在微信里点完），列表默认不显示它。
 * - `cancelled` 与 `expired` 的区别不是「谁发起的」而是**还能不能复活**：前者有一方明确说
 *   不续了，后者是正常走完。都显示成「已结束」会让这两种情况在列表上长得一样。
 */
export const SUBSCRIPTION_STATUS: Record<string, EnumMeta> = {
  pending_sign: { text: '待签约', color: 'processing' },
  active: { text: '生效中', color: 'success' },
  suspended: { text: '已暂停', color: 'warning' },
  cancelled: { text: '已取消', color: 'error' },
  expired: { text: '已到期', color: 'default' },
};

/**
 * 签约场景。库里没有这一列，是后端由三个来源 id 现推的。
 *
 * 三档里**今天只可能出现 member_center**：另外两条路要小程序端把咖啡订单号 / 活动领取单号
 * 写进订阅行，而那条链路还没接（见 services/subscription.ts 的文件头）。照登的理由与
 * CHANGE_TYPE 那段相同：登记一个今天到不了的码，好过将来那一格原样显示 `coffee_order`。
 */
export const SUBSCRIPTION_SCENE: Record<string, EnumMeta> = {
  coffee_order: { text: '扫码点单时签约', color: 'blue' },
  store_campaign: { text: '店铺码活动签约', color: 'purple' },
  member_center: { text: '会员中心签约', color: 'default' },
};
