import { request } from '@umijs/max';
import type { AuditStatus } from './brand';
import type { PageQuery, PageResult } from './pagination';

// ——— 类型 ———

export type StoreStatus = 'active' | 'disabled';

export type Store = {
  id: string;
  merchantId: string;
  merchantName: string;
  brandId: string;
  brandName: string;
  name: string;
  logo: string;
  photos: string[];
  province: string;
  city: string;
  district: string;
  // 区划编码与名称并存：名字给人看，编码用来定位与迁移。历史行为空串。
  provinceCode: string;
  cityCode: string;
  districtCode: string;
  // 订货系统的客户三列（merchant/004）。两个编码是跟供应商、DMS 对账的业务键，
  // 库上各有「空串不参与」的部分唯一索引——空串表示还没编码，重复的非空编码会被拒。
  customerCode: string;
  dmsCode: string;
  customerType: string;
  address: string;
  longitude: number | null;
  latitude: number | null;
  phone: string;
  contactName: string;
  contactPhone: string;
  detail: string;
  businessHours: string;
  status: StoreStatus;
  auditStatus: AuditStatus;
  auditRemark: string;
  auditAt: string;
  auditBy: string;
  remark: string;
  visible: boolean;
  createdAt: string;
};

export type StoreInput = {
  merchantId: string;
  brandId: string;
  name: string;
  logo?: string;
  photos?: string[];
  province?: string;
  city?: string;
  district?: string;
  provinceCode?: string;
  cityCode?: string;
  districtCode?: string;
  /** 客户三列；不填即空串（= 还没编码），不是清空 */
  customerCode?: string;
  dmsCode?: string;
  customerType?: string;
  address?: string;
  longitude?: number | null;
  latitude?: number | null;
  phone?: string;
  contactName?: string;
  contactPhone?: string;
  detail?: string;
  businessHours?: string;
  remark?: string;
  visible?: boolean;
};

// ——— 接口 ———

/** 门店列表（服务端分页）。 */
export async function listStores(
  params?: PageQuery & {
    merchantId?: string;
    brandId?: string;
    name?: string;
    status?: string;
    auditStatus?: string;
  },
) {
  return request<PageResult<Store>>('/api/v1/admin/stores', { params });
}

/**
 * 单个门店。
 *
 * 出库单详情要用它把 `store_id` 换成一个店名——库存库里只存值引用，没有店名，
 * 与订单域补门店名的做法一致（都是拿 id 回来现查，不做联表也不落冗余）。
 *
 * 刻意不写「查不到就给个占位名」的兜底：门店被删之后这里会 404，而「查不到」
 * 与「名字是空串」必须分得开——前者说明这条引用已经悬空，是要报出来的事实。
 */
export async function getStore(id: string) {
  return request<Store>(`/api/v1/admin/stores/${id}`);
}

export async function createStore(data: StoreInput) {
  return request<Store>('/api/v1/admin/stores', { method: 'POST', data });
}

export async function updateStore(id: string, data: StoreInput) {
  return request<Store>(`/api/v1/admin/stores/${id}`, { method: 'PUT', data });
}

export async function updateStoreStatus(id: string, status: StoreStatus) {
  return request(`/api/v1/admin/stores/${id}/status`, {
    method: 'PATCH',
    data: { status },
  });
}

/** 审核：仅 pending 可通过或拒绝 */
export async function auditStore(id: string, approve: boolean, remark?: string) {
  return request(`/api/v1/admin/stores/${id}/audit`, {
    method: 'PATCH',
    data: { approve, remark },
  });
}

export async function deleteStore(id: string) {
  return request(`/api/v1/admin/stores/${id}`, { method: 'DELETE' });
}
