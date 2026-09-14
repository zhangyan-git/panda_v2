import { request } from '@umijs/max';

// 优惠券接口的字段名与商户/身份那套一致，全部是 camelCase。
// 这套名字同时是后端的 json tag、前端的 dataIndex 和查询参数键：改一处必须同批
// 改三处，否则 TypeScript 不会报错，页面只是整列空白。

export type CouponType = {
  id: string;
  code: string;
  name: string;
  description: string;
  status: 'active' | 'disabled';
  createdAt: string;
  updatedAt: string;
};

export type CouponTemplate = {
  id: string;
  couponTypeId: string;
  merchantId?: string;
  name: string;
  shortTitle: string;
  description: string;
  // 金额单位一律是「分」（整数）。这一层不做换算：接口给什么就是什么，
  // 页面在录入/展示时按元转换（见 pages/coupon-templates 的 fen/yuan）。
  faceValue: number;
  minPurchaseAmount: number;
  purchasePrice: number;
  totalQuantity: number;
  validityMode: 'fixed' | 'relative';
  validFrom?: string;
  validTo?: string;
  validDays?: number;
  // 后端模板校验要求 claimLimitMode 取 once_ever/unlimited_after_use/periodic，
  // redemptionType 取 platform/external_code/show_qr，缺任一项创建都会 400。
  claimLimitMode: 'once_ever' | 'unlimited_after_use' | 'periodic';
  redemptionType: 'platform' | 'external_code' | 'show_qr';
  auditStatus: 'pending' | 'approved' | 'rejected';
  status: 'draft' | 'active' | 'disabled' | 'closed';
  visible: boolean;
  createdAt: string;
  // 适用范围。「商户」那层是上面的 merchantId（单选，null/undefined = 平台券）；
  // 这两个是品牌/门店两层，空数组 = 该层不限。
  brandIds: string[];
  storeIds: string[];
};

export type CouponBatch = {
  id: string;
  templateId: string;
  batchNo: string;
  source: string;
  totalQuantity: number;
  reservedQuantity: number;
  issuedQuantity: number;
  releasedQuantity: number;
  status: string;
  orderId?: string;
  userId?: string;
  requestId?: string;
  createdBy?: string;
  createdAt: string;
  updatedAt: string;
};

export type CouponBatchQuery = {
  page?: number;
  pageSize?: number;
  templateId?: string;
  status?: string;
  source?: string;
  batchNo?: string;
};

export type CouponBatchListResponse = {
  items: CouponBatch[];
  total: number;
};

export type UserCouponStatus = 'claimed' | 'held' | 'redeemed' | 'expired' | 'refunded' | 'invalidated';

export type UserCoupon = {
  id: string;
  templateId: string;
  batchId?: string;
  userId: string;
  couponTypeCode: string;
  claimType: string;
  issueReason?: string;
  status: UserCouponStatus | string;
  faceValue: number;
  minPurchaseAmount: number;
  redemptionType?: string;
  validFrom: string;
  expiredAt: string;
  claimedAt: string;
  heldAt?: string;
  redeemedAt?: string;
  refundedAt?: string;
  invalidatedAt?: string;
  createdAt?: string;
  updatedAt?: string;
};

export type UserCouponQuery = {
  page?: number;
  pageSize?: number;
  userId?: string;
  status?: UserCouponStatus;
  batchId?: string;
  templateId?: string;
};

export type UserCouponListResponse = { items: UserCoupon[]; total: number };
export type UserCouponActionInput = { reason?: string };

export type TemplateInput = {
  couponTypeId: string;
  merchantId?: string;
  name: string;
  shortTitle?: string;
  description?: string;
  // 提交的也是「分」。传字符串或小数后端会在 JSON 解码阶段直接 400，
  // 所以这里必须是 number。
  faceValue: number;
  minPurchaseAmount?: number;
  purchasePrice?: number;
  totalQuantity: number;
  validityMode: 'fixed' | 'relative';
  validFrom?: string;
  validTo?: string;
  validDays?: number;
  claimLimitMode: 'once_ever' | 'unlimited_after_use' | 'periodic';
  redemptionType?: 'platform' | 'external_code' | 'show_qr';
  visible?: boolean;
  // PUT 是全量覆盖：这两个字段不发就等于清空已有范围（空数组 = 不限）。
  brandIds?: string[];
  storeIds?: string[];
};

export type IssueCouponsInput = {
  templateId: string;
  userIds: string[];
  quantityPerUser: number;
  reason?: string;
  skipLimitValidation?: boolean;
};

export async function listCouponTypes(params?: Record<string, unknown>) {
  return request<CouponType[]>('/api/v1/admin/coupons/types', { params });
}
export async function createCouponType(data: Pick<CouponType, 'code' | 'name' | 'description' | 'status'>) {
  return request<CouponType>('/api/v1/admin/coupons/types', { method: 'POST', data });
}
export async function updateCouponType(id: string, data: Partial<Pick<CouponType, 'name' | 'description' | 'status'>>) {
  return request<CouponType>(`/api/v1/admin/coupons/types/${id}`, { method: 'PUT', data });
}

export async function listCouponTemplates(params?: Record<string, unknown>) {
  return request<{ items: CouponTemplate[]; total: number }>('/api/v1/admin/coupons/templates', { params });
}
export async function createCouponTemplate(data: TemplateInput) {
  return request<CouponTemplate>('/api/v1/admin/coupons/templates', { method: 'POST', data });
}
export async function updateCouponTemplate(id: string, data: Partial<TemplateInput>) {
  return request<CouponTemplate>(`/api/v1/admin/coupons/templates/${id}`, { method: 'PUT', data });
}
export async function auditCouponTemplate(id: string, status: 'approved' | 'rejected', remark?: string) {
  return request(`/api/v1/admin/coupons/templates/${id}/audit`, { method: 'PATCH', data: { status, remark } });
}
export async function updateCouponTemplateStatus(id: string, status: CouponTemplate['status']) {
  return request(`/api/v1/admin/coupons/templates/${id}/status`, { method: 'PATCH', data: { status } });
}

export async function issueCoupons(data: IssueCouponsInput) {
  return request<{ batchId: string; issuedQuantity: number; userCouponIds: string[] }>(
    '/api/v1/admin/coupons/issue',
    { method: 'POST', headers: { 'Idempotency-Key': crypto.randomUUID() }, data },
  );
}
export async function listCouponBatches(params?: CouponBatchQuery) {
  return request<CouponBatchListResponse>('/api/v1/admin/coupons/batches', { params });
}
export async function listUserCoupons(params?: UserCouponQuery) {
  return request<UserCouponListResponse>('/api/v1/admin/coupons/user-coupons', { params });
}
export async function getUserCoupon(id: string) {
  return request<UserCoupon>(`/api/v1/admin/coupons/user-coupons/${id}`);
}
export async function redeemUserCoupon(id: string, data: UserCouponActionInput = {}) {
  return request<UserCoupon>(`/api/v1/admin/coupons/user-coupons/${id}/redeem`, {
    method: 'POST',
    headers: { 'Idempotency-Key': crypto.randomUUID() },
    data,
  });
}
export async function revokeUserCoupon(id: string, data: UserCouponActionInput = {}) {
  return request<UserCoupon>(`/api/v1/admin/coupons/user-coupons/${id}/revoke`, {
    method: 'POST',
    headers: { 'Idempotency-Key': crypto.randomUUID() },
    data,
  });
}
