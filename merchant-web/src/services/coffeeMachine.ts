import { request } from '@umijs/max';
import type { PageQuery, PageResult } from './pagination';

/**
 * 商户端设备接口。字段名与 coffee-machine-service 的 dto.DeviceSummary / DeviceDetail 的
 * json tag 逐字对应，同时也是页面上的 dataIndex。
 *
 * **商户端不做饮品**：饮品管理留在后台（coffee-machine-service 的 /v1/admin/coffee-machines 那棵
 * 树上），这一层没有任何饮品接口，设备详情也不挂饮品 tab。
 *
 * 这里没有「按门店筛」的入参：设备属于哪些门店由服务端按令牌实时解析出来的数据范围决定。
 * 给一个能表达「看哪家门店」的参数，早晚会有人把它接着往下传。
 */

export type DeviceStatus = 'active' | 'disabled';

/**
 * 设备在列表里的形状，对应后端 dto.DeviceSummary。
 *
 * 列表刻意不带版本号、二维码配置、余额和静态验证码：要什么就去详情拿，别指望这里多出一个字段。
 */
export type DeviceSummary = {
  id: string;
  serialUnique: string;
  deviceName: string;
  manufacturerId: string;
  storeId: string | null;
  status: DeviceStatus;
  /** null = 从未同步过厂商状态，与 false（厂商说它离线）是两回事 */
  vendorOnline: boolean | null;
  lastSyncedAt: string | null;
  lastFaultCode: string;
  lastFaultMessage: string;
  lastActiveAt: string | null;
  createdAt: string;
  updatedAt: string;
};

/**
 * 设备详情，对应后端 dto.DeviceDetail：列表的全部字段，加上版本号、二维码配置与余额。
 *
 * 与后台是**同一个 DTO**（服务端刻意不裁剪）：同一台设备在两个端上显示的是同一组事实，
 * 各写一份只会让字段各自漂移。
 */
export type DeviceDetail = DeviceSummary & {
  versionNumber: string;
  androidVersion: string;
  mainBoardVersion: string;
  lastFaultAt: string | null;
  /**
   * 静态验证码明文。商户端是只读的，这里只用来展示（运营要在机器跟前核对这个码）；
   * 后台那份还能回填编辑表单，那边有「留空即不改」的坑，这边没有。
   */
  pickupPassword: string;
  /** 单位是分。列表里没有这个字段，只有详情有。 */
  coffeeBalance: number;
  showVip: boolean;
  enableCouponVerification: boolean;
  warrantyEndAt: string | null;
  qrcodeType: string;
  regularQrcodePaymentMethod: string | null;
};

/** 设备列表（服务端分页）。keyword 由服务端按设备名 / 机器编码模糊匹配。 */
export async function listDevices(
  params?: PageQuery & { status?: string; keyword?: string; manufacturerId?: string },
) {
  return request<PageResult<DeviceSummary>>('/api/v1/merchant/devices', { params });
}

/** 设备详情。范围外与不存在都回 404，服务端不区分这两件事。 */
export async function getDevice(id: string) {
  return request<DeviceDetail>(`/api/v1/merchant/devices/${id}`);
}
