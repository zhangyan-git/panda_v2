import { request } from '@umijs/max';

// ——— 类型 ———

export type BrandStatus = 'active' | 'disabled';
export type AuditStatus = 'pending' | 'approved' | 'rejected';

export type Brand = {
  id: string;
  merchantId: string;
  merchantName: string;
  name: string;
  logo: string;
  banner: string;
  description: string;
  status: BrandStatus;
  auditStatus: AuditStatus;
  auditRemark: string;
  auditAt: string;
  auditBy: string;
  remark: string;
  visible: boolean;
  sort: number;
  createdAt: string;
};

export type BrandInput = {
  merchantId: string;
  name: string;
  logo?: string;
  banner?: string;
  description?: string;
  remark?: string;
  visible?: boolean;
  sort?: number;
};

// ——— 接口 ———

export async function listBrands(params?: {
  merchantId?: string;
  name?: string;
  status?: string;
  auditStatus?: string;
}) {
  return request<Brand[]>('/api/v1/admin/brands', { params });
}

export async function createBrand(data: BrandInput) {
  return request<Brand>('/api/v1/admin/brands', { method: 'POST', data });
}

export async function updateBrand(id: string, data: BrandInput) {
  return request<Brand>(`/api/v1/admin/brands/${id}`, { method: 'PUT', data });
}

export async function updateBrandStatus(id: string, status: BrandStatus) {
  return request(`/api/v1/admin/brands/${id}/status`, {
    method: 'PATCH',
    data: { status },
  });
}

/** 审核：仅 pending 可通过或拒绝 */
export async function auditBrand(id: string, approve: boolean, remark?: string) {
  return request(`/api/v1/admin/brands/${id}/audit`, {
    method: 'PATCH',
    data: { approve, remark },
  });
}

export async function deleteBrand(id: string) {
  return request(`/api/v1/admin/brands/${id}`, { method: 'DELETE' });
}
