import { describe, expect, it } from 'vitest';
import {
  FORTUNE_CARD_ENTRY_TYPE,
  FORTUNE_CARD_FREEZE_STATUS,
  FORTUNE_CARD_REFERENCE_TYPE,
  fortuneCardAmountLabel,
} from './fortuneCardLabels';

/**
 * 与 orderLabels.test.ts 同一个理由：钉的是**取值全集**而不是「有没有文案」。
 * 前者能在后端加码时失败，后者只在漏写文案时失败——而多出来的那个码才是常见的。
 *
 * 这两个集合来自 account-service 的 model 常量，也就是 migrations/account/001 的
 * CHECK 约束（entry_type IN ('grant','draw','reverse')，reference_type 没有 CHECK、
 * 默认 ''）。
 */
describe('福卡枚举文案表', () => {
  it('覆盖 fortune_card_entries.entry_type 的三个取值', () => {
    expect(Object.keys(FORTUNE_CARD_ENTRY_TYPE).sort()).toEqual(['draw', 'grant', 'reverse']);
  });

  it('覆盖 reference_type 的四个取值（含空串这个合法的默认值）', () => {
    expect(Object.keys(FORTUNE_CARD_REFERENCE_TYPE).sort()).toEqual(['', 'draw', 'entry', 'order']);
  });

  it('覆盖 fortune_card_freezes.status 的两个取值', () => {
    expect(Object.keys(FORTUNE_CARD_FREEZE_STATUS).sort()).toEqual(['frozen', 'released']);
  });
});

describe('fortuneCardAmountLabel', () => {
  // 带符号是有意的：光看数字看不出这是进账还是出账，而「抽奖看起来像又发了一张」正是
  // 这个函数要防的那件事。
  it('发放带正号', () => {
    expect(fortuneCardAmountLabel(2)).toBe('+2');
  });

  it('抽奖与冲正保留负号', () => {
    expect(fortuneCardAmountLabel(-1)).toBe('-1');
  });

  it('零原样显示，不吞掉', () => {
    expect(fortuneCardAmountLabel(0)).toBe('0');
  });
});
