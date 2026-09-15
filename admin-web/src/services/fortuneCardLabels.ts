/**
 * 福卡账户域那些枚举码的中文文案。
 *
 * 取值来自 account-service 的 model 常量（也就是 migrations/account 里各表的 CHECK 约束），
 * 不是接口给的——接口回的就是库里那些英文码。所以**改枚举必须同时改迁移和这里**，加了新码
 * 而没登记，界面上就退回显示原始码。
 *
 * 单独一个文件而不是并进 orderLabels：福卡账户归 account-service，它的码跟着那张表走。
 * 两域的码混在一张表里，下一个改动的人会以为它们是同一个域的。
 */

import type { EnumMeta } from './labels';

/**
 * fortune_card_entries.entry_type：这一笔是哪种账变。
 *
 * 文案刻意用「赠送 / 抽奖消耗 / 冲正」而不是「收入 / 支出」：账变类型回答的是**为什么**变，
 * 而金额的正负（amount 是有符号的）已经回答了方向。写成「收入/支出」会让人分不清一次
 * 冲正到底加还是减。
 */
export const FORTUNE_CARD_ENTRY_TYPE: Record<string, EnumMeta> = {
  grant: { text: '赠送', color: 'gold' },
  draw: { text: '抽奖消耗', color: 'blue' },
  reverse: { text: '冲正', color: 'warning' },
};

/**
 * fortune_card_entries.reference_type：这条流水指向什么。
 *
 * 空串是合法的默认值（服务侧默认 ''），所以这里的 '' 不是漏写——它代表「这一条不指向
 * 任何单据」，文案给「—」。
 */
export const FORTUNE_CARD_REFERENCE_TYPE: Record<string, EnumMeta> = {
  '': { text: '—', color: 'default' },
  order: { text: '订单', color: 'blue' },
  draw: { text: '抽奖', color: 'cyan' },
  entry: { text: '流水', color: 'geekblue' },
};

/**
 * 流水金额的显示文案。
 *
 * 必须带符号：这个数是有符号的（发放为正、抽奖与冲正为负），只显示绝对值会让一次抽奖
 * 看起来像又发了一张。零在这里不该出现（后端 CHECK 卡的是 amount <> 0），但真出现了也
 * 按原来的样子显示，不悄悄吞掉。
 */
export function fortuneCardAmountLabel(amount: number): string {
  return amount > 0 ? `+${amount}` : `${amount}`;
}

/**
 * fortune_card_freezes.status：这张售后单冻着的卡现在是什么状态。
 *
 * 「已解冻」用 default 而不是绿：解冻不是一件好事也不是坏事，它只是「卡又能抽奖了」，
 * 而当初为什么冻、后来为什么解，答案在 reason 那一列。给绿色会让人以为这是一次成功操作。
 */
export const FORTUNE_CARD_FREEZE_STATUS: Record<string, EnumMeta> = {
  frozen: { text: '冻结中', color: 'warning' },
  released: { text: '已解冻', color: 'default' },
};
