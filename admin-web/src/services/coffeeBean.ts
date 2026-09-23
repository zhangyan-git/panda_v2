import { request } from '@umijs/max';
import type { PageQuery, PageResult } from './pagination';

/**
 * 咖啡豆账户域的接口（account-service）。字段名与后端 dto（internal/dto/coffee_bean.go）
 * 的 json tag 逐字对应，同时也是页面上的 dataIndex——改一处要同批改三处。
 *
 * ⚠️ 这里的「咖啡豆」是**用户维度**的余额（coffee_bean_accounts，键是 user_id），
 * 单位是**分**，与订单金额 1:1。另有一个名字很像的东西：设备维度的
 * `devices.coffee_balance`（在 services/coffeeMachine.ts 里是 listDeviceBalanceEntries），
 * 那是按 device_id 记账的预付余额，两者没有任何关系、
 * 不共用接口、也不共用表名。**读这个文件时不要把两边的金额混着用。**
 *
 * 与福卡那份（fortuneCard.ts）的两处结构差异，都来自口径：
 *   1. 没有冻结——豆在支付那一刻就扣走了，账户上没有 frozen/available 两项。
 *   2. 有写——`adjustCoffeeBeans` 是后台**人工调整**，也全仓唯一能凭空加豆的接口。
 */

// ——— 枚举 ———

/**
 * 流水的账变类型。取值来自 account-service 的 model 常量（BeanEntryType*），与
 * migrations/account 的 CHECK 一致。
 *
 * 加新码而这里没登记时，界面退回显示原始码（同 order.ts / fortuneCard.ts 的规矩）。
 */
export type CoffeeBeanEntryType = 'adjust' | 'consume' | 'reverse';

/** 这条流水为什么发生：后台人工调整 / 订单扣减 / 售后冲正。 */
export type CoffeeBeanReferenceType = 'manual' | 'order' | 'after_sale' | '';

// ——— 形状 ———

/**
 * 一个用户的咖啡豆账户。
 *
 * 没有账户行时余额是 0 且 `hasAccount=false`，**不是 404**——账户行是第一次调整或第一次
 * 扣减时才懒创建的。客服看到 0 分时会想区分「有过账户、花光了」与「从来没有过」，
 * 两者在流水表上差别很大（一个有历史，一个一条都没有）。
 */
export type CoffeeBeanAccount = {
  userId: string;
  /** 单位为分。没有 frozen/available 两项：豆没有冻结态，可用就是余额。 */
  balance: number;
  hasAccount: boolean;
  createdAt: string;
  updatedAt: string;
};

/**
 * 一条咖啡豆流水。
 *
 * `amount` 是**有符号**的、单位为分：调整充值为正、纠错为负、订单扣减为负、冲正与它冲掉的
 * 那笔相反。页面因此不能按「金额」渲染，要带着符号显示，否则一次扣减看起来像又充了一笔。
 */
export type CoffeeBeanEntry = {
  id: string;
  userId: string;
  entryType: CoffeeBeanEntryType;
  /** 单位为分，带符号。 */
  amount: number;
  /**
   * 这笔发生**之后**的余额，不是当前余额。冲正重放时只有它能回答「当时是多少」。
   * 要当前余额请看 getCoffeeBeanAccount 或 BeanAdjustResponse.balance。
   */
  balanceAfter: number;
  /** 给用户看的那一行文案（后端生成，界面原样显示）。 */
  title: string;
  referenceType: CoffeeBeanReferenceType;
  referenceId: string;
  /**
   * 人可读的号。三种形状：调整是幂等号（`adjust:{requestId}` 里的后半段）、扣减是订单号、
   * 冲正是售后单号。所以接口的筛选参数叫 referenceNo 而不是 orderNo。
   */
  referenceNo: string;
  /** 冲正时指向被冲的那笔扣减；其余为空串。 */
  reversesEntryId: string;
  /** 只有后台调整那一条有值：谁动的。是管理员的 user id，不是名字。 */
  operatorId: string;
  /** 操作人填的理由。它也是调整理由唯一的去处（审计表没有 reason 列）。 */
  remark: string;
  /** 业务发生时间（扣减是支付时间、冲正是审核时间），与 createdAt 可能不同。 */
  occurredAt: string;
  createdAt: string;
};

/** 「余额调整」的请求体。 */
export type CoffeeBeanAdjustInput = {
  /** 单位为分，**带符号**：充值为正，把充错的豆调回来为负。0 会被后端拒掉。 */
  amount: number;
  /** 幂等号。每次打开弹窗生成一个新的，见 adjustCoffeeBeans 的注释。 */
  requestId: string;
  remark?: string;
};

/**
 * 调整之后立刻回来的那两个数。
 *
 * 余额一起给，页面就能把数字直接改对，不必再发一次读——而且**这个**余额是刚写完那笔之后的
 * 数，重新读一次拿到的可能是别人又动过的余额。
 */
export type CoffeeBeanAdjustResult = {
  /** 单位为分。 */
  balance: number;
  entryId: string;
};

// ——— 查询参数 ———

/**
 * 流水筛选。`referenceNo` 是精确匹配（后端 BeanEntryQuery），所以调用前 trim。
 *
 * `from`/`to` 是**闭开区间** [from, to)：同一条流水只会落在相邻两天的其中一边，
 * 拼接时不会漏也不会重。必须传带时区的 RFC3339（用 toRFC3339 转，与订单列表同款）。
 */
export type CoffeeBeanEntryQuery = PageQuery & {
  userId?: string;
  referenceNo?: string;
  entryType?: CoffeeBeanEntryType;
  from?: string;
  to?: string;
};

// ——— 接口 ———

/**
 * 查某个用户的咖啡豆账户（客服最常问的那一句：他还有多少豆）。
 *
 * 后端把 `/v1/admin/coffee-beans/entries` 注册在 `/v1/admin/coffee-beans/{userId}`
 * **之前**，所以这里不会被打成「userId 是 entries」——两者在网关与路由上都分得开。
 */
export async function getCoffeeBeanAccount(userId: string) {
  return request<CoffeeBeanAccount>(`/api/v1/admin/coffee-beans/${userId}`);
}

/**
 * 按条件查咖啡豆流水（后台）。小程序用户详情的「咖啡豆账户」区块按 userId 用它。
 */
export async function listCoffeeBeanEntries(params?: CoffeeBeanEntryQuery) {
  return request<PageResult<CoffeeBeanEntry>>('/api/v1/admin/coffee-beans/entries', {
    params,
  });
}

/**
 * 后台人工调整某人的咖啡豆余额。返回调整后的余额，不必再查一次。
 *
 * 权限码是 `account:manage`（不是读那条 `account:read`）：这是**直接加钱**的能力，
 * 充值就是正数，没有审批也没有额度上限，所以读写刻意分开。
 *
 * 同一个 requestId 重复提交时后端回 409（而不是静默回放、也不是假装成功）：上一次多半
 * 已经记过账，只是响应没收到。点这个按钮的是人，回一句「成功了」会让他再点一次，于是
 * 真的充两次。所以调用方必须把 409 和普通失败分开处理，且 409 之后要换一枚新的 requestId
 * ——详情页里的做法见 pages/miniapp-users/index.tsx。
 */
export async function adjustCoffeeBeans(userId: string, data: CoffeeBeanAdjustInput) {
  return request<CoffeeBeanAdjustResult>(
    `/api/v1/admin/coffee-beans/${userId}/adjustments`,
    { method: 'POST', data },
  );
}
