import { request } from '@umijs/max';
import type { AuditStatus } from './brand';

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

export async function listStores(params?: {
  merchantId?: string;
  brandId?: string;
  name?: string;
  status?: string;
  auditStatus?: string;
}) {
  return request<Store[]>('/api/v1/admin/stores', { params });
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
