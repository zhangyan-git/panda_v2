import { request } from '@umijs/max';
import type { PageQuery, PageResult } from './pagination';

/**
 * 福卡账户域的接口（account-service）。字段名与后端 dto（internal/dto/fortune_card.go）
 * 的 json tag 逐字对应，同时也是页面上的 dataIndex——改一处要同批改三处。
 *
 * 张数一律是整数，没有小数也没有单位换算：福卡是抽奖凭证，不是金额。别把它塞进
 * money.ts 的那些格式化函数里。
 */

// ——— 枚举 ———

/**
 * 流水的收支类型。取值来自 account-service 的 model 常量，与
 * migrations/account/001 的 CHECK 约束一致。
 *
 * 加新码而这里没登记时，界面退回显示原始码（同 order.ts 的规矩）。
 */
export type FortuneCardEntryType = 'grant' | 'draw' | 'reverse';

/** 这条流水指向什么：订单赠送 / 参与抽奖 / 冲正某一条流水。 */
export type FortuneCardReferenceType = 'order' | 'draw' | 'entry' | '';

// ——— 形状 ———

/**
 * 一条福卡流水。
 *
 * `amount` 是**有符号**的：发放为正，抽奖扣减与冲正为负。页面因此不能按「数量」渲染，
 * 要带着符号显示，否则一次抽奖看起来像又发了一张。
 */
export type FortuneCardEntry = {
  id: string;
  userId: string;
  entryType: FortuneCardEntryType;
  amount: number;
  /**
   * 这笔发生**之后**的余额，不是当前余额。明细页要显示的就是它——冲正重放时只有它能
   * 回答「当时是多少」。要当前余额请看 getFortuneCardAccount。
   */
  balanceAfter: number;
  /** 小程序明细上那一行文案（后端生成，界面原样显示）。 */
  title: string;
  referenceType: FortuneCardReferenceType;
  referenceId: string;
  /** 人可读的号：订单赠送时是订单号。按它筛流水就是靠这个字段。 */
  referenceNo: string;
  /** 冲正时指向被冲的那一条；其余为空串。 */
  reversesEntryId: string;
  remark: string;
  /** 业务发生时间（订单完成时间），与 created_at 可能不同。 */
  occurredAt: string;
  createdAt: string;
};

/** 冻结状态。取值来自 account-service 的 model 常量，与 migrations/account/003 的 CHECK 一致。 */
export type FortuneCardFreezeStatus = 'frozen' | 'released';

/**
 * 一条退款冻结：一张售后单冻住的那几笔发放。
 *
 * 冻结**不是账变**——balance 与流水一格不动，被冻的卡只是不能拿去抽奖。所以这里的
 * `amount` 不是「余额减少了几张」，别和 FortuneCardEntry.amount 混着读。解冻之后
 * `amount` 保留（审计要看当初冻了多少），`status` 变成 released。
 */
export type FortuneCardFreeze = {
  id: string;
  userId: string;
  /** 发起这次冻结的售后单号。它也是后端那把幂等键。 */
  afterSaleNo: string;
  orderId: string;
  orderNo: string;
  /**
   * 被冻的那几笔发放的幂等键（`order:{id}:base` / `order:{id}:bonus:{campaignId}`）。
   * 「按退款范围冻」的语义全在这里：只退加购行时这里只有 bonus 那一个键。
   */
  entryKeys: string[];
  /** 实际冻住的张数。可能是 0（申请早于发放，或卡已经被抽掉了）。 */
  amount: number;
  status: FortuneCardFreezeStatus;
  /** 后端生成的一句话：退款申请中 / 被驳回 / 用户撤销。 */
  reason: string;
  occurredAt: string;
  /** 只有 status=released 时有值。 */
  releasedAt: string | null;
  createdAt: string;
  updatedAt: string;
};

/** 一个用户的福卡账户。 */
export type FortuneCardAccount = {
  userId: string;
  /** 总余额（含被冻的）。明细页上「变动后余额」对的就是它。 */
  balance: number;
  /** 退款冻结中的张数。 */
  frozenBalance: number;
  /** 能拿去抽奖的张数 = balance - frozenBalance。后端算好了给，别在前端再减一次。 */
  availableBalance: number;
  createdAt: string;
  updatedAt: string;
  /**
   * 区分「有过账户、余额为 0」与「从来没有过账户」。两者都是 balance 0，但流水页上
   * 差别很大（一个有历史，一个一条都没有），所以文案要分开写。
   */
  hasAccount: boolean;
};

// ——— 查询参数 ———

/**
 * 流水筛选。后端 EntryQuery 里全是精确匹配，`orderNo` 也不例外（所以调用前 trim）。
 *
 * `from`/`to` 是**闭开区间** [from, to)：同一条流水只会落在相邻两天的其中一边，
 * 拼接时不会漏也不会重。必须传带时区的 RFC3339（用 toRFC3339 转，与订单列表同款）。
 */
export type FortuneCardEntryQuery = PageQuery & {
  userId?: string;
  orderNo?: string;
  entryType?: FortuneCardEntryType;
  from?: string;
  to?: string;
};

/**
 * 冻结筛选。三个都是精确匹配（后端 dto.FreezeQuery），所以调用前 trim；
 * `status` 不传就是「不限」，界面上的「全部」要翻译成不传，而不是传空串。
 */
export type FortuneCardFreezeQuery = PageQuery & {
  userId?: string;
  orderNo?: string;
  status?: FortuneCardFreezeStatus;
};

// ——— 接口 ———

/**
 * 按条件查福卡流水（后台）。订单详情的「福卡」tab 就是按 orderNo 用它。
 *
 * 后端把 `/v1/admin/fortune-cards/entries` 注册在 `/v1/admin/fortune-cards/{userId}`
 * **之前**，所以这里不会被打成「userId 是 entries」——两者在网关与路由上都分得开。
 */
export async function listFortuneCardEntries(params?: FortuneCardEntryQuery) {
  return request<PageResult<FortuneCardEntry>>('/api/v1/admin/fortune-cards/entries', {
    params,
  });
}

/**
 * 查某个用户的福卡账户（客服最常问的那一句：他还有几张）。
 *
 * 没有账户行时回的是余额 0 + hasAccount=false，**不是 404**——账户行是第一次发放时才
 * 懒创建的，一个从没收到过福卡的用户查余额，答案就是「0 张」。
 */
export async function getFortuneCardAccount(userId: string) {
  return request<FortuneCardAccount>(`/api/v1/admin/fortune-cards/${userId}`);
}

/**
 * 按条件查退款冻结（后台）。订单详情的「福卡」tab 用它显示「冻结中 N 张」。
 *
 * 与 entries 同一条路由规矩：后端把 `/v1/admin/fortune-cards/freezes` 也注册在
 * `/{userId}` **之前**。名字里的 Freezes 不能省——`/fortune-cards/{userId}` 那条会把
 * 路径段当 userId 解析，写成 `/fortune-cards/freezes` 会被打成「userId 不是 UUID」的 400。
 */
export async function listFortuneCardFreezes(params?: FortuneCardFreezeQuery) {
  return request<PageResult<FortuneCardFreeze>>('/api/v1/admin/fortune-cards/freezes', {
    params,
  });
}
