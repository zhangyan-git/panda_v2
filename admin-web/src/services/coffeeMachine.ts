import { request } from '@umijs/max';
import type { PageQuery, PageResult } from './pagination';

// ——— 类型 ———

export type ManufacturerStatus = 'active' | 'disabled';
export type DeviceStatus = 'active' | 'disabled';
export type DrinkStatus = 'on_shelf' | 'off_shelf';
export type QrcodeType = 'miniprogram' | 'regular';
export type RegularQrcodePaymentMethod = 'fengxuan_wanlian' | 'youlian';
export type DrinkType = 'milk_coffee' | 'black_coffee' | 'other';

export type Manufacturer = {
  id: string;
  code: string;
  name: string;
  contactName: string;
  contactPhone: string;
  status: ManufacturerStatus;
};

/**
 * 设备在列表里的形状，对应后端 dto.DeviceSummary。
 *
 * 列表刻意不带版本号、二维码配置、余额和静态验证码（见后端 dto.DeviceDetail 的
 * 说明）：要什么就去详情拿，别指望这里多出一个字段。
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

/** 设备详情，对应后端 dto.DeviceDetail：列表的全部字段，加上编辑与余额要用的那些。 */
export type DeviceDetail = DeviceSummary & {
  versionNumber: string;
  androidVersion: string;
  mainBoardVersion: string;
  lastFaultAt: string | null;
  /**
   * 静态验证码明文。**不要拿它回填编辑表单**：编辑时留空表示「不改」，
   * 后端按 nil 处理，一旦回填就会把它当成待提交的新值。
   */
  pickupPassword: string;
  /** 单位是分。列表里没有这个字段，只有详情有。 */
  coffeeBalance: number;
  showVip: boolean;
  enableCouponVerification: boolean;
  warrantyEndAt: string | null;
  qrcodeType: QrcodeType;
  regularQrcodePaymentMethod: RegularQrcodePaymentMethod | null;
};

export type DeviceInput = {
  serialUnique: string;
  deviceName?: string;
  manufacturerId: string;
  storeId?: string | null;
  qrcodeType: QrcodeType;
  regularQrcodePaymentMethod?: string | null;
  /**
   * 编辑时**留空表示不改**，不是清空——后端没有「清空」这条路。不传或传空串
   * 都会走「那一列原样不动」。
   */
  pickupPassword?: string;
  showVip?: boolean;
  enableCouponVerification?: boolean;
  warrantyEndAt?: string;
};

/** 饮品在列表里的形状，对应后端 dto.DrinkSummary。价格单位是分。 */
export type Drink = {
  id: string;
  manufacturerId: string;
  /** 厂商侧的商品 ID，是同步的自然键，创建后不可改 */
  originId: string;
  productNum: string;
  productName: string;
  enName: string;
  drinkType: DrinkType | null;
  productImg: string;
  productDesc: string;
  price: number;
  vipPrice: number;
  pickupCodePrice: number;
  status: DrinkStatus;
  sort: number;
};

export type DrinkInput = {
  manufacturerId: string;
  originId?: string;
  productNum?: string;
  productName: string;
  enName?: string;
  drinkType?: DrinkType | null;
  productImg?: string;
  productDesc?: string;
  price?: number;
  vipPrice?: number;
  pickupCodePrice?: number;
  sort?: number;
};

/** 编辑入参比创建少掉厂商与 originId：那是同步的自然键，改了匹配不上。 */
export type DrinkUpdateInput = Omit<DrinkInput, 'manufacturerId' | 'originId'>;

export type BalanceAdjustInput = {
  /** 有符号，正数加、负数减，0 不接受 */
  amount: number;
  /** 幂等键：同一个 requestId 重发只会记一次账 */
  requestId: string;
  remark?: string;
};

export type BalanceResult = { coffeeBalance: number };

// ——— 厂商 ———

/** 厂商列表不分页：手动维护的个位数基础数据，所有下拉都要全集。 */
export async function listManufacturers() {
  return request<Manufacturer[]>('/api/v1/admin/coffee-machines/manufacturers');
}

export async function createManufacturer(data: {
  code: string;
  name: string;
  contactName?: string;
  contactPhone?: string;
}) {
  return request<Manufacturer>('/api/v1/admin/coffee-machines/manufacturers', {
    method: 'POST',
    data,
  });
}

/** 编辑不带 code：厂商编码创建后不可改，后端也没有这个入口。 */
export async function updateManufacturer(
  id: string,
  data: { name: string; contactName?: string; contactPhone?: string },
) {
  return request<Manufacturer>(`/api/v1/admin/coffee-machines/manufacturers/${id}`, {
    method: 'PUT',
    data,
  });
}

export async function updateManufacturerStatus(id: string, status: ManufacturerStatus) {
  return request(`/api/v1/admin/coffee-machines/manufacturers/${id}/status`, {
    method: 'PATCH',
    data: { status },
  });
}

// ——— 设备 ———

export async function listDevices(
  params?: PageQuery & {
    manufacturerId?: string;
    storeId?: string;
    status?: DeviceStatus;
  },
) {
  return request<PageResult<DeviceSummary>>('/api/v1/admin/coffee-machines/devices', { params });
}

/** 详情是编辑表单与余额调整的数据来源：列表那条 DeviceSummary 没有这些字段。 */
export async function getDevice(id: string) {
  return request<DeviceDetail>(`/api/v1/admin/coffee-machines/devices/${id}`);
}

export async function createDevice(data: DeviceInput) {
  return request<DeviceDetail>('/api/v1/admin/coffee-machines/devices', {
    method: 'POST',
    data,
  });
}

export async function updateDevice(id: string, data: DeviceInput) {
  return request<DeviceDetail>(`/api/v1/admin/coffee-machines/devices/${id}`, {
    method: 'PUT',
    data,
  });
}

export async function updateDeviceStatus(id: string, status: DeviceStatus) {
  return request(`/api/v1/admin/coffee-machines/devices/${id}/status`, {
    method: 'PATCH',
    data: { status },
  });
}

/**
 * 调整设备余额。返回调整后的余额，不必再查一次。
 *
 * 同一个 requestId 重复提交时后端回 409（而不是假装成功）：上一次多半已经记过账，
 * 只是响应没收到。到底记没记要自己去看流水，所以调用方要把 409 和普通失败分开处理。
 */
export async function adjustDeviceBalance(id: string, data: BalanceAdjustInput) {
  return request<BalanceResult>(`/api/v1/admin/coffee-machines/devices/${id}/balance`, {
    method: 'POST',
    data,
  });
}

// ——— 饮品 ———

export async function listDrinks(
  params?: PageQuery & { manufacturerId?: string; status?: DrinkStatus },
) {
  return request<PageResult<Drink>>('/api/v1/admin/coffee-machines/drinks', { params });
}

export async function createDrink(data: DrinkInput) {
  return request<Drink>('/api/v1/admin/coffee-machines/drinks', { method: 'POST', data });
}

export async function updateDrink(id: string, data: DrinkUpdateInput) {
  return request<Drink>(`/api/v1/admin/coffee-machines/drinks/${id}`, { method: 'PUT', data });
}

export async function updateDrinkStatus(id: string, status: DrinkStatus) {
  return request(`/api/v1/admin/coffee-machines/drinks/${id}/status`, {
    method: 'PATCH',
    data: { status },
  });
}
