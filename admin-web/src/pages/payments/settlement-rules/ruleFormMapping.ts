import { fenToYuan, yuanToFen } from '../../../services/money';
import type {
  SettlementRuleItem,
  SettlementRuleItemInput,
  SettlementScopeType,
} from '../../../services/settlement';
import { scopeNeedsRef } from '../../../services/settlementLabels';

/**
 * 分账规则表单与接口之间的**每一处换算**。
 *
 * # 为什么单独一个文件
 *
 * 规则这一页上写坏钱的方式只有一种，而它**不会报错**：金额进出的单位。接口给的是**分**（全仓
 * HTTP 层都是分），页面上一律按**元**录入（`services/money.ts` 是唯一那份换算）。编辑一条规则
 * 时要从分转到元、提交时要转回分——这两处各写各的，其中一处写反了，页面上看不出任何异常：
 * 「固定额 12.34 元」的那一项编辑进来会变成 1234 元，保存回去变成 123400 分。
 *
 * 所以换算连同「算法决定哪两个字段归零」一起摆在这里，并有 `ruleFormMapping.test.ts` 钉着
 * ——**其中一条是往返**（详情 → 表单 → 提交原样回来），它能同时接住两个方向上的写反。
 * 与 `pages/roles/permSelection.ts`、`pages/partners/keyAccess.ts` 是同一个做法：页面里的纯函数
 * 拿出来才能直接测。
 *
 * # 三个字段互相排斥
 *
 * service 与 `settlement_rule_items` 上那条 CHECK 都钉着：比例项不能带固定额、固定额项不能带比例、平台项不能带账户。表单里
 * 三个字段是**按算法条件渲染**的，切换算法之后旧值会留在一个不再渲染的输入框里——那是这种
 * 表单最容易出现的半截组合，所以在提交时按当前算法把它们归零，而不是等用户自己去清空一个
 * 已经看不见的框。
 */

/** 一项的默认值：一个还没填完的门店档比例项。主体类型与算法先给上，省两次点击。 */
export const emptyItem = (): Partial<SettlementRuleItemInput> => ({
  partyType: 'member_store',
  calcType: 'percent',
  sortOrder: 0,
  remark: '',
});

/**
 * 编辑时把库里的一项摊到表单上。**金额由分换成元**——页面上一律按元录入，反了就是 100 倍。
 *
 * 只有 `fixedAmount` 要转：`ratioPercent` 两侧都是百分数（45 表示 45.00%），`services/
 * settlement.ts` 里专门写过为什么不做 percent↔ratio 的换算。
 */
export const itemToForm = (item: SettlementRuleItem): Partial<SettlementRuleItemInput> => ({
  partyType: item.partyType,
  calcType: item.calcType,
  ratioPercent: item.ratioPercent,
  fixedAmount: fenToYuan(item.fixedAmount),
  accountId: item.accountId || undefined,
  sortOrder: item.sortOrder,
  remark: item.remark,
});

/**
 * 提交时把表单上的一项落成接口要的形状。**金额由元换回分**——与 `itemToForm` 是一对，两条
 * 一起看才是那笔钱的往返。
 *
 * `index` 就是最终的 `sortOrder`：顺序按提交时的先后重编，拖拽排序没有做（项在列表里的次序
 * 就是它提交的次序），所以表单上那个 `sortOrder` 字段是不作数的。
 */
export const itemToPayload = (
  item: Partial<SettlementRuleItemInput>,
  index: number,
): SettlementRuleItemInput => {
  const isPlatform = item.partyType === 'platform';
  const calcType = item.calcType;
  return {
    partyType: item.partyType as SettlementRuleItemInput['partyType'],
    calcType: calcType as SettlementRuleItemInput['calcType'],
    // 三项互斥，按当前算法归零——见文件头的说明。
    ratioPercent: calcType === 'percent' ? Number(item.ratioPercent ?? 0) : 0,
    fixedAmount: calcType === 'fixed' ? yuanToFen(item.fixedAmount) : 0,
    accountId: isPlatform ? '' : (item.accountId ?? ''),
    sortOrder: index,
    remark: item.remark ?? '',
  };
};

/**
 * 规则上的范围引用该发什么。
 *
 * 全局档**绝不能带引用**（`settlement_rules` 上那条 CHECK 是充要条件：`scope_type='global' ⇔ scope_ref=''`），
 * 其余四档必须给一个 uuid。所以按档位把它归零，而不是靠人记得清空——换档位时表单里那个框
 * 会换一个控件重新渲染，旧值留着是常态。换档位时页面还会主动清一次（见 index.tsx 里
 * scopeType 的 onChange）：**提交时这条是最后的兜底，不是唯一的一道**。
 */
export const ruleScopeRef = (scopeType: SettlementScopeType, scopeRef?: string) =>
  scopeNeedsRef(scopeType) ? (scopeRef ?? '') : '';
