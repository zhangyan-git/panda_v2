/**
 * 优惠券域那几个枚举码的中文文案。
 *
 * 取值来自 migrations/coupon 里的 CHECK 约束（audit_status、status、
 * claim_type、redemption_type），不是接口给的——接口回的就是库里那些英文码。所以
 * **改枚举必须同时改迁移和这里**，加了新码而这里没登记，界面上就会退回显示原始码。
 *
 * 为什么集中放一个文件：同一个码会在好几个地方出现（audit_status 在模板列表和用户券
 * 详情都有，redemption_type 在模板表单和用户券详情都有）。分开写一定会漂移，而漂移的
 * 表现是同一张券在两个页面显示成两种状态，或者一处中文一处英文——这类不一致不会报错，
 * 只会让人怀疑数据本身有问题。
 *
 * 表在这里，翻码的那两个函数在 services/labels.ts（每个域都一样，只留一份实现）。
 */

import type { EnumMeta } from './labels';

/** coupon_templates.audit_status */
export const AUDIT_STATUS: Record<string, EnumMeta> = {
  pending: { text: '待审核', color: 'gold' },
  approved: { text: '已通过', color: 'green' },
  rejected: { text: '已拒绝', color: 'red' },
};

/** coupon_templates.status */
export const TEMPLATE_STATUS: Record<string, EnumMeta> = {
  draft: { text: '草稿', color: 'default' },
  active: { text: '已启用', color: 'green' },
  disabled: { text: '已停用', color: 'default' },
  closed: { text: '已关闭', color: 'red' },
};

/** user_coupons.status */
export const USER_COUPON_STATUS: Record<string, EnumMeta> = {
  claimed: { text: '已领取', color: 'blue' },
  held: { text: '已预占', color: 'processing' },
  redeemed: { text: '已核销', color: 'success' },
  expired: { text: '已过期', color: 'default' },
  refunded: { text: '已退款', color: 'warning' },
  invalidated: { text: '已作废', color: 'error' },
};

/** user_coupons.claim_type：这张券是怎么到用户手里的 */
export const CLAIM_TYPE: Record<string, EnumMeta> = {
  user_claim: { text: '主动领取', color: 'blue' },
  admin_assign: { text: '后台发放', color: 'geekblue' },
  daily_gift: { text: '每日赠送', color: 'cyan' },
  event_reward: { text: '活动奖励', color: 'purple' },
  claim_by_code: { text: '领取码', color: 'magenta' },
  purchase: { text: '购买', color: 'orange' },
};

/** coupon_templates.redemption_type / user_coupons.redemption_type */
export const REDEMPTION_TYPE: Record<string, EnumMeta> = {
  platform: { text: '平台核销', color: 'blue' },
  external_code: { text: '外部券码', color: 'geekblue' },
  show_qr: { text: '展示二维码', color: 'purple' },
};

/** coupon_templates.validity_mode：有效期怎么算出来的 */
export const VALIDITY_MODE: Record<string, EnumMeta> = {
  fixed: { text: '固定日期', color: 'blue' },
  relative: { text: '领取后天数', color: 'cyan' },
};

/** coupon_templates.claim_limit_mode：同一个人还能不能再领一次 */
export const CLAIM_LIMIT_MODE: Record<string, EnumMeta> = {
  once_ever: { text: '仅一次', color: 'default' },
  unlimited_after_use: { text: '使用后可再领', color: 'blue' },
  periodic: { text: '按周期', color: 'purple' },
};

/** coupon_batches.status */
export const BATCH_STATUS: Record<string, EnumMeta> = {
  pending: { text: '待处理', color: 'default' },
  active: { text: '进行中', color: 'processing' },
  exhausted: { text: '已耗尽', color: 'green' },
  closed: { text: '已关闭', color: 'red' },
  cancelled: { text: '已取消', color: 'default' },
};

/** coupon_batches.source */
export const BATCH_SOURCE: Record<string, EnumMeta> = {
  platform: { text: '平台', color: 'default' },
  admin: { text: '后台', color: 'blue' },
  merchant: { text: '商户', color: 'geekblue' },
  event: { text: '活动', color: 'purple' },
  purchase: { text: '购买', color: 'cyan' },
};
