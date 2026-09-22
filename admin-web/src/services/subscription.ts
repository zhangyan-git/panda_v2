import { request } from '@umijs/max';
import type { PageQuery, PageResult } from './pagination';

/**
 * 连续包月订阅（membership_subscriptions）的后台接口。字段名与后端 dto 的 json tag 逐字对应，
 * 同时也是页面上的 dataIndex——改一处要同批改三处。
 *
 * **这一份只有读，加「同步」与「取消」**，与后端那棵树一致：
 *
 * - 没有「新建订阅」：订阅只能由小程序端签约产生，老后台也建不了。
 * - 有「同步」：回渠道核一次这份代扣协议，按渠道的结论纠正本地订阅（见 syncSubscription）。
 * - 有「取消」：**它是一次渠道调用**——先让微信解约、成功了才改本地（见 cancelSubscription）。
 *
 * 列表为空仍然是可能的（还没有人在小程序里签过连续包月），它**不再等于「链路没接」**。
 */

// ——— 枚举 ———
// 取值来自 membership-service 的 model（也就是 001 迁移里的 CHECK）。加了新码而这里没登记，
// 界面上就退回显示原始码。

/** 订阅状态。`pending_sign` 是「已下单、等签约结果」的中间态——列表默认不显示它。 */
export type SubscriptionStatus =
  | 'pending_sign'
  | 'active'
  | 'suspended'
  | 'cancelled'
  | 'expired';

/**
 * 签约场景。**库里没有这一列**，是后端由三个来源 id 现推的（见 dto/subscription.go）：
 * 扫码点单时签的、扫店铺码参加活动时签的、在会员中心直接买的。
 */
export type SubscriptionScene = 'coffee_order' | 'store_campaign' | 'member_center';

// ——— 类型 ———

/** 一条订阅。 */
export type Subscription = {
  id: string;
  membershipId: string;
  userId: string;
  planId: string;
  /** 套餐名（后端 join 出来的）。页面上「会员等级」那一列就是它——V2 没有 VIP 等级这一层。 */
  planName: string;
  status: SubscriptionStatus;
  /** 签约时约定死的每期扣款金额，套餐后来调价不影响已签约的人。 */
  priceCents: number;
  period: 'month' | 'year';
  periodCount: number;
  signScene: SubscriptionScene;
  /** 首月那张订单。**只对 member_center 场景有值**，其余场景首月那笔钱不在订阅上。 */
  firstPaymentOrderId: string;
  chargeCount: number;
  failedCount: number;
  /** 连续失败次数（触发暂停用的）。与累计失败分开：「好了但历史失败过 5 次」是另一个问题。 */
  consecutiveFailedCount: number;
  nextChargeAt: string | null;
  lastChargeAt: string | null;
  suspendedAt: string | null;
  cancelAt: string | null;
  cancelReason: string;
  /** 解约发起人（后台账户 id）。用户自己在小程序点的、系统触发的都留空。 */
  cancelledBy: string;
  createdAt: string;
  updatedAt: string;
};

/**
 * 一期的代扣记录（后台「续费明细」那一行）。
 *
 * 来源是支付域的 `payment_agreement_charges`，经**本服务代理**出来——这一页的权限码是
 * `membership:read`，运营不该为了看这一块再要一枚 `payment:read`（见后端
 * dto.SubscriptionDetailResponse 的说明）。
 *
 * 四个时刻是**字符串**且空串表示「没有这个时刻」：未扣成的期次不该在界面上显示一个公元
 * 元年的日期。显示时直接给 `dash`，不要 Date 一次。
 */
export type SubscriptionCharge = {
  /** 期次（yyyyMMdd），也是这一期在渠道那边的业务标识。 */
  bizPeriod: string;
  /** 这一期**应收**的金额（分）。失败的那几期一分钱都没收到，但仍要显示应收多少。 */
  amount: number;
  /** pending / charging / succeeded / failed / skipped / cancelled，文案见 AGREEMENT_CHARGE_STATUS。 */
  status: string;
  /** 已经向渠道发起过几次。它比状态更能说明「这一期在反复重试」。 */
  attemptCount: number;
  /** 退避重试的下一次时刻。**扣款中/失败的行看着它才知道系统还在试**。 */
  nextRetryAt: string;
  /** 扣成的那一刻。 */
  chargedAt: string;
  createdAt: string;
  /** 渠道流水号，成功那一期就是它。出了争议时运营拿它去微信商户平台查这一笔。 */
  providerTransactionId: string;
  failureCode: string;
  /** 给人看的失败原因（「余额不足」这类）。成功与进行中的期次是空串。 */
  failureMessage: string;
};

/**
 * 首月那一笔的支付信息。**没有就是 undefined**（后端回 null），整块不渲染。
 *
 * 今天它恒为空：要小程序端把首月订单号写进订阅行，而小程序端还没接。所以这一段是照最终
 * 形态先建好的，不是有数据看不到。
 */
export type SubscriptionFirstPayment = {
  orderId: string;
  orderNo: string;
  /** 订单状态，取值同订单域的 order_status（paid / pending …）。 */
  status: string;
  /** 实付金额（分）。首月可能带优惠，所以它未必等于每期扣款额。 */
  paidAmount: number;
  /** 支付方式的 code，文案与订单页共用一份（paymentMethodLabels）。 */
  paymentMethod: string;
  /** 订单上那个支付单号。空串表示这张单还没发起过支付。 */
  paymentNo: string;
  /**
   * 渠道流水号（首月那笔的微信流水）。**这一块里唯一要绕两跳的值**：订单 → payment_no →
   * 支付单 → 渠道流水；任一跳断掉它留空，其余几格照常显示。
   */
  providerTransactionId: string;
  paidAt: string;
};

/**
 * 订阅详情的形状：列表那一条的所有字段（摊平在同一层）+ 三块回显。
 *
 * **detail 与列表不是同一个接口**：列表那一行只带得动列表要用的字段，这一份要多跑两次跨服务
 * 只读（支付域的期次、订单域的首月订单），所以只在抽屉打开时取一次。
 */
export type SubscriptionDetail = Subscription & {
  /** 渠道那份代扣协议的协议号。本库的列，一定有值——**除了活动发放与后台开通那两条路**（它们没有渠道协议），那两种情况下是空串。 */
  contractCode: string;
  /** 这一份协议下的扣款期次，按期次升序。**读不到时是空数组，不是 null**，所以可以直接 .length。 */
  charges: SubscriptionCharge[];
  /** 首月支付信息。**没有首月订单时整个是 undefined**。 */
  firstPayment?: SubscriptionFirstPayment;
};

/** 列表页头上那两张卡。 */
export type SubscriptionStats = {
  /** 只数 active：suspended 在等重试，混进来会让「现在有多少人在正常续费」虚高。 */
  activeCount: number;
  /** 已经到点该扣、但还没扣的条数。 */
  dueCount: number;
};

export type SubscriptionQuery = PageQuery & {
  /** 精确匹配用户 ID。客服接到「为什么我这个月没扣款」，手上只有这个。 */
  userId?: string;
  /**
   * 空 = 不筛，而**「不筛」是不等于 pending_sign**（不是不过滤）——一屏等着用户去微信点完
   * 的单子对运营没有任何用处。想看它们得显式选这一项。
   */
  status?: SubscriptionStatus;
};

// ——— 接口 ———

export async function listSubscriptions(params?: SubscriptionQuery) {
  return request<PageResult<Subscription>>('/api/v1/admin/membership-subscriptions', { params });
}

/**
 * 两张统计卡。单独一个请求，不跟列表一起回：翻页时这两个数与当前页无关，跟页面走会让第一页
 * 之后每次都白算一遍全表。
 */
export async function getSubscriptionStats() {
  return request<SubscriptionStats>('/api/v1/admin/membership-subscriptions/stats');
}

/**
 * 一条订阅的详情。比列表多三块：协议号、续费明细、首月支付信息。
 *
 * 那三块里的后两块是**跨服务的只读代理**（支付域、订单域）。它们读不到时后端**不报错**——
 * 整页照常返回，只有那一块空着：这一页要回答的是「谁签的、下一次什么时候扣」，那在本库里。
 * 所以运营看到「暂无续费记录」时，未必要当成故障。
 */
export async function getSubscription(id: string) {
  return request<SubscriptionDetail>(`/api/v1/admin/membership-subscriptions/${id}`);
}

/** 「同步」的应答。 */
export type SubscriptionSyncResult = {
  subscription: Subscription;
  /**
   * 这一次有没有真的改到本地。false 有两种正常情形：渠道说还在等用户点同意（`providerState`
   * 是 pending），或者本地早就是渠道说的那个状态了。所以**它不是失败**，提示语要分开说。
   */
  changed: boolean;
  /** 渠道的原话（signed / pending / terminated），不翻译——「同步了但没变」时下一句就要问它。 */
  providerState: string;
};

/**
 * 同步一条订阅：拿这条订阅的协议号回渠道核一次，按渠道的结论纠正本地。
 *
 * 两个结果都是成功，说给运营的话不一样：`changed` 为真才是「已同步」，为假是「无需更正」
 * （渠道还在等用户点同意，或者本地早就是那个状态了）。
 *
 * **点几次都安全**：它不产生任何财产后果——把一条订阅从 pending_sign 变成 active 只是让「以后
 * 可以扣款」在本地成立，而真正扣钱是按 nextChargeAt 由后端扫描发起的（见后端
 * service/subscription.go 的说明）。
 */
export async function syncSubscription(id: string) {
  return request<SubscriptionSyncResult>(`/api/v1/admin/membership-subscriptions/${id}/sync`, {
    method: 'POST',
  });
}

/**
 * 取消一条订阅。**先让微信把代扣协议解掉，成功了才改本地**。
 *
 * 所以它有两种失败，页面上要说清的是后一种：渠道没答上来（503，重试即可），以及**微信明确
 * 不同意解约**（409）——那一种后端一个字节都没改，本地状态原样，运营该做的是稍后再点一次。
 * 顺序反过来写（先改本地再去解约）留下的就是「后台显示已取消、微信下个月照扣」。
 *
 * 原因此处必填：这是别人钱袋子上的一次人工操作，事后的留痕里不该有一句系统替他写的话。
 * 它会同时进本地留痕与报给微信的解约备注。
 *
 * 只有 `active` / `suspended` 能取消，其余状态后端回 409。
 */
export async function cancelSubscription(id: string, reason: string) {
  return request<Subscription>(`/api/v1/admin/membership-subscriptions/${id}/cancel`, {
    method: 'POST',
    data: { reason },
  });
}
