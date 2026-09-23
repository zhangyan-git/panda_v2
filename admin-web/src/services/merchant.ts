import { request } from '@umijs/max';
import type { PageQuery, PageResult } from './pagination';

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

/** 数据范围档位三选一：merchant=整个商户 / brand=指定品牌 / store=指定门店（旗下数据全部可见） */
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
  /** 范围目标：品牌档是品牌 id，门店档是门店 id，可以多个；商户档为空数组 */
  scopeIds: string[];
  /** 与 scopeIds 同序等长：查不到名字的目标占一个空串。商户档是空数组 */
  scopeNames: string[];
  lastLoginAt: string;
  createdAt: string;
};

// ——— 商户 ———

/** 商户列表（服务端分页）。品牌/门店页的商户下拉要全集，见 FULL_PAGE_PARAMS。 */
export async function listMerchants(params?: PageQuery & { name?: string; status?: string }) {
  return request<PageResult<Merchant>>('/api/v1/admin/merchants', { params });
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

/** 某商户的登录账号（服务端分页），商户抽屉里的二级表用。 */
export async function listMerchantUsers(merchantId: string, params?: PageQuery) {
  return request<PageResult<MerchantUser>>(`/api/v1/admin/merchants/${merchantId}/users`, { params });
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
    scopeIds?: string[];
  },
) {
  return request<MerchantUser>(`/api/v1/admin/merchants/${merchantId}/users`, {
    method: 'POST',
    data,
  });
}

/** 调整账号数据范围（merchant/brand/store 三档，品牌与门店档可以多个，每个目标都必须属于该商户） */
export async function updateMerchantUserScope(
  id: string,
  data: { scopeType: MerchantUserScopeType; scopeIds?: string[]; isAdmin?: boolean },
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
