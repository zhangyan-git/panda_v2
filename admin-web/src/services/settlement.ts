import { request } from '@umijs/max';
import type { PageQuery, PageResult } from './pagination';

/**
 * 分账域的接口。字段名与 payment-service 的 `internal/dto/settlement.go` 的 json tag 逐字对应，
 * 同时也是页面上的 dataIndex —— 改一处要同批改三处（tag、这里、ProTable 的列），否则 TypeScript
 * 不报错，只是整列空白。
 *
 * 三个页面共用这一份：**规则**（这类业务、这个范围上的钱怎么分）、**账户**（钱分到谁的哪个子商户
 * 号上）、**明细**（已经发生的那一笔分给了谁多少）。三页都在「支付管理」目录下，因为它们离开
 * 那一笔支付就没有意义。
 *
 * # 两件这一份里**没有**的东西
 *
 *  - **没有「发起分账」。** 分账在 V2 是**随支付一次下发**的：银联商务的 `divisionFlag` /
 *    `platformAmount` / `subOrders` 三个键拼在**下单报文**里，支付成功即分账成功。所以这里既
 *    没有「打款」接口，也没有 `settlement:payout` 这个码（identity/035 里写着为什么暂不发它）。
 *    老系统同样没有这一层，连 `ConfirmDivision` 都是定义了零调用方。
 *  - **没有「结算单」。** `settlement_statements` 那两张表仍然是空的，本刀不做。
 *
 * # 金额与比例的单位
 *
 * 金额一律 **int64 分**（与全仓 HTTP 层一致），页面上用 `services/money.ts` 的 `formatYuan`
 * 展示。比例是 **ratioPercent**：一个百分数，最多两位小数（45 表示 45.00%）——**不是 0~1 的
 * 比值**。所以 `settlementLabels.ts` 里没有 percent↔ratio 的换算函数：报文两侧说的都是百分数，
 * 中间再转一道只会多出「0.45 被当成 45% 还是 0.45%」这种多一个零就没人发现的错。
 */

// ——— 枚举 ———
// 取值来自 payment-service 的 internal/model/settlement.go，也就是
// migrations/payment/008_settlement_core.sql 的 CHECK 约束。接口回的就是库里那些英文码。
// **改枚举必须同时改迁移、改 model、改这里**，加了新码而这里没登记，界面上就退回显示原始码。
// 文案表在 services/settlementLabels.ts。

/**
 * `settlement_rules.biz_type`：这笔钱是哪一类业务收上来的。规则命中的第一段键。
 *
 * 顺序**不是**命中顺序——命中先比档位（从具体到宽泛），同档位才比业务分类；同一个业务分类下
 * 同档位只允许有一条启用中的规则（008 的部分唯一索引）。
 */
export type SettlementBizType = 'coffee' | 'membership' | 'store_consume' | 'addon_product';

/**
 * `settlement_rules.scope_type`：这条规则管多大一片地方。**声明顺序就是命中顺序**，从具体到
 * 宽泛，先命中先返回。
 *
 * 五档后台都配得出来，但**今天只有 store 与 device 会被推上来**：订单侧发起支付时给的
 * `SettlementRuleQuery` 只有这两维。brand / product 两档配了不会命中（页面上有提示），
 * 留着是因为词表与老系统都是五档，将来订单侧补齐这两维时不用改表。
 */
export type SettlementScopeType = 'device' | 'store' | 'brand' | 'product' | 'global';

/**
 * `settlement_rule_items.calc_type`：这一项按什么算。
 *
 * `remainder` 是**平台自留**，拿的是差额（基数 − 其他接收方），只能给 platform 项用——
 * 008 的 CHECK 钉死了：让某个门店去当那个「剩下的」，既算不清也说不通。
 */
export type SettlementCalcType = 'percent' | 'fixed' | 'remainder';

/**
 * 收款主体类型。规则项与账户**共用一套词表**（008 的 CHECK 要求两边逐字一致）。
 *
 * `platform` 是特殊的那一个：008 把它与「没有账户」写成了充要条件
 * （`CHECK ((party_type='platform') = (account_id IS NULL))`）。
 */
export type SettlementPartyType = 'partner' | 'city_center' | 'agent' | 'member_store' | 'platform';

/**
 * `settlement_rules.allocation_mode`：先扣固定额还是各算各的。
 *
 * `normal`：各接收方按各自的比例/固定额拿，剩下的归平台。
 * `fixed_then_remaining`：**先扣掉所有固定额**，剩余部分再按比例分。
 */
export type SettlementAllocationMode = 'normal' | 'fixed_then_remaining';

/**
 * 规则与账户共用的启用状态（两张表的 CHECK 是同一对取值）。
 *
 * 与下面那组任务状态是**两件事**，只是名字撞在一起：这两个回答「这条配置参不参与命中」，
 * 那些回答「这一笔钱分到哪一步了」。
 */
export type SettlementRecordStatus = 'enabled' | 'disabled';

/**
 * `settlement_tasks.status`：一笔分账走到哪一步了。**六个取值**。
 *
 * 今天本服务只写得出三个：`pending`（发起支付时建）、`succeeded`（支付成功回调里置）、
 * `cancelled`（支付失败/超时关单时作废）。另外三个是留给微信四步分账那条路与将来的
 * ——`submitted`（先发起、等回执）、`failed`（渠道明确拒绝）、`returned`（已退回）。
 * 登记它们不是为了好看：筛选框要能筛出库里真有的每一行，而「筛不出来」会在那些值真出现的
 * 那一天变成一个新的谜。
 *
 * 注意 **`succeeded` 不等于「渠道回过一句分账成功」**：银联商务的分账指令随下单一次下发，
 * 成功应答本身就是分账成功的凭据（老系统同款口径）。渠道若实际分账失败，本地会显示成功。
 */
export type SettlementTaskStatus =
  | 'pending'
  | 'submitted'
  | 'succeeded'
  | 'failed'
  | 'returned'
  | 'cancelled';

/**
 * `settlement_receivers.status`：某一条接收方明细走到哪一步。**五个取值**（任务少了 submitted）。
 *
 * `returned` 与金额是钉在一起的：008 的 CHECK 要求 `status='returned'` 与
 * `reversed_amount = amount` 互为充要条件。回退本身（`settlement_reversals`）本刀不做，
 * 所以今天这里只会是 pending 或 succeeded。
 */
export type SettlementReceiverStatus =
  | 'pending'
  | 'succeeded'
  | 'failed'
  | 'returned'
  | 'cancelled';

/**
 * `settlement_accounts.receiver_type`：渠道侧那个号是什么号。
 *
 * `MERCHANT_ID` 是默认、也是今天唯一用得到的那个：银联商务按子商户号直接分。`PERSONAL_OPENID`
 * 是微信服务商分账要的那一种，跟着词表留着。
 */
export type SettlementReceiverType = 'MERCHANT_ID' | 'PERSONAL_OPENID';

// ——— 规则 ———

/**
 * 规则里的一项。
 *
 * `accountName` / `receiverId` 是账户的展示列（主体名与渠道侧的接收方号），**读的时候 JOIN
 * 出来、写的时候不收**——写只认 `accountId`。
 *
 * `ratioPercent` 是当初生效的比例（百分数）；固定额项记 0，它的数值在 `fixedAmount` 上；
 * remainder 项两个都是 0（008 的 CHECK 钉着）。
 */
export type SettlementRuleItem = {
  id: string;
  partyType: SettlementPartyType;
  calcType: SettlementCalcType;
  ratioPercent: number;
  /** 单位分。只有 calcType = 'fixed' 时有意义。 */
  fixedAmount: number;
  /** 空串表示平台项——008 的 CHECK 把「平台项没有账户」写死了。 */
  accountId: string;
  accountName: string;
  /** 账户上的子商户号，渠道认的就是它。 */
  receiverId: string;
  sortOrder: number;
  remark: string;
};

/**
 * 一条分账规则：**这类业务、这个范围上的钱怎么分**。
 *
 * 列表与详情同形，差别只在 `items`：**只有详情填**，列表页是空数组（列表不 JOIN 项）。
 */
export type SettlementRule = {
  id: string;
  name: string;
  bizType: SettlementBizType;
  scopeType: SettlementScopeType;
  /**
   * 范围引用。`scopeType` 是 global 时**必须是空串**（008 的 CHECK 是充要条件），其余四档
   * 必须给一个 uuid。今天真正会命中的只有门店 id 与设备 id 两种。
   */
  scopeRef: string;
  allocationMode: SettlementAllocationMode;
  status: SettlementRecordStatus;
  remark: string;
  createdAt: string;
  updatedAt: string;
  items: SettlementRuleItem[];
};

/** 后台规则列表的筛选条件。 */
export type SettlementRuleQuery = PageQuery & {
  /** 模糊搜（ILIKE），按规则名。 */
  name?: string;
  /** 空表示不筛。**传错的码后端回 400**，不是空列表——打错一个字母不该静默返回一片空白。 */
  bizType?: SettlementBizType;
  scopeType?: SettlementScopeType;
  status?: SettlementRecordStatus;
};

/** 提交规则里的一项。`accountId` 为空表示平台项。 */
export type SettlementRuleItemInput = {
  partyType: SettlementPartyType;
  calcType: SettlementCalcType;
  /** 百分数，最多两位小数（45 表示 45.00%）。percent 项必须落在 (0, 100]。 */
  ratioPercent: number;
  /** 单位分。fixed 项必须 > 0；其余算法下一律 0。 */
  fixedAmount: number;
  accountId: string;
  sortOrder: number;
  remark: string;
};

/**
 * 新建 / 整体更新一条规则的请求体。
 *
 * **规则项不单独开接口**：「同规则下比例合计 ≤ 100%、平台项至多一条、remainder 只能给平台」
 * 是**跨行**约束——拆成子资源逐行改，就没有任何一个时刻能在一个事务里看见完整的一套项，也就
 * 校验不了。所以 items 是**整体提交**：`updateSettlementRule` 在事务内全量替换掉原来的项
 * （后端没有 PATCH 语义，只发改动过的字段会把没发的清空）。
 */
export type SettlementRuleInput = {
  name: string;
  bizType: SettlementBizType;
  scopeType: SettlementScopeType;
  scopeRef: string;
  allocationMode: SettlementAllocationMode;
  /** 空表示 enabled：新建的规则默认参与命中，停用是一个明确动作。 */
  status: SettlementRecordStatus;
  remark: string;
  items: SettlementRuleItemInput[];
};

export async function listSettlementRules(params?: SettlementRuleQuery) {
  return request<PageResult<SettlementRule>>('/api/v1/admin/settlement/rules', { params });
}

/** 取一条规则详情。与列表的区别只有一个：这里带 `items`。 */
export async function getSettlementRule(id: string) {
  return request<SettlementRule>(`/api/v1/admin/settlement/rules/${id}`);
}

export async function createSettlementRule(data: SettlementRuleInput) {
  return request<SettlementRule>('/api/v1/admin/settlement/rules', { method: 'POST', data });
}

/**
 * 整体替换一条规则**连同它的项**（见 SettlementRuleInput 的说明）。
 *
 * 编辑时必须先把详情取出来（`getSettlementRule`）再在它上面改：列表里的 `items` 是空的，
 * 拿列表行整份发回来会把这条规则的项**全部清空**。
 */
export async function updateSettlementRule(id: string, data: SettlementRuleInput) {
  return request<SettlementRule>(`/api/v1/admin/settlement/rules/${id}`, { method: 'PUT', data });
}

/**
 * 删除一条规则。**被分账任务引用过的规则删不掉，后端回 409**（`settlement_tasks.rule_id` 是
 * ON DELETE RESTRICT）。
 *
 * 常规出口是**停用**，不是删：停用之后同业务分类同档位可以再配一条（008 的唯一索引带
 * `WHERE status='enabled'`），已经发生的那几笔分账也不受影响——它们用的是任务上的快照。
 */
export async function deleteSettlementRule(id: string) {
  return request<{ id: string }>(`/api/v1/admin/settlement/rules/${id}`, { method: 'DELETE' });
}

// ——— 账户 ———

/**
 * 一个收款账户：**钱分到谁的哪个子商户号上**。
 *
 * `provider` 是渠道名（catalog 里的那个值，如 `ums`），**不是 uuid**——渠道与支付方式在
 * identity/009 之后是代码里的目录表，库里没有可指向的那张表。
 *
 * `receiverId` 就是下发时子单里的 `mid`：银联商务按它找到收款方。它是**快照源头**——任务建
 * 下来的那一刻这个值会被冻结进 `settlement_receivers`，账户后来改了名或换了号都不会回头影响
 * 历史明细。这也是账户**改得**而明细**不改**的原因。
 *
 * **没有账户号与接收方名**（017 删的两列）：这一行给人看的名字就是 `partyName`，渠道侧要的号
 * 就是 `receiverId`，多出来的那两个标签谁也没读。
 */
export type SettlementAccount = {
  id: string;
  /** 收款主体名。必填（017 起），也是规则项下拉、接收方快照与列表里用的同一个名字。 */
  partyName: string;
  partyType: SettlementPartyType;
  provider: string;
  receiverType: SettlementReceiverType;
  receiverId: string;
  status: SettlementRecordStatus;
  remark: string;
  createdAt: string;
  updatedAt: string;
};

/** 后台账户列表的筛选条件。 */
export type SettlementAccountQuery = PageQuery & {
  /** **一个词搜两样**（主体名 / 子商户号）：运营手上那串是什么，他自己多半也说不清。 */
  keyword?: string;
  partyType?: SettlementPartyType;
  /** 渠道名，取值来自 `listSettlementChannels()`。 */
  provider?: string;
  status?: SettlementRecordStatus;
};

export type SettlementAccountInput = {
  partyName: string;
  partyType: SettlementPartyType;
  provider: string;
  /** 空表示 MERCHANT_ID（后端的归一化值与 008 的列默认值都是它）。 */
  receiverType: SettlementReceiverType;
  receiverId: string;
  /** 空表示 enabled。 */
  status: SettlementRecordStatus;
  remark: string;
};

export async function listSettlementAccounts(params?: SettlementAccountQuery) {
  return request<PageResult<SettlementAccount>>('/api/v1/admin/settlement/accounts', { params });
}

export async function getSettlementAccount(id: string) {
  return request<SettlementAccount>(`/api/v1/admin/settlement/accounts/${id}`);
}

export async function createSettlementAccount(data: SettlementAccountInput) {
  return request<SettlementAccount>('/api/v1/admin/settlement/accounts', { method: 'POST', data });
}

/** **整份替换**，同 updateSettlementRule。 */
export async function updateSettlementAccount(id: string, data: SettlementAccountInput) {
  return request<SettlementAccount>(`/api/v1/admin/settlement/accounts/${id}`, {
    method: 'PUT',
    data,
  });
}

/**
 * 删除一个账户。**被规则项引用过、或被历史分账明细引用过的账户删不掉，后端回 409**
 * （`settlement_rule_items.account_id` 与 `settlement_receivers.account_id` 都是 RESTRICT）。
 *
 * 常规出口是**停用**：停用之后不会再有新的分账分到它，历史明细仍然带着它当初的那些快照值。
 * 被引用几乎是必然的——一条账户只要分过一次钱就删不掉了。
 */
export async function deleteSettlementAccount(id: string) {
  return request<{ id: string }>(`/api/v1/admin/settlement/accounts/${id}`, { method: 'DELETE' });
}

/**
 * 「钱能分到哪条渠道上」的取值，给账户表单的渠道下拉用。
 *
 * 它来自**代码里的目录**（`catalog.Channel.Settlement`），不是一张表：能分账的渠道由「哪条
 * 渠道的下单报文里能带子单」决定，那是发版的事，不是运营的输入。**今天只有一条：银联商务。**
 *
 * 所以下拉里只有一项不是页面坏了——它是这个事实的如实反映。返回的是**裸数组**（不是分页
 * 信封），因为这个集合由代码决定、不可能长大。
 */
export type SettlementChannel = {
  /** 渠道 code，给人看与给人认的。 */
  code: string;
  name: string;
  /** 要存进 `SettlementAccount.provider` 的那个值。 */
  provider: string;
};

export async function listSettlementChannels() {
  return request<SettlementChannel[]>('/api/v1/admin/settlement/channels');
}

// ——— 明细（只读）———

/**
 * 一条分账任务：**一笔支付分账的那一次**。
 *
 * `orderNo` / `provider` / `method` 来自 JOIN `payments`——那些是支付单上的事实，任务表上再存
 * 一份就是同一件事的第二个写法。`ruleId` 为空是**常态**：门店没配规则时这条任务照样建，
 * 整单归平台。
 */
export type SettlementTask = {
  id: string;
  taskNo: string;
  /** 支付单号。详情页把它链到 `/payments/:id`。 */
  paymentNo: string;
  orderNo: string;
  /** 分账基数（实付金额，券后），单位分。 */
  baseAmount: number;
  /** 平台自留，单位分。**差额倒挤**：BaseAmount = PlatformAmount + Σ receivers.amount。 */
  platformAmount: number;
  status: SettlementTaskStatus;
  scopeType: SettlementScopeType;
  scopeRef: string;
  /** 三个主体维度，建任务那一刻的快照。 */
  storeRef: string;
  brandRef: string;
  merchantRef: string;
  ruleId: string;
  ruleName: string;
  provider: string;
  /** 支付方式的 code（catalog 里的那个值）。 */
  method: string;
  providerTaskNo: string;
  /**
   * 渠道那一笔交易的流水号。
   *
   * 拼写跟的是**后端 JSON 标签**（`dto/settlement.go`：Go 字段叫 ProviderTransactionID，
   * 标签是 `json:"providerTransactionId"`），不是 Go 那边的字段名。这里写成
   * `providerTransactionID` 的话详情页那一列恒显示「—」——与「这笔真没有流水号」长得一模一样。
   */
  providerTransactionId: string;
  attempts: number;
  lastError: string;
  createdAt: string;
  /** 分账成功的那一刻。pending 时是 null。 */
  finishedAt: string | null;
  updatedAt: string;
};

/** 一条接收方明细。**全部是快照列**，不回查账户补当前的名字与号。 */
export type SettlementReceiver = {
  id: string;
  accountId: string;
  partyType: SettlementPartyType;
  partyName: string;
  merchantRef: string;
  brandRef: string;
  storeRef: string;
  receiverType: SettlementReceiverType;
  receiverId: string;
  /** 当初生效的比例快照（百分数）。固定额项记 0，它的金额在 amount 上。 */
  ratioPercent: number;
  amount: number;
  reversedAmount: number;
  providerDetailNo: string;
  status: SettlementReceiverStatus;
  lastError: string;
  createdAt: string;
};

/** 后台明细列表的筛选条件。单号都是模糊匹配：只记得一截是常态。 */
export type SettlementTaskQuery = PageQuery & {
  taskNo?: string;
  paymentNo?: string;
  /** 按订单号筛。它**不在**任务表上，后端 JOIN `payments` 才有。 */
  orderNo?: string;
  status?: SettlementTaskStatus;
  scopeType?: SettlementScopeType;
  /**
   * 三个主体维度各一个等值条件（都是 uuid）。老系统按这三个维度分了三个接口，这里一个接口
   * 三个参数——它们是同一张表上的同一件事，拆开之后「按门店和品牌一起筛」就没地方写了。
   */
  storeRef?: string;
  brandRef?: string;
  merchantRef?: string;
  /** 闭开区间的两端，都必须是**完整的 RFC3339**（`2026-01-01` 这种短式会被拒）。 */
  createdFrom?: string;
  createdTo?: string;
};

/** 分账明细详情：任务本身加上它的接收方明细。 */
export type SettlementTaskDetail = {
  task: SettlementTask;
  receivers: SettlementReceiver[];
};

export async function listSettlementTasks(params?: SettlementTaskQuery) {
  return request<PageResult<SettlementTask>>('/api/v1/admin/settlement/tasks', { params });
}

export async function getSettlementTask(id: string) {
  return request<SettlementTaskDetail>(`/api/v1/admin/settlement/tasks/${id}`);
}
