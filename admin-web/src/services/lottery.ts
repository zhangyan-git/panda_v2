import { request } from '@umijs/max';
import type { PageQuery, PageResult } from './pagination';

/**
 * 抽奖域的接口。字段名与后端 dto（lottery-service/internal/dto）的 json tag 逐字对应，
 * 同时也是页面上的 dataIndex——改一处要同批改三处（tag、这里、ProTable 的列），
 * 否则 TypeScript 不报错，只是整列空白。
 *
 * 后端那个包的注释里点名了这一份文件，说的就是这条约定。
 *
 * 边界先说清楚：**本服务只拥有抽奖与奖品**。福卡余额在 account-service（抽奖中心那
 * 一页读它，是实时问过去的，不在本域库里），订单事实在 order-service。所以这个文件里
 * **没有余额相关的写接口**，也别加。
 */

// ——— 枚举 ———
// 取值来自 lottery-service model 的常量，也就是 migrations/lottery/001 的 CHECK 约束。
// 接口回的就是库里那些英文码。**改枚举必须同时改迁移和这里**，加了新码而这里没登记，
// 界面上就退回显示原始码。文案表在 services/lotteryLabels.ts。

/** lottery_activations.status：这家门店的抽奖开着还是关着 */
export type ActivationStatus = 'enabled' | 'disabled';

/** lottery_campaigns.status：一个活动的生命周期 */
export type CampaignStatus = 'draft' | 'enabled' | 'paused' | 'ended';

/** lottery_rounds.status：一期走到哪一步 */
export type RoundStatus = 'open' | 'closed' | 'drawn' | 'cancelled';

/**
 * lottery_participations.status：一次参与的三段事务走到哪一步。
 *
 * reversed 是「扣了卡又退回去」——它在库里是一个终态，但**界面上不该与 failed 合并**：
 * failed 是「没扣成」，reversed 是「扣了又原路退回」，两者对用户的钱不同，虽然结果一样。
 */
export type ParticipationStatus = 'pending' | 'confirmed' | 'failed' | 'reversed';

/** 参与失败的原因码（participations.failure_code，空串 = 没有失败）。 */
export type ParticipationFailureCode =
  | ''
  | 'insufficient_fortune_cards'
  | 'round_closed'
  | 'invalid_request'
  | 'round_not_open';

/** lottery_campaign_prizes.prize_kind：这个奖是什么东西 */
export type PrizeKind = 'coupon' | 'coffee' | 'physical' | 'custom';

/**
 * lottery_wins.status：中奖记录的状态机。
 *
 * **本轮只有 pending 会出现**（核销整块延后了，见 service 包的文件头）。其余五个取值是
 * 最终形态的一部分，先写进 CHECK 免得下一轮再来一次迁移，所以这里也照登——
 * 登记它不等于它今天能被写出来。
 */
export type WinStatus =
  | 'pending'
  | 'claimed'
  | 'redeemed'
  | 'expired'
  | 'revoked'
  | 'superseded';

/** lottery_win_events.event_type：中奖流水。本轮只写 created。 */
export type WinEventType =
  | 'created'
  | 'claimed'
  | 'testimonial_updated'
  | 'redeemed'
  | 'swapped'
  | 'superseded'
  | 'revoked'
  | 'expired';

/** 是谁做的这次变动。win_events.actor_type。 */
export type ActorType = 'user' | 'merchant' | 'admin' | 'system';

/** lottery_draws.mode / trigger：这次开奖是谁、因为什么触发的 */
export type DrawMode = 'auto' | 'manual';
export type DrawTrigger = 'threshold' | 'deadline' | 'manual';

// ——— 开通门店 ———

/**
 * 一条开通记录。
 *
 * 开通是一个**动作**（有操作人、有时间），不是「这家店有没有启用中的活动」派生出来的。
 * 所以它在期次之间的空档里仍然显示为「已开通」。
 */
export type Activation = {
  id: string;
  locationId: string;
  /** 开通那一刻的店名快照。商户改了店名之后它不变——这是有意的。 */
  locationName: string;
  status: ActivationStatus;
  remark: string;
  /**
   * 默认活动。它来自 lottery_campaigns.is_default，不是开通行上的一列。
   * 列表里直接点得进去，也能回答「这家店开通的是哪个活动」。
   */
  defaultCampaignId: string;
  defaultCampaignName: string;
  /** 活动数（含默认）。 */
  campaignCount: number;
  activatedBy: string;
  activatedAt: string;
  deactivatedAt: string | null;
  createdAt: string;
  updatedAt: string;
  /**
   * 这个门店**正在跑的那一期**。运营最关心的就是它（「现在第几期、还差几个人」），
   * 直接摆进列表省掉「点进活动再点进期次」那两步。没有在跑的期次时全为空。
   */
  liveRoundId: string;
  liveRoundNo: string;
  liveRoundSize: number;
  liveRoundDone: number;
};

/**
 * 开通门店。这一个动作会同时建出**默认活动与第一期**。
 *
 * locationName 由调用方带上来（后台的门店选择器本来就有名字）。不在这里回头问商户服务：
 * 开通是一个动作，重试它不该因为一次跨服务读失败而失败。
 *
 * participantTarget / startAt / endAt 都是可选的：默认活动是内置模板建的，都不给时窗口
 * 取「现在起 90 天」。给 participantTarget 时它同时是**第一期**的门槛（门槛在开期时冻结
 * 到期次上）。
 */
export type ActivateInput = {
  locationId: string;
  locationName: string;
  remark?: string;
  campaignName?: string;
  participantTarget?: number;
  startAt?: string;
  endAt?: string;
};

/**
 * 开通记录上可改的那两样。**门店不能改**：改门店等于换一家店开通，那是停用 + 新开通
 * 两件事，不是一次 PUT。
 *
 * 停用不会删掉已开出的期中奖记录——它只是不再开新期。已开奖的期次是终局。
 */
export type UpdateActivationInput = {
  status: ActivationStatus;
  remark?: string;
};

// ——— 活动与奖池 ———

/** 奖池里的一行。 */
export type CampaignPrize = {
  id: string;
  /** 行序由运营决定。开奖按它依次分配名额（第一档拿满自己那 quantity 份才轮到下一档）。 */
  sortOrder: number;
  prizeKind: PrizeKind;
  /** 展示名，如「10 元咖啡兑换券」。 */
  name: string;
  /**
   * **本期无人消费**：券类奖品的自动发放要 coupon-service 的 gRPC，而它是空壳
   * （coupon.proto 里只有 `service CouponService {}`）。所以这一列现在只存不用。
   */
  couponTemplateId: string;
  imageUrl: string;
  claimInstructions: string;
  /** 这一档奖的名额。SUM(quantity) 就是下一期的 winner_count。 */
  quantity: number;
};

/** 奖池整体替换时提交的一行（新建时没有 id）。 */
export type PrizeInput = {
  sortOrder: number;
  prizeKind: PrizeKind;
  name: string;
  couponTemplateId?: string;
  imageUrl?: string;
  claimInstructions?: string;
  quantity: number;
};

/** 一个活动。 */
export type Campaign = {
  id: string;
  activationId: string;
  locationId: string;
  locationName: string;
  /** **为空 = 门店级活动**；有值 = 这台咖啡机专属。界面的「适用设备」列按它渲染。 */
  machineId: string | null;
  /** 短名，期次号的前缀（`{code}-{seq:04d}`）。全局唯一，且只能是大写字母数字。 */
  code: string;
  name: string;
  /** 新期次的默认门槛，开期时冻结到期次上。 */
  participantTarget: number;
  description: string;
  /** 开通时建出来的那一个。一个开通记录只有一个默认活动。 */
  isDefault: boolean;
  startAt: string;
  endAt: string;
  status: CampaignStatus;
  /**
   * 奖池。**列表接口不带它**（一次 20 个活动、每个带 5 个奖品，列表就成了奖池查询），
   * 详情接口带。所以列表页里这个字段恒为 undefined/[]，别在那一页读它。
   */
  prizes?: CampaignPrize[];
  /** 奖池总名额 = SUM(prizes.quantity)，也就是下一期的 winner_count。 */
  prizeTotalQuantity: number;
  /** 在跑的那一期（open / closed），没有则为空。 */
  liveRoundId: string;
  liveRoundNo: string;
  liveRoundSize: number;
  liveRoundDone: number;
  /** 已开出多少期。 */
  roundCount: number;
  createdAt: string;
  updatedAt: string;
};

/**
 * 新建 / 修改活动。
 *
 * prizes 是**整份清单**，不是差集：后台那个表单就是「一张奖品表」，运营加一行删一行之后
 * 点保存。逐行增删的接口会让前端自己算差集，而差集算错的后果是名额总数对不上——那正是
 * 开奖要用的数。
 *
 * activationId 新建时必填。**修改时后端整个忽略它**——UpdateCampaign 取的是库里已有的
 * 那个 activationId，你传什么都不看（活动不能换门店，那等于新建一个）。所以编辑页把那
 * 一格禁用掉是安全的，不必费心回填。
 */
export type CampaignInput = {
  activationId: string;
  machineId?: string | null;
  code: string;
  name: string;
  participantTarget: number;
  description?: string;
  startAt: string;
  endAt: string;
  status: CampaignStatus;
  prizes: PrizeInput[];
};

// ——— 期次 ———

/** 一期。 */
export type Round = {
  id: string;
  campaignId: string;
  campaignCode: string;
  campaignName: string;
  seq: number;
  /** 期次号，`{code}-{seq:04d}`。全局唯一。 */
  roundNo: string;
  status: RoundStatus;
  /** 门槛与已达标人数。列表页的进度条按这两个数画。 */
  participantTarget: number;
  participantCount: number;
  /** **名额数**（开期时冻结的 SUM(quantity)），不是实际中奖人数。 */
  winnerCount: number;
  startsAt: string;
  endsAt: string;
  drawnAt: string | null;
  cancelledAt: string | null;
  cancelReason: string;
  drawId: string;
  drawMode: DrawMode | '';
  drawTrigger: DrawTrigger | '';
  /** 实际中奖人数。与 winnerCount 不同：参与人数不够时会少于名额。开奖后才有值。 */
  actualWinnerCount: number;
  createdAt: string;
};

/**
 * 人工开奖的请求体。
 *
 * expectedRoundStatus / expectedParticipantCount 是**必填**，不是防御性的客套：
 * 管理员手上那个页面可能是三十秒前加载的，这期间期次可能已经达标关闭、已经被自动开奖、
 * 甚至已经被作废。对不上时后端回 409 ROUND_CHANGED，而不是替一个已经变了的局面决定谁
 * 中奖。所以调用方必须把**页面上显示的那两个值**原样报回去，不能现取现填。
 */
export type DrawInput = {
  /** 必填，≤200 字。会进开奖记录与平台审计。 */
  reason: string;
  expectedRoundStatus: RoundStatus;
  expectedParticipantCount: number;
};

/**
 * 一次开奖的结果。
 *
 * **零人参与的那一期不走这个形状**：它直接作废、不写开奖记录，后端回的是
 * `{ roundId, status, drawId: '', message }`。见 drawRound 的说明。
 */
export type DrawResult = {
  drawId: string;
  roundId: string;
  roundNo: string;
  mode: DrawMode;
  trigger: DrawTrigger;
  /** 开奖种子。回给调用方是为了让管理员能把这次开奖记下来备查——它是复核的全部依据。 */
  seed: string;
  algorithm: string;
  participantCount: number;
  winnerCount: number;
  createdAt: string;
  winners: Win[];
  /**
   * 同一次事务里开出来的下一期。为空表示开完之后没有下一期了，此时 campaignEnded
   * 必然是 true——两句是同一个事实的两面。
   */
  nextRoundId: string;
  nextRoundNo: string;
  campaignEnded: boolean;
};

/** 零人参与时开奖接口回的那个形状（没有开奖记录）。 */
export type EmptyDrawResult = {
  roundId: string;
  status: RoundStatus;
  drawId: '';
  message: string;
};

/** 作废一期之后回的那几格。**不是完整的期次行**——后端只写了这四个键。 */
export type CancelRoundResult = {
  roundId: string;
  roundNo: string;
  /** 作废后的期次状态，也就是 'cancelled'。 */
  status: RoundStatus;
  cancelReason: string;
};

// ——— 参与 ———

/** 一条参与记录。 */
export type Participation = {
  id: string;
  roundId: string;
  roundNo: string;
  campaignId: string;
  campaignName: string;
  userId: string;
  /** 这一笔是从哪张订单来的。「从抽奖中心直接参与」为空。 */
  sourceOrderId: string;
  sourceOrderNo: string;
  sourceMachineId: string;
  sourceLocationId: string;
  /** 扣了几张福卡。恒为 1（原型规则：一次一张），由服务端定死。 */
  cost: number;
  status: ParticipationStatus;
  failureCode: ParticipationFailureCode;
  /** 账户域的账变 ID。对账时从这一笔参与反查那次扣卡。 */
  fortuneEntryId: string;
  createdAt: string;
  confirmedAt: string | null;
  /** 这一笔有没有中奖。中奖记录按 participation_id 反查，一期内一条参与只能中一次。 */
  winId: string;
  winClaimNo: string;
  prizeName: string;
};

// ——— 中奖 ———

/** 一条中奖记录。 */
export type Win = {
  id: string;
  drawId: string;
  roundId: string;
  roundNo: string;
  campaignId: string;
  campaignName: string;
  participationId: string;
  userId: string;
  prizeId: string;
  prizeKind: PrizeKind;
  /**
   * 原奖品与现奖品分两列。换奖改的是 current，original 永远留着——它是「当时开出来的
   * 是哪个奖」的唯一记录。本轮两列相同（还没有换奖入口）。
   */
  originalPrizeName: string;
  currentPrizeName: string;
  /**
   * 领取 / 核销凭证号，如 LW20260915-000123。用户将来在门店要报的就是它。
   *
   * 本轮**只有读取方**：核销延后了，后台能看不能用。它不是凭据（不校验、不作鉴权），
   * 这一点与订单的取杯号一致。
   */
  claimNo: string;
  status: WinStatus;
  /** 获奖感言与图片。本轮都没有写入方，恒为空。 */
  testimonial: string;
  testimonialImages: string[];
  /** 来源快照。中奖详情不必回头 join 参与表。 */
  sourceOrderId: string;
  sourceOrderNo: string;
  sourceMachineId: string;
  sourceLocationId: string;
  /** 领取截止时间，null = 不过期。本轮恒为 null。 */
  expiresAt: string | null;
  claimedAt: string | null;
  /** 核销信息，本轮恒为空（核销整块延后）。 */
  redeemedAt: string | null;
  redeemedBy: string;
  redeemLocationId: string;
  redeemLocationName: string;
  createdAt: string;
};

/** 中奖记录的一条流水。本轮只会出现 created。 */
export type WinEvent = {
  id: string;
  eventType: WinEventType;
  fromStatus: string;
  toStatus: string;
  actorType: ActorType;
  actorId: string;
  actorName: string;
  reason: string;
  /** 按 event_type 有不同的形状，所以前端也要按类型分支读，别当成固定结构。 */
  metadata: Record<string, unknown>;
  createdAt: string;
};

/** 中奖详情：记录本身 + 它的流水。 */
export type WinDetail = {
  win: Win;
  events: WinEvent[];
};

// ——— 查询参数 ———
// 与 dto 里的 *Query 逐个对应。三个筛选都是**精确匹配**（后端是 `=`，没有 keyword），
// 所以调用前要 trim，也不要做成模糊搜索框。

export type ActivationQuery = PageQuery & {
  /** 门店 ID 精确匹配。不做 uuid 前缀匹配——那是没有意义的模糊。 */
  locationId?: string;
  status?: ActivationStatus;
  /** 门店名的模糊匹配（后端 LIKE）。运营按店名找店是常走的一条路。 */
  name?: string;
};

export type CampaignQuery = PageQuery & {
  /** 按门店看这个店有哪些活动，是后台最常走的一条路。两个都能筛，给哪个都行。 */
  activationId?: string;
  locationId?: string;
  /** 筛设备级活动用它。 */
  machineId?: string;
  status?: CampaignStatus;
  name?: string;
};

export type RoundQuery = PageQuery & {
  campaignId?: string;
  status?: RoundStatus;
};

export type ParticipationQuery = PageQuery & {
  roundId?: string;
  campaignId?: string;
  userId?: string;
  status?: ParticipationStatus;
};

export type WinQuery = PageQuery & {
  roundId?: string;
  campaignId?: string;
  userId?: string;
  status?: WinStatus;
  /** 凭证号精确匹配。门店端将来的核销入口按它查，本轮只有后台用它。 */
  claimNo?: string;
};

// ——— 接口 ———

export async function listActivations(params?: ActivationQuery) {
  return request<PageResult<Activation>>('/api/v1/admin/lottery/activations', { params });
}

export async function getActivation(id: string) {
  return request<Activation>(`/api/v1/admin/lottery/activations/${id}`);
}

/**
 * 开通门店抽奖。**同时建出默认活动与第一期**，所以这一个请求的副作用比它看起来大：
 * 返回之后这家店就已经在收参与了。
 *
 * 重复开通同一家门店回 409（`UNIQUE (location_id)`），不是覆盖——重复点击不是两笔业务。
 */
export async function activateLocation(data: ActivateInput) {
  return request<Activation>('/api/v1/admin/lottery/activations', {
    method: 'POST',
    data,
  });
}

/**
 * 改开通状态 / 备注。停用只是不再开新期，不影响已开奖的期次——那些是终局。
 *
 * 路径是 `/status` 而不是把整个资源 PUT 回来：门店不可改，这个端点能动的就是状态与备注。
 */
export async function updateActivation(id: string, data: UpdateActivationInput) {
  return request<Activation>(`/api/v1/admin/lottery/activations/${id}/status`, {
    method: 'POST',
    data,
  });
}

export async function listCampaigns(params?: CampaignQuery) {
  return request<PageResult<Campaign>>('/api/v1/admin/lottery/campaigns', { params });
}

/** 活动详情。**只有这一条路能拿到奖池**（列表接口不带 prizes）。 */
export async function getCampaign(id: string) {
  return request<Campaign>(`/api/v1/admin/lottery/campaigns/${id}`);
}

export async function createCampaign(data: CampaignInput) {
  return request<Campaign>('/api/v1/admin/lottery/campaigns', { method: 'POST', data });
}

/**
 * 修改活动，**含整份奖池**。
 *
 * 奖池整体替换，所以提交的 prizes 必须是完整清单，不是这次改的那几行。
 * code 建出来之后不可改：它是期次号的前缀，已经开出去的期次号里嵌着它。
 */
export async function updateCampaign(id: string, data: CampaignInput) {
  return request<Campaign>(`/api/v1/admin/lottery/campaigns/${id}`, { method: 'PUT', data });
}

/**
 * 改活动的生命周期（draft / enabled / paused / ended）。
 *
 * 与 updateCampaign 分开：那个走的是「表单保存」，会把奖池整份换掉；这个只是一个开关。
 * 用同一个端点的话，暂停一个活动得先把整份表单填对，而运营想做的只是一下点击。
 */
export async function updateCampaignStatus(id: string, status: CampaignStatus) {
  return request<Campaign>(`/api/v1/admin/lottery/campaigns/${id}/status`, {
    method: 'POST',
    data: { status, remark: '' },
  });
}

/** 某个活动的奖池（只读）。写入口只有 createCampaign / updateCampaign 的整份替换。 */
export async function listCampaignPrizes(campaignId: string) {
  return request<CampaignPrize[]>(`/api/v1/admin/lottery/campaigns/${campaignId}/prizes`);
}

export async function listRounds(params?: RoundQuery) {
  return request<PageResult<Round>>('/api/v1/admin/lottery/rounds', { params });
}

export async function getRound(id: string) {
  return request<Round>(`/api/v1/admin/lottery/rounds/${id}`);
}

/**
 * 人工开奖。权限码 `lottery:draw`，**只绑 super_admin**。
 *
 * 这是全系统少数几个能凭空决定「谁中奖」的动作，所以它要求两样东西：
 *   1. reason 必填（≤200 字）——事后要能回答「当时为什么提前开」；
 *   2. expectedRoundStatus + expectedParticipantCount 必须与**页面上显示的那两个值**
 *      一致，否则回 409 ROUND_CHANGED。别现取现填：那样这个校验就白做了。
 *
 * 两种成功形状：正常开奖回 DrawResult；**零人参与**时回 EmptyDrawResult（这一期直接作废、
 * 不写开奖记录）。两种情况都回 200，所以调用方要判断 drawId 才知道走的是哪一条。
 */
export async function drawRound(id: string, data: DrawInput) {
  return request<DrawResult | EmptyDrawResult>(`/api/v1/admin/lottery/rounds/${id}/draw`, {
    method: 'POST',
    data,
  });
}

/**
 * 作废一期（权限码同开奖）。
 *
 * 后端**只允许 participant_count = 0 的期次作废**。有参与者的作废要把 N 张卡沿 N 次跨服务
 * 冲正还回去，那条路属下一轮。所以这个按钮只在零人参与的期次上出现——别的期次点了必然
 * 回 409。
 *
 * 回的**不是**一整行期次，是这四格（后端就写了这四个键）。别把它当成 Round 用：
 * 少了 participantCount / endsAt 那些字段，读出来是 undefined。
 */
export async function cancelRound(id: string, reason: string) {
  return request<CancelRoundResult>(`/api/v1/admin/lottery/rounds/${id}/cancel`, {
    method: 'POST',
    data: { reason },
  });
}

export async function listParticipations(params?: ParticipationQuery) {
  return request<PageResult<Participation>>('/api/v1/admin/lottery/participations', { params });
}

export async function listWins(params?: WinQuery) {
  return request<PageResult<Win>>('/api/v1/admin/lottery/wins', { params });
}

export async function getWin(id: string) {
  return request<WinDetail>(`/api/v1/admin/lottery/wins/${id}`);
}

export async function getDraw(id: string) {
  return request<DrawResult>(`/api/v1/admin/lottery/draws/${id}`);
}
