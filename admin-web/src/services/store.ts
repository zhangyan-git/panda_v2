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
  // 订货系统的客户三列（migrations/merchant）。两个编码是跟供应商、DMS 对账的业务键，
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

// 这里原来有一个 getStore(id)：它是**出库单详情**用来把 store_id 换店名的。订货/库存域
// 2026-09-22 整体删除之后，全仓再没有调用点——门店详情页本来
// 就在列表行上直接跳 `/stores/:id`，不需要再查一次。留着它是等着下一个人以为有页面在用。

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
