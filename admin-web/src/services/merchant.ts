import { request } from '@umijs/max';

// ——— 类型 ———

export type MerchantStatus = 'pending' | 'active' | 'suspended';

export type Merchant = {
  id: string;
  name: string;
  status: MerchantStatus;
  contactName: string;
  contactPhone: string;
  contactEmail: string;
  createdAt: string;
};

/** 数据范围单点三选一：merchant=整个商户 / brand=单品牌 / store=单门店（旗下数据全部可见） */
export type MerchantUserScopeType = 'merchant' | 'brand' | 'store';

export type MerchantUser = {
  id: string;
  username: string;
  name: string;
  email: string;
  phone: string;
  status: 'active' | 'disabled';
  isAdmin: boolean;
  scopeType: MerchantUserScopeType;
  scopeId: string;
  scopeName: string; // 联表计算列：范围品牌/门店名称
  lastLoginAt: string;
  createdAt: string;
};

// ——— 商户 ———

export async function listMerchants(params?: { name?: string; status?: string }) {
  return request<Merchant[]>('/api/v1/admin/merchants', { params });
}

export async function createMerchant(data: {
  name: string;
  contactName?: string;
  contactPhone?: string;
  contactEmail?: string;
}) {
  return request<Merchant>('/api/v1/admin/merchants', { method: 'POST', data });
}

export async function updateMerchant(
  id: string,
  data: {
    name: string;
    contactName?: string;
    contactPhone?: string;
    contactEmail?: string;
  },
) {
  return request<Merchant>(`/api/v1/admin/merchants/${id}`, { method: 'PUT', data });
}

/** 状态流转：pending→active 审核通过 / active→suspended 暂停 / suspended→active 恢复 */
export async function updateMerchantStatus(id: string, status: 'active' | 'suspended') {
  return request(`/api/v1/admin/merchants/${id}/status`, {
    method: 'PATCH',
    data: { status },
  });
}

export async function deleteMerchant(id: string) {
  return request(`/api/v1/admin/merchants/${id}`, { method: 'DELETE' });
}

// ——— 商户登录账号 ———

export async function listMerchantUsers(merchantId: string) {
  return request<MerchantUser[]>(`/api/v1/admin/merchants/${merchantId}/users`);
}

export async function createMerchantUser(
  merchantId: string,
  data: {
    username: string;
    password: string;
    name?: string;
    email?: string;
    phone?: string;
    isAdmin?: boolean;
    scopeType?: MerchantUserScopeType;
    scopeId?: string;
  },
) {
  return request<MerchantUser>(`/api/v1/admin/merchants/${merchantId}/users`, {
    method: 'POST',
    data,
  });
}

/** 调整账号数据范围（单点：merchant/brand/store，目标必须属于该商户） */
export async function updateMerchantUserScope(
  id: string,
  data: { scopeType: MerchantUserScopeType; scopeId?: string; isAdmin?: boolean },
) {
  return request(`/api/v1/admin/merchant-users/${id}/scope`, {
    method: 'PATCH',
    data,
  });
}

export async function updateMerchantUserStatus(id: string, status: 'active' | 'disabled') {
  return request(`/api/v1/admin/merchant-users/${id}/status`, {
    method: 'PATCH',
    data: { status },
  });
}

export async function deleteMerchantUser(id: string) {
  return request(`/api/v1/admin/merchant-users/${id}`, { method: 'DELETE' });
}
