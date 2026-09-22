import { request } from '@umijs/max';
import type { PageQuery, PageResult } from './pagination';

/**
 * 商户端门店接口。字段名与 merchant-service handler 里的 storeResponse 的 json tag 逐字
 * 对应，同时也是页面上的 dataIndex——改一处要同批改三处，否则 TypeScript 不报错，只是整列空白。
 *
 * 这里**没有** merchantId 之类的入参：查询串能指定商户就等于把数据范围交给了调用方。
 * 服务端按请求令牌实时解析出来的授权点位集合过滤，前端连表达「我要看哪个商户」的能力都
 * 不该有（见 merchant-service 的 MerchantStoreHandler）。
 */

export type StoreStatus = 'active' | 'disabled';
export type AuditStatus = 'pending' | 'approved' | 'rejected';

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

/** 门店列表（服务端分页）。只认 name 与 status 两个展示性过滤，与数据范围取交集。 */
export async function listStores(params?: PageQuery & { name?: string; status?: string }) {
  return request<PageResult<Store>>('/api/v1/merchant/stores', { params });
}

/**
 * 门店详情。
 *
 * 范围外与不存在回的都是 404（服务端刻意不区分，见 MerchantStoreHandler 的说明），所以调用方
 * 拿到的只有「这一条我看不到」这一个结论，界面也只能这么写。
 */
export async function getStore(id: string) {
  return request<Store>(`/api/v1/merchant/stores/${id}`);
}
