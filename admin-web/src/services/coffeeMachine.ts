import { request } from '@umijs/max';
import { FULL_PAGE_PARAMS } from './pagination';
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

/**
 * 饮品在列表里的形状，对应后端 dto.DrinkSummary。价格单位是分。
 *
 * deviceId 是**这一行自己的属性**：一行饮品就是「某台设备上的一杯」，价格也在这行上，
 * 没有一张单独的设备×饮品关系表。所以 null 的含义是「这行还没挂到设备上」——只有迁移
 * 之前留下的历史行会是这样，界面上显示成「未分配设备」。
 */
export type Drink = {
  id: string;
  deviceId: string | null;
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
  /**
   * 挂到哪台设备上。**新建时必填**——没有设备的饮品行卖不出去，后端会回
   * 「饮品必须挂到一台设备上」。可以是设备详情页当前那台（新建入口就在那一屏上）。
   *
   * 这里没有 manufacturerId：厂商跟着设备走，后端按 deviceId 现取设备上的那一列。
   * 传了也会被忽略（后端入参结构里没有这个字段），所以别在前端算一份出来。
   */
  deviceId?: string | null;
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

/** 编辑入参比创建少掉 originId：那是同步的自然键，改了匹配不上。 */
export type DrinkUpdateInput = Omit<DrinkInput, 'originId'>;

export type BalanceAdjustInput = {
  /** 有符号，正数加、负数减，0 不接受 */
  amount: number;
  /** 幂等键：同一个 requestId 重发只会记一次账 */
  requestId: string;
  remark?: string;
};

export type BalanceResult = { coffeeBalance: number };

/**
 * 一条设备余额流水，对应后端 dto.DeviceBalanceEntrySummary。金额单位是分。
 *
 * balanceBefore 不在表里（这张表只存变动后的余额），是后端用 balanceAfter - amount
 * 推出来的。前端不要再推一次：两处各推各的，迟早有一处把符号搞反。
 */
export type DeviceBalanceEntry = {
  id: string;
  deviceId: string;
  /** recharge=充值 / deduct=提货扣减 / adjust=后台调整 / reverse=冲正 */
  type: string;
  /** **有符号**：正数是加钱，负数是扣钱。不是流水金额的绝对值。 */
  amount: number;
  balanceBefore: number;
  balanceAfter: number;
  /** 冲正流水指向被冲正的那一条；其余类型为 null */
  reversesEntryId: string | null;
  referenceType: string;
  referenceId: string | null;
  requestId: string;
  remark: string;
  operatorId: string | null;
  /** 写入侧**刻意留空**（令牌里没有用户名），展示名要按 operatorId 现查 */
  operatorName: string;
  createdAt: string;
};

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

/**
 * 设备列表。storeIds 与 keyword 是筛选栏新增的两项。
 *
 * storeIds 是**多选**，序列化成重复键（storeIds=a&storeIds=b）。不能靠 axios 默认的
 * 数组序列化：它拼出来的是 storeIds[]=a，方括号是它自己加的，Go 那边看到的是一个叫
 * "storeIds[]" 的参数，取值永远为空——而后端把「没收到门店」理解为「不筛门店」，
 * 于是返回全量。表现是筛了跟没筛一样，一路没有报错。
 *
 * 品牌不在这里：品牌不是咖啡机库的列。列表页选完品牌会在前端换算成该品牌下的门店 id
 * 并进 storeIds，后端不认识品牌，也就不必为一个筛选项去问商户服务。
 */
export async function listDevices(
  params?: PageQuery & {
    manufacturerId?: string;
    storeIds?: string[];
    status?: DeviceStatus;
    /** 设备标识，后端按 serial_unique 模糊匹配 */
    keyword?: string;
  },
) {
  return request<PageResult<DeviceSummary>>('/api/v1/admin/coffee-machines/devices', {
    params,
    // 只在这一处需要：其余接口的参数里没有数组。
    paramsSerializer: repeatKeyParams,
  });
}

/** 把参数拼成 query string，数组按重复键展开。见 listDevices 的说明。 */
function repeatKeyParams(params: Record<string, unknown>): string {
  return Object.entries(params)
    .filter(([, value]) => value !== undefined && value !== null && value !== '')
    .flatMap(([key, value]) =>
      (Array.isArray(value) ? value : [value]).map(
        (item) => `${encodeURIComponent(key)}=${encodeURIComponent(String(item))}`,
      ),
    )
    .join('&');
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

/**
 * 某台设备的余额流水，服务端分页，最近的在前。
 *
 * 权限与调整余额共用同一个码（后端这两个方法挂在同一条路由表项上）：能看流水的人就是
 * 能调余额的人，所以详情页那个 tab 用 access.canAdjustCoffeeBalance 控制，不另开权限码。
 *
 * 设备不存在时回 404，与「这台设备一次都没动过余额」的空列表是两回事——详情页上这两句
 * 话完全不同。
 */
export async function listDeviceBalanceEntries(deviceId: string, params?: PageQuery) {
  return request<PageResult<DeviceBalanceEntry>>(
    `/api/v1/admin/coffee-machines/devices/${deviceId}/balance`,
    { params },
  );
}

// ——— 饮品 ———

/**
 * 某台设备上有哪些饮品。
 *
 * 不分页，一次给全：一台机器的饮品是个位数，内联改价那一屏本来也要整表在场
 * （翻页会把上一步的改动连同它的报错一起翻走）。
 *
 * 返回的与 GET /drinks 是同一个形状——饮品行自带 device_id，这一屏只是换了个入口
 * 去读同一张表的同一批行，没有一张单独的关系表。所以改价、上下架也走下面那两个
 * 「饮品自身」的写接口，不存在「改设备上的这杯」这种第二种写法。
 *
 * 设备不存在时后端回 404，与「这台设备配了零款饮品」的空列表是两回事——详情页
 * 上这两种情况要说的话完全不同。
 */
export async function listDeviceDrinks(deviceId: string) {
  return request<Drink[]>(`/api/v1/admin/coffee-machines/devices/${deviceId}/drinks`);
}

/**
 * 设备下拉用的全集。
 *
 * 上限就是 FULL_PAGE_PARAMS 的 200——设备数真的超过 200 时该做的是「远程搜索的
 * 设备下拉」（后端 listDevices 已经支持 keyword 按设备标识模糊查），那是另一件事；
 * 在那之前，这里和门店/厂商下拉是同一档取舍。列表接口本身仍是分页的。
 */
export async function listDeviceOptions() {
  const result = await listDevices(FULL_PAGE_PARAMS);
  return result.items;
}

export async function listDrinks(
  params?: PageQuery & {
    /** 只筛某台设备上的饮品；饮品管理页的设备筛选走它 */
    deviceId?: string;
    manufacturerId?: string;
    status?: DrinkStatus;
  },
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
