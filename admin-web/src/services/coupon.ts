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
  // 封面图与使用规则说明：接口一直有，以前没登记，详情里要摆出来。
  coverImage: string;
  useRuleDescription: string;
  // 金额单位一律是「分」（整数）。这一层不做换算：接口给什么就是什么，
  // 页面在录入/展示时按元转换（见 pages/coupon-templates 的 fen/yuan）。
  faceValue: number;
  minPurchaseAmount: number;
  purchasePrice: number;
  totalQuantity: number;
  // 已发行 / 已预留。列表上的「剩余」由这三个算，接口没有单独的剩余字段，
  // 因为剩余是导出量（total - issued - reserved），存一份就会有对不上的时候。
  issuedQuantity: number;
  reservedQuantity: number;
  validityMode: 'fixed' | 'relative';
  validFrom?: string;
  validTo?: string;
  validDays?: number;
  // 后端模板校验要求 claimLimitMode 取 once_ever/unlimited_after_use/periodic，
  // redemptionType 取 platform/external_code/show_qr，缺任一项创建都会 400。
  claimLimitMode: 'once_ever' | 'unlimited_after_use' | 'periodic';
  // 只有 claimLimitMode='periodic' 时才有值，库里那两条 CHECK 保证互斥。
  claimPeriodUnit?: string;
  claimPeriodQuantity?: number;
  redemptionType: 'platform' | 'external_code' | 'show_qr';
  // 外部核销方式，同样只对部分 redemptionType 有意义（接口带 omitempty）。
  externalUseMethod?: string;
  auditStatus: 'pending' | 'approved' | 'rejected';
  auditRemark: string;
  auditedAt?: string;
  auditedBy?: string;
  status: 'draft' | 'active' | 'disabled' | 'closed';
  isHot: boolean;
  isRecommended: boolean;
  sortOrder: number;
  visible: boolean;
  createdBy?: string;
  createdAt: string;
  updatedAt: string;
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
  // 用户券自身的 id：客服拿着顾客发来的券号直接定位那一张。
  id?: string;
  // coupon_types.code（user_coupons 里存的是发券时抄下来的副本）。搜索下拉给的是
  // 中文名，发出去的仍是编码，所以这里的类型是 string 而不是枚举字面量。
  couponTypeCode?: string;
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
// 详情单独取一次，不拿列表那一行凑合：已发行/剩余是会被人改动的数（别人刚发过券），
// 列表数据在打开抽屉的那一刻可能已经旧了。
export async function getCouponTemplate(id: string) {
  return request<CouponTemplate>(`/api/v1/admin/coupons/templates/${id}`);
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
