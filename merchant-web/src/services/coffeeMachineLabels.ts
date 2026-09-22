/**
 * 设备域那几个枚举码的中文文案。
 *
 * 取值来自 coffee-machine-service model 里的常量（也是 devices 表 CHECK 约束里的值），
 * 不是接口给的——接口回的就是库里那些英文码。加了新码而这里没登记，界面上就退回显示原始码。
 */

import type { EnumMeta } from './labels';

/** devices.status */
export const DEVICE_STATUS: Record<string, EnumMeta> = {
  active: { text: '在架', color: 'success' },
  disabled: { text: '停用', color: 'default' },
};

/** devices.qrcode_type：机器屏上那张码是什么形态 */
export const QRCODE_TYPE: Record<string, EnumMeta> = {
  miniprogram: { text: '小程序码', color: 'blue' },
  regular: { text: '普通二维码', color: 'geekblue' },
};

/**
 * 「厂商在线」不是枚举而是三态：true / false / null。
 *
 * null（从未同步过厂商状态）必须与前两者分开——把它显示成「离线」会让一台还没接上的
 * 新机器看起来像坏了，而那是完全另一件事。所以它单独一个映射，不走 enumMeta。
 */
export function vendorOnlineMeta(value: boolean | null): EnumMeta {
  if (value === null) return { text: '未同步', color: 'default' };
  return value ? { text: '在线', color: 'success' } : { text: '离线', color: 'error' };
}
