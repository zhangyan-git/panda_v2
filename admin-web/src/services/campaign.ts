import { request } from '@umijs/max';
import type { PageQuery, PageResult } from './pagination';

/**
 * 店铺码会员活动的后台接口。字段名与后端 dto 的 json tag 逐字对应，同时也是页面上的
 * dataIndex——改一处要同批改三处。
 *
 * 一场活动 = 一个门店 + 一款连续包月套餐 + 送多少天（+ 可选的一次赠券）。用户扫门店里那张小
 * 程序码（码上带一串 scene），本服务靠它找回活动、发一次会员天数，并发出 `membership.campaign
 * .claimed` 让 coupon-service 发券。
 *
 * **券是可选的**：`couponTemplateId` 为空 + `couponCount` 为 0 = 这场活动只送会员天数。两栏
 * 要么都填、要么都不填，只填一半后端会拒（那等于「该发券」静默变成「什么都不发」）。模板列表
 * 走券服务的接口，且只列平台核销的券（见页面里的说明）。
 *
 * 两件今天没有、将来才有的东西：
 *
 * - **没有「生成小程序码」**：那要走微信的 wxacode.getUnlimited，只有拿着 appid/secret 的
 *   user-service 发得出来。后端连路由都没挂，前端也就没有这个按钮。
 * - **领取记录一定是空的**：领取要用户在门店里扫码，扫码入口在小程序端，而那一端还没接。
 *   页面上有一句话说明。
 */

/** 活动状态。draft 是「还没配完」，只有 enabled 且落在起止窗口里的活动能被扫到。 */
export type CampaignStatus = 'draft' | 'enabled' | 'disabled';

/** 一场活动。 */
export type Campaign = {
  id: string;
  /** 给运营看的名字，不出现在码里。 */
  name: string;
  /** 小程序码带的参数，扫码进来靠它找回活动。全库唯一，且**启用后不给改**。 */
  scene: string;
  storeId: string;
  /**
   * 门店名，后端**现解出来的**（会员库只存门店 id）。解不出来时是空串——商户域抖一下不该让
   * 整页打不开，所以显示时要按「空 ⇒ 显示 — 」处理。
   */
  storeName: string;
  planId: string;
  planName: string;
  /** 送多少天。与套餐的时长无关：这是一次赠送，按天算。 */
  giftDays: number;
  startAt: string;
  endAt: string;
  status: CampaignStatus;
  /**
   * 每领一次送的券。**空串 + 0 = 只送会员天数**，库里那两列是 NULL、后端归一化成零值。
   *
   * 它是**承诺，不是结果**：实际发了几张在券库（`user_coupons.campaign_claim_id`），这一页
   * 答不了那个问题。模板 id 是跨库值引用，后端存之前查不出它存不存在——配错了要等发券那一刻
   * 由券服务记一条日志跳过。
   */
  couponTemplateId: string;
  couponCount: number;
  /**
   * 小程序码图片地址。**今天恒为空**：生成码要走微信、而本服务没有微信配置，页面也不提供
   * 生成入口（见文件头）。它描述的是活动的事实，没有码的时候空着，语义清楚。
   */
  qrCodeUrl: string;
  qrCodeGeneratedAt: string | null;
  createdAt: string;
  updatedAt: string;
};

/** 一条领取记录。 */
export type CampaignClaim = {
  id: string;
  campaignId: string;
  userId: string;
  /** 发放那一刻的门店与天数快照：活动后来改了，这一次不受影响。 */
  storeId: string;
  giftDays: number;
  /** 券同样是快照，且是**承诺**：实际发出去几张要去券库按 campaignClaimId 数。 */
  couponTemplateId: string;
  couponCount: number;
  membershipId: string;
  /** 领到哪天。会员行上的到期时间会随后台调整、续费而变，这里记的是当时送到哪天。 */
  membershipExpireAt: string;
  createdAt: string;
};

export type CampaignQuery = PageQuery & {
  /** 空 = 不筛。 */
  status?: CampaignStatus;
  /** 模糊搜：活动名或 scene。运营手里那半截可能是「五一活动」，也可能是码上那串 smc_xxx。 */
  keyword?: string;
};

/** 新建 / 修改的请求体。两者共用同一个形状与同一套校验。 */
export type CampaignPayload = {
  name: string;
  scene: string;
  storeId: string;
  planId: string;
  giftDays: number;
  startAt: string;
  endAt: string;
  /** 券：**要么都填、要么都不填**（空串 + 0 = 不送券）。只填一半后端回 400。 */
  couponTemplateId: string;
  couponCount: number;
};

// ——— 接口 ———

export async function listCampaigns(params?: CampaignQuery) {
  return request<PageResult<Campaign>>('/api/v1/admin/membership-campaigns', { params });
}

export async function getCampaign(id: string) {
  return request<Campaign>(`/api/v1/admin/membership-campaigns/${id}`);
}

/** 新建。**一律建成 draft**：还没配完的活动被扫到，用户领到的是半成品。 */
export async function createCampaign(data: CampaignPayload) {
  return request<Campaign>('/api/v1/admin/membership-campaigns', { method: 'POST', data });
}

/**
 * 修改。**它不改状态**——启停走 setCampaignStatus，否则运营改一句活动名就会把正开着的活动
 * 顺手存回 draft，而那一刻可能正有人拿着码在扫。
 *
 * 活动处于 enabled 时改 scene / storeId 会被后端拒（409）：scene 进了码，改它会让已经印出去
 * 的码静默失效；门店是归属门店，改了等于把已经领过的人的归属挪走。要改先停用。
 */
export async function updateCampaign(id: string, data: CampaignPayload) {
  return request<Campaign>(`/api/v1/admin/membership-campaigns/${id}`, { method: 'PUT', data });
}

export async function setCampaignStatus(id: string, status: CampaignStatus) {
  return request<Campaign>(`/api/v1/admin/membership-campaigns/${id}/status`, {
    method: 'POST',
    data: { status },
  });
}

export async function listCampaignClaims(id: string, params?: PageQuery) {
  return request<PageResult<CampaignClaim>>(`/api/v1/admin/membership-campaigns/${id}/claims`, {
    params,
  });
}
