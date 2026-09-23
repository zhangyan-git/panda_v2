import type { EnumMeta } from './labels';
import type { SettlementCalcType, SettlementScopeType } from './settlement';

/**
 * 分账域那些枚举码的中文文案。
 *
 * 取值来自 payment-service 的 `internal/model/settlement.go`（也就是
 * `migrations/payment` 的 CHECK 约束），不是接口给的——接口回的就是库里
 * 那些英文码。所以**改枚举必须同时改迁移、改 model 和这里**，加了新码而没登记，界面上就退回
 * 显示原始码。
 *
 * 集中放一个文件，理由与 membershipLabels / orderLabels 相同：同一个码会出现在好几个地方
 * （主体类型在规则页、账户页、明细详情三处都有；启用状态在规则与账户两张表上是同一对取值），
 * 分开写一定漂移，而漂移的表现是同一件事在两页显示成两种说法——不报错，只让人怀疑数据本身。
 *
 * 表在这里，翻码的那两个函数（enumMeta / searchOptions）在 services/labels.ts。
 */

/**
 * `settlement_rules.biz_type`：这笔钱是哪一类业务收上来的。
 *
 * `store_consume` 是**今天推不上来**的那一个：它是老系统 store_pos 的存量口径，V2 没有产生
 * 它的来源（order-service 只有咖啡、会员、加购三条）。照登是因为词表里本来就有它，规则可以
 * 配——将来订单侧补上这一维时不用改这里。
 */
export const BIZ_TYPE: Record<string, EnumMeta> = {
  coffee: { text: '咖啡订单', color: 'blue' },
  membership: { text: '会员订阅', color: 'purple' },
  store_consume: { text: '到店消费', color: 'cyan' },
  addon_product: { text: '加购商品', color: 'gold' },
};

/**
 * `settlement_rules.scope_type`：这条规则管多大一片地方。**声明顺序就是命中顺序**（从具体到
 * 宽泛，先命中先返回），所以这里的顺序不是随便排的。
 *
 * 五档后台都配得出来，但**今天只有门店与设备会命中**——见 SCOPE_NOT_HIT_TODAY。
 */
export const SCOPE_TYPE: Record<string, EnumMeta> = {
  device: { text: '按设备', color: 'geekblue' },
  store: { text: '按门店', color: 'blue' },
  brand: { text: '按品牌', color: 'cyan' },
  product: { text: '按商品', color: 'gold' },
  global: { text: '全局', color: 'default' },
};

/**
 * 今天配了也**不会命中**的两档。
 *
 * 订单侧发起支付时给的 `SettlementRuleQuery` 只有门店与设备两维（它是照老系统那三个维度里
 * 真正在跑的两个来的）。品牌与商品这两档留着是因为词表与老系统都是五档，订单侧补齐这一维时
 * 不用改表——但**在那之前配了它等于配了个永远不生效的规则**，而且列表上看起来完全正常。
 *
 * 页面据此在选中这两档时给一句提示。**不隐藏这两档**：藏起来的话，将来订单侧接了这两维，
 * 还得再把它们放回去，而中间这段时间里没人知道它们曾经存在过。
 */
export const SCOPE_NOT_HIT_TODAY: SettlementScopeType[] = ['brand', 'product'];

/** `settlement_rule_items.calc_type`：这一项按什么算。 */
export const CALC_TYPE: Record<string, EnumMeta> = {
  percent: { text: '按比例', color: 'blue' },
  fixed: { text: '固定额', color: 'cyan' },
  // 「平台自留」而不是「剩余」：它只能给平台项用（`settlement_rule_items` 上那条 CHECK 钉死了），拿的是差额，
  // 与上面两项是两种东西——写「剩余」会让人以为任何一个主体都能选它。
  remainder: { text: '平台自留', color: 'default' },
};

/**
 * 收款主体类型。规则项与账户共用一套词表。
 *
 * `platform` 是特殊的那一个：它不能挂账户（`settlement_rule_items` 上那条 CHECK 把它写成了充要条件），而且一条规则里至多出现
 * 一次。所以它在这一列上的含义与另外四个不完全一样——另外四个是「分给谁」，它是「剩下的归自己」。
 */
export const PARTY_TYPE: Record<string, EnumMeta> = {
  partner: { text: '合作方', color: 'purple' },
  city_center: { text: '城市中心', color: 'geekblue' },
  agent: { text: '代理商', color: 'cyan' },
  member_store: { text: '加盟门店', color: 'blue' },
  platform: { text: '平台', color: 'default' },
};

/** `settlement_rules.allocation_mode`：先扣固定额还是各算各的。 */
export const ALLOCATION_MODE: Record<string, EnumMeta> = {
  normal: { text: '各自计算', color: 'blue' },
  fixed_then_remaining: { text: '先扣固定额再分', color: 'purple' },
};

/**
 * 规则与账户共用的启用状态。**两张表上的同一对取值**，所以只有这一份。
 *
 * 与下面的 SWITCH 是两件事：这一对回答「这条配置参不参与命中」，「停用」在这里是常规出口
 * ——被引用过的规则与账户删不掉（RESTRICT），停用是唯一能做的那个动作。
 */
export const RECORD_STATUS: Record<string, EnumMeta> = {
  enabled: { text: '已启用', color: 'success' },
  disabled: { text: '已停用', color: 'warning' },
};

/**
 * `settlement_tasks.status`：一笔分账走到哪一步了。**六个取值**。
 *
 * 今天只会出现 pending / succeeded / cancelled 三个（本服务只写得出这三个）。另外三个照登，
 * 理由见 services/settlement.ts 里 SettlementTaskStatus 的说明。
 *
 * **succeeded 说的是「这笔支付成功了」，不是「渠道回过一句分账成功」**——银联商务的分账指令
 * 随下单一次下发，成功应答本身就是分账成功的凭据。所以这一列没有「分账中」这个中间态，
 * 这不是缺状态，是口径（老系统同款）。
 */
export const TASK_STATUS: Record<string, EnumMeta> = {
  pending: { text: '待分账', color: 'processing' },
  submitted: { text: '已提交', color: 'blue' },
  succeeded: { text: '分账成功', color: 'success' },
  failed: { text: '分账失败', color: 'error' },
  returned: { text: '已退回', color: 'warning' },
  // 支付没成，任务跟着作废。**不是失败**：那时还没发往渠道，没有要收回的钱。
  cancelled: { text: '已作废', color: 'default' },
};

/**
 * `settlement_receivers.status`：某一条接收方明细走到哪一步。**五个取值**（比任务少
 * submitted——明细没有「已提交但没结果」这一档）。
 *
 * `returned` 与金额是钉在一起的：`settlement_receivers` 上那条 CHECK 要求它与「已全额回退」互为充要条件。回退本身
 * （settlement_reversals）本刀不做，所以今天这里只会是待分账或分账成功。
 */
export const RECEIVER_STATUS: Record<string, EnumMeta> = {
  pending: { text: '待分账', color: 'processing' },
  succeeded: { text: '分账成功', color: 'success' },
  failed: { text: '分账失败', color: 'error' },
  returned: { text: '已退回', color: 'warning' },
  cancelled: { text: '已作废', color: 'default' },
};

/** `settlement_accounts.receiver_type`：渠道侧那个号是什么号。 */
export const RECEIVER_TYPE: Record<string, EnumMeta> = {
  MERCHANT_ID: { text: '子商户号', color: 'blue' },
  // 今天用不到：银联商务按子商户号直接分，取不到「个人 openid」这个概念。
  PERSONAL_OPENID: { text: '个人 OpenID', color: 'default' },
};

/**
 * 范围档位要不要填范围引用。
 *
 * `settlement_rules` 上那条 CHECK 是**充要条件**：`(scope_type='global') = (scope_ref='')`。所以两个方向都要管
 * ——选了全局就绝不能带引用（带了是一条 CHECK 违规），选了其余四档就必须给。
 */
export const scopeNeedsRef = (scopeType?: string | null) => !!scopeType && scopeType !== 'global';

/** 这一档今天命中不了吗（见 SCOPE_NOT_HIT_TODAY）。 */
export const scopeNeverHits = (scopeType?: string | null): boolean =>
  !!scopeType && (SCOPE_NOT_HIT_TODAY as string[]).includes(scopeType);

/** 百分比的上限，**以百分之一为单位**的整数（100% = 10000）。 */
export const PERCENT_MAX_HUNDREDTHS = 10000;

/**
 * 把一组比例项求和，**以百分之一为单位**。
 *
 * # 为什么是整数而不是浮点求和
 *
 * 后端拦「同规则下比例合计超过 100%」时用的是整数百分点：`95.29 + 2.93 + 1.78` 三项在十进制上
 * 正好 100，整数求和也是 10000，但 float64 加起来是 `100.00000000000002`——一条**完全合法**
 * 的规则会被浮点判据误拒。这里做的是页面上的实时提示，误报一次就足以让人不再信它，所以用
 * `Math.round(v * 100)` 先把每一项变成整数再相加：两位小数以内的值乘 100 之后取整是无损的，
 * 与后端 `RatioHundredths` 的算法逐字同源。
 *
 * 它**不替代后端那一条校验**（后端才是权威，且它还会看账户状态、渠道、平台项条数）。这里只是
 * 让人在按下保存之前就看见自己把 100% 分超了——那条错误配出来是不会报错的，只会静默整单归平台。
 */
export function percentSumHundredths(
  items: { calcType?: SettlementCalcType | string; ratioPercent?: number | null }[],
): number {
  return items.reduce(
    (sum, item) =>
      // 只有按比例的项参与合计：固定额与平台自留项没有比例（`settlement_rule_items` 上那条 CHECK 要求它们的 ratio = 0）。
      item?.calcType === 'percent' ? sum + Math.round(Number(item.ratioPercent ?? 0) * 100) : sum,
    0,
  );
}

/**
 * 百分数的展示：`45` → `45%`，`45.5` → `45.5%`。
 *
 * 去掉小数末尾的零而不是固定两位：这一列上 `45%` 与 `45.00%` 说的是同一件事，而运营配的时候
 * 打的就是 45。真需要两位的地方（元）有 money.ts 那份实现。
 */
export function formatPercent(value?: number | null): string {
  const percent = Number(value ?? 0);
  if (!Number.isFinite(percent)) return '—';
  // toFixed(2) 再剥掉末尾的零与小数点，避免 45.000000000000004 这种浮点尾巴显示出来。
  const text = percent.toFixed(2).replace(/\.?0+$/, '');
  return `${text || '0'}%`;
}
