/**
 * 门店域那几个枚举码的中文文案。
 *
 * 取值来自 merchant-service model 里的常量（也是 stores 表 CHECK 约束里的值），不是接口给
 * 的——接口回的就是库里那些英文码。所以**改枚举必须同时改迁移和这里**，加了新码而这里没
 * 登记，界面上就退回显示原始码。
 *
 * 列表与详情共用这一份：同一个码在两处显示成两种说法，比显示成原始码更让人怀疑数据。
 */

import type { EnumMeta } from './labels';

/** stores.status */
export const STORE_STATUS: Record<string, EnumMeta> = {
  active: { text: '启用', color: 'success' },
  disabled: { text: '停用', color: 'default' },
};

/** stores.audit_status：门店的审核走到哪一步 */
export const STORE_AUDIT_STATUS: Record<string, EnumMeta> = {
  pending: { text: '待审核', color: 'gold' },
  approved: { text: '已通过', color: 'success' },
  rejected: { text: '已驳回', color: 'error' },
};
