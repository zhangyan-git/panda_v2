import { request } from '@umijs/max';
import type { PageQuery, PageResult } from './pagination';

/**
 * 会员域的接口。字段名与后端 dto（membership-service/internal/dto）的 json tag 逐字对应，
 * 同时也是页面上的 dataIndex——改一处要同批改三处（tag、这里、ProTable 的列），
 * 否则 TypeScript 不报错，只是整列空白。
 *
 * 后端那个包的文件头点名了这一份与小程序会员中心的字段名，说的就是这条约定。
 *
 * 边界先说清楚：**本域只有两样东西**——「卖了什么」（套餐）与「谁在会员中」（会员资格）。
 * 会员是**在别处成交、在这里生效**：买会员那张单在 order-service，会员价的券在
 * coupon-service，代扣签约在微信。所以这个文件里没有订单、没有发券、没有签约，也别加。
 *
 * 连续包月订阅（`membership_subscriptions`）**不在这一份里**，它有自己的
 * services/subscription.ts：那一页是可看的（列表 / 统计 / 详情 + 一个「取消」），但同样
 * **没有创建**——订阅只能由小程序端签约产生。两件事的权限码也分开了（订阅取消是
 * membership:manage，不是 adjust）。
 */

// ——— 枚举 ———
// 取值来自 membership-service model 的常量，也就是 migrations/membership 的 CHECK 约束。
// 接口回的就是库里那些英文码。**改枚举必须同时改迁移和这里**，加了新码而这里没登记，
// 界面上就退回显示原始码。文案表在 services/membershipLabels.ts。

/**
 * membership_plans.status：这个套餐现在卖不卖。
 *
 * **新建的套餐一律是 draft**（CreatePlanRequest 里根本没有 status 字段），要卖得再调一次
 * 上下架接口。三个取值之间没有状态机，任意跳转都行。
 */
export type PlanStatus = 'draft' | 'active' | 'disabled';

/** 每期时长的单位。与 periodCount 一起读才对：「month/1」是连续包月，「year/1」是年度会员。 */
export type PlanPeriod = 'month' | 'year';

/**
 * membership_plans.member_price_mode：会员价从哪来。
 *
 * auto 是「会员本人自动享」，coupon 是「靠会员价体验券」——后者要配券模板与每期张数。
 * 它决定本域那句最要紧的话怎么说：是「你已享会员价」还是「你有 N 张会员价券」。
 */
export type MemberPriceMode = 'auto' | 'coupon';

/**
 * memberships.status：一个人现在的会员资格处在哪一步。
 *
 * frozen 是可逆的（解冻回 active），revoked **不可逆**——所以两者的按钮再像也不能合并。
 * expired 不由后台产生：它只由到期扫描写，后台没有「标记为过期」这个动作。
 */
export type MembershipStatus = 'active' | 'frozen' | 'expired' | 'revoked';

/**
 * membership_changes.change_type：会员身上发生过的一件事。
 *
 * 十四个取值里有几个今天写不出来（subscribe / unsubscribe 要签约链路、refund_adjust 要退款单、
 * expire 要到期扫描），照登是因为迁移里的 CHECK 已经把它们写全了——登记一个今天到不了的码，
 * 好过将来那一格原样显示 `unsubscribe`。
 *
 * charge_failed 与 suspend 来自**已经落地**的代扣链路，两个都写得出来：一期扣款
 * 没扣到写前者（到期日不动），连续失败到上限、停掉自动续费写后者。漏登它们的后果是时间线与
 * 筛选下拉里直接显示英文码。
 */
export type ChangeType =
  | 'activate'
  | 'renew'
  | 'expire'
  | 'freeze'
  | 'unfreeze'
  | 'auto_renew_on'
  | 'auto_renew_off'
  | 'subscribe'
  | 'unsubscribe'
  | 'charge_failed'
  | 'suspend'
  | 'refund_adjust'
  | 'admin_adjust'
  | 'revoke';

/** 这次变动是谁做的。system 是「没有任何人做」——到期扫描那一条就是它。 */
export type MembershipOperatorType = 'user' | 'admin' | 'system' | 'worker';

// ——— 套餐 ———

/**
 * 一个会员套餐。
 *
 * 它是**套餐定义**，不是某个人的会员：这里的 priceCents / period 是「现在买要多少钱、买多久」。
 * 已经买过的人的条款在会员行的快照列上，改这个结构不会动到他们——改价改时长对已购会员
 * 没有影响，这一点是后台敢放开编辑的原因。
 */
export type MembershipPlan = {
  id: string;
  /** 稳定标识，创建后不可改（库上有触发器钉着）。只在新建表单里出现。 */
  code: string;
  name: string;
  description: string;
  /** 「每月 20 次咖啡享会员价」这类卖点，一行一条。 */
  benefits: string[];
  priceCents: number;
  period: PlanPeriod;
  periodCount: number;
  /**
   * 这个**产品**支不支持自动续费（要签微信委托代扣）。
   *
   * 与会员行上的 autoRenew 不是一回事：那个说「这个人现在开着」。套餐今天关掉自动续费，
   * 不等于当初买了它的人不支持。
   */
  autoRenew: boolean;
  /** 微信商户平台的签约模板 ID。autoRenew 为真时必填，为假时必须留空。 */
  wechatPlanId: string;
  memberPriceMode: MemberPriceMode;
  /** 只在 coupon 模式下有值（库上的 CHECK 钉着这对关系）。 */
  memberPriceCouponTemplateId: string;
  memberPriceCouponsPerPeriod: number;
  sortOrder: number;
  status: PlanStatus;
  createdAt: string;
  updatedAt: string;
};

/** 后台套餐列表的筛选条件。 */
export type PlanQuery = PageQuery & {
  /** draft / active / disabled，空表示不筛。**后端不校验这个值**：传错的码返回的是空列表，不是 400。 */
  status?: PlanStatus;
  /** 模糊搜（ILIKE），按名称或编码。 */
  keyword?: string;
};

/**
 * 新建 / 修改套餐的请求体。
 *
 * 新建与修改共用：两者的差别只在「这条记录之前存在吗」，校验是同一套。
 * **没有 status**（上下架是单独的动作，见 updateMembershipPlanStatus）——合在一起的话，
 * 改一次价格描述会把一个正在售的套餐顺手存成草稿，而那一刻可能正有人在下单页上。
 */
export type PlanInput = {
  name: string;
  description: string;
  benefits: string[];
  priceCents: number;
  period: PlanPeriod;
  periodCount: number;
  autoRenew: boolean;
  wechatPlanId: string;
  memberPriceMode: MemberPriceMode;
  memberPriceCouponTemplateId: string;
  memberPriceCouponsPerPeriod: number;
  sortOrder: number;
};

/** 新建套餐的请求体：在 PlanInput 之上多一个 Code（也只有新建给得了它）。 */
export type CreatePlanInput = PlanInput & { code: string };

export async function listMembershipPlans(params?: PlanQuery) {
  return request<PageResult<MembershipPlan>>('/api/v1/admin/membership-plans', { params });
}

export async function getMembershipPlan(id: string) {
  return request<MembershipPlan>(`/api/v1/admin/membership-plans/${id}`);
}

export async function createMembershipPlan(data: CreatePlanInput) {
  return request<MembershipPlan>('/api/v1/admin/membership-plans', { method: 'POST', data });
}

/**
 * 修改套餐。**是整份替换**（后端没有 PATCH 语义），所以调用方必须先把整份详情取出来、
 * 在上面改，再把全量发回来——只发改动过的字段会把没发的字段清空。
 */
export async function updateMembershipPlan(id: string, data: PlanInput) {
  return request<MembershipPlan>(`/api/v1/admin/membership-plans/${id}`, { method: 'PUT', data });
}

/**
 * 上下架 / 转草稿。与「保存表单」分开，理由同抽奖那边：那个是整份换，这个只是开关。
 *
 * 下架**不影响已购会员**：会员行上存着成交快照，套餐不卖了，买过的人拿到的还是当初那些。
 */
export async function updateMembershipPlanStatus(id: string, status: PlanStatus) {
  return request<MembershipPlan>(`/api/v1/admin/membership-plans/${id}/status`, {
    method: 'POST',
    data: { status },
  });
}

// ——— 会员 ———

/** 会员身上发生过的一件事。详情页的时间线，一行一条。 */
export type MembershipChange = {
  id: string;
  changeType: ChangeType;
  /**
   * 状态与到期时间各有一对 from/to，两对都可能**半边为空**：续费只动有效期（状态两个都是
   * active），冻结只动状态。空串表示「没有前一个状态」——开通那一条的 fromStatus 就是空串。
   */
  fromStatus: string;
  toStatus: string;
  fromExpireAt: string | null;
  toExpireAt: string | null;
  /** 到期扫描、后台调整都没有订单，所以这是常态里的常态。 */
  orderId: string | null;
  operatorType: MembershipOperatorType;
  /** 系统与 worker 写的那几条没有操作人。 */
  operatorId: string | null;
  reason: string;
  remark: string;
  occurredAt: string;
};

/**
 * 一条会员资格。
 *
 * 列表与详情同形，差别只在 `changes`：**只有详情填**，列表页是空数组。
 */
export type Membership = {
  id: string;
  /**
   * 用户 ID。**只有它**——会员库没有昵称也没有手机号，接口也不 join 用户服务
   * （跨库，且会员域不该依赖身份域）。再要别的身份信息得另外去问 miniappUsers。
   */
  userId: string;
  /**
   * 套餐编码与名称是**成交当时的快照**，不是现查套餐。套餐后来改了名，这里不跟着变
   * ——用户当初买的就是那个名字。
   */
  planCode: string;
  planName: string;
  /** 同样是成交快照，见 MembershipPlan.memberPriceMode。 */
  memberPriceMode: MemberPriceMode;
  status: MembershipStatus;
  startAt: string;
  expireAt: string;
  /**
   * 这个会员此刻到底还作不作数（status=active **且在有效期内**），**后端算好的**。
   *
   * 不要在前端重算：到期扫描没跑完的那一小段时间里，status 还是 active 而 expireAt 已经
   * 过了，只看 status 的前端会显示「会员有效」。
   */
  active: boolean;
  /**
   * **归属门店**：这个人是谁拉来的，不参与任何金额计算（会员价在哪家店用都一样）。
   *
   * 规则：第一次成为会员那一刻固化，续费与升级不覆盖；**过期之后重新开通**才可变——两个变更
   * 渠道是店铺码活动与在门店买咖啡时开会员。空串表示没有归属，不是错误。
   *
   * storeName 是**后端现解出来的**（会员库只存门店 id，名字是商户域的事实），解不出来时是
   * 空串——门店服务抖一下不会让整页打不开。所以显示时按「空串 ⇒ 显示 id 或 —」处理，
   * 别把空串当成「这家店被删了」。
   */
  storeId: string;
  storeName: string;
  autoRenew: boolean;
  /** 用户关掉自动续费的时刻。开着的时候是 null。 */
  autoRenewOffAt: string | null;
  /** 续了几天期。首购不算续期，所以刚开通是 0。 */
  renewalCount: number;
  lastRenewedAt: string | null;
  /** 冻结与撤销的留痕。时间戳是 null 表示没发生过；两个 reason 没发生过时是**空串**。 */
  frozenAt: string | null;
  freezeReason: string;
  revokedAt: string | null;
  revokeReason: string;
  createdAt: string;
  updatedAt: string;
  /** 时间线，只有详情填。列表页是空数组。 */
  changes: MembershipChange[];
};

/** 后台会员列表的筛选条件。 */
export type MembershipQuery = PageQuery & {
  /** 精确匹配（uuid 列）。后台最常见的一次查询就是「这个用户是不是会员」。 */
  userId?: string;
  /** active / frozen / expired / revoked，空表示不筛。传错的码后端返回 400。 */
  status?: MembershipStatus;
  /** 按套餐编码筛。筛的是**成交快照上的** code，不是现查套餐。 */
  planCode?: string;
  /** 到期时间的闭开区间 [expireFrom, expireTo)，必须是完整的 RFC3339（`2026-01-01` 这种短式会被拒）。 */
  expireFrom?: string;
  expireTo?: string;
  /**
   * 三态：true 只看开着自动续费的，false 只看没开的，不给就是不筛。
   *
   * **别用真值判断**：false 是一个有意义的值，把它当成「没填」会悄悄地少筛一半。
   */
  autoRenew?: boolean;
};

export async function listMemberships(params?: MembershipQuery) {
  return request<PageResult<Membership>>('/api/v1/admin/memberships', { params });
}

/** 取一条会员详情。与列表的区别只有一个：这里带 `changes`。 */
export async function getMembership(id: string) {
  return request<Membership>(`/api/v1/admin/memberships/${id}`);
}

/**
 * 后台**直接开通**会员：给一个还不是会员的人开一条会员。
 *
 * 这是会员域唯一的创建入口。别的会员都是「在别处成交、在这里生效」（买会员的订单付款成功
 * 后发事件过来），只有这一条**没有订单**——客服补偿、线下活动、渠道争议走的就是它。
 *
 * 三条要知道的：
 *
 *  - **已有会员一律 409**，不叠加续期。一个人一条会员，叠加等于把「开通」偷偷变成「续期」，
 *    而那件事在会员详情页有专门的按钮（adjustMembershipExpiry）。重试同一次提交不会 409
 *    ——后端按 requestId 认得出那是同一次点击。
 *  - **到期日默认按套餐算，可以改**。补偿场景常是「送到年底」。
 *  - 权限码是 membership:adjust（**直接白送钱**那一枚），不是 manage。
 */
export type GrantMembershipInput = {
  userId: string;
  planId: string;
  /** 到期时刻（RFC3339）。不传就按套餐的 period / periodCount 从当下算。 */
  expireAt?: string;
  /** 归属门店，可空。给了后端会先问一次「这家店在不在」，不在就拒。 */
  storeId?: string;
  /** 开通原因，必填——这是一次人工白送，审计里要能看出为什么。 */
  reason: string;
  remark?: string;
  /**
   * 幂等键，必填，**由前端在弹窗打开时生成一次**（提交时带上，重试沿用同一个）。
   *
   * 不能省：没有它，一次网络抖动后的重发会让操作员看到「这个人已经是会员了」——而那是他
   * 刚刚亲手建的那条，一句彻头彻尾的假警报。
   */
  requestId: string;
};

export async function grantMembership(data: GrantMembershipInput) {
  return request<Membership>('/api/v1/admin/memberships', { method: 'POST', data });
}

// 下面四个是后台对会员的**人工干预**，权限码是 membership:adjust 而不是 membership:manage
// ——它们直接改一个人已经在享的权益，而改套餐只影响接下来卖什么。每一个都要填原因，
// 每一次调用都会写一条平台审计（方案 §11.6「影响用户资产归属的人工操作」）。

/** 冻结：**只能从 active 来**（不是 active 的会员后端会 409）。不改到期时间。 */
export async function freezeMembership(id: string, reason: string) {
  return request<Membership>(`/api/v1/admin/memberships/${id}/freeze`, {
    method: 'POST',
    data: { reason },
  });
}

/** 解冻：**只能从 frozen 来**。解冻后状态回 active，但「冻结原因」会留着（它记的是上一次冻结）。 */
export async function unfreezeMembership(id: string, reason: string) {
  return request<Membership>(`/api/v1/admin/memberships/${id}/unfreeze`, {
    method: 'POST',
    data: { reason },
  });
}

/** 撤销：除已撤销外都能来，**且不可逆**——后端没有「取消撤销」，它也同时关掉自动续费。 */
export async function revokeMembership(id: string, reason: string) {
  return request<Membership>(`/api/v1/admin/memberships/${id}/revoke`, {
    method: 'POST',
    data: { reason },
  });
}

/**
 * 后台直接改有效期。**是本域破坏力最大的一个接口**：没有任何订单、支付或流水跟着发生，
 * 只是把 expireAt 挪一下。
 *
 * 传的是**绝对时刻**而不是「延长 N 天」：客服工单上给的本来就是一个日期，让操作的人自己
 * 先算一遍，算错的那次没有任何东西能发现。延长语义由调用方算完再发过来。
 * 后端还会挡一条：新的到期时间必须晚于开通时间（否则 409）。
 */
export async function adjustMembershipExpiry(
  id: string,
  data: { expireAt: string; reason: string; remark?: string },
) {
  return request<Membership>(`/api/v1/admin/memberships/${id}/expire`, {
    method: 'POST',
    data,
  });
}
