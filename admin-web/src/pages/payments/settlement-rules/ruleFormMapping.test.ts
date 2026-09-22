import { describe, expect, it } from 'vitest';
import type { SettlementRuleItem } from '../../../services/settlement';
import { emptyItem, itemToForm, itemToPayload, ruleScopeRef } from './ruleFormMapping';

/**
 * 这一份测的是**金额的单位**。
 *
 * 那条 bug 的形状：编辑一条固定额 12.34 元的规则，表单里出现 1234.00 元，保存回去变成
 * 123400 分——页面上从头到尾不报一个错，`tsc` 也查不出来（两侧都是 number）。
 *
 * 所以第一条用例是**往返**：详情 → 表单 → 提交，金额必须原样回来。它同时接住两个方向上的
 * 写反（进错、出对，或者进对、出错）。
 */

const fixedItem = (fixedAmount: number): SettlementRuleItem => ({
  id: 'item-1',
  partyType: 'member_store',
  calcType: 'fixed',
  ratioPercent: 0,
  fixedAmount,
  accountId: 'acct-1',
  accountName: '一号店',
  receiverId: 'MID-1',
  sortOrder: 0,
  remark: '',
});

describe('itemToForm / itemToPayload', () => {
  it('固定额的元与分是往返的', () => {
    // 1234 分 = 12.34 元。写反了这里就是 1234。
    expect(itemToForm(fixedItem(1234)).fixedAmount).toBe(12.34);
    // 再走回去仍然是 1234 分，不是 123400。
    expect(itemToPayload(itemToForm(fixedItem(1234)), 0).fixedAmount).toBe(1234);
  });

  it('分母除不尽的金额走不了往返，但不会越走越大', () => {
    // 0.005 元 = 0.5 分，`yuanToFen` 四舍五入成 1 分；再来一次是 0.01 元。
    // 这类值页面录入时就进不来（ProFormDigit 有 precision={2}），在这里是为了说明
    // 「往返」的边界在哪：它不是无损的，只是不会一次放大一百倍。
    const once = itemToPayload(itemToForm(fixedItem(1)), 0);
    expect(once.fixedAmount).toBe(1);
  });

  it('五项互斥与默认值按算法归零', () => {
    // 比例项：哪怕表单里留着一个旧固定额（切换过算法的残留），提交时也必须是 0。
    expect(
      itemToPayload({ partyType: 'member_store', calcType: 'percent', ratioPercent: 45, fixedAmount: 12 }, 0),
    ).toMatchObject({ ratioPercent: 45, fixedAmount: 0 });

    // 固定额项：比例反过来归零。
    expect(
      itemToPayload({ partyType: 'member_store', calcType: 'fixed', ratioPercent: 45, fixedAmount: 12 }, 0),
    ).toMatchObject({ ratioPercent: 0, fixedAmount: 1200 });

    // 平台项没有账户（008 的 CHECK 是等价式），留着的一律清掉。
    expect(
      itemToPayload(
        { partyType: 'platform', calcType: 'remainder', ratioPercent: 45, fixedAmount: 12, accountId: 'acct-1' },
        2,
      ),
    ).toMatchObject({ ratioPercent: 0, fixedAmount: 0, accountId: '', sortOrder: 2 });
  });

  it('编辑器里没填的可空字段落成空串而不是 undefined', () => {
    // 后端收的是空串（`accountId: ''` 就是平台项的写法），undefined 会被 JSON 丢掉，
    // 丢掉之后这一项在报文里就少一个键。
    const payload = itemToPayload({ partyType: 'member_store', calcType: 'fixed', fixedAmount: 1 }, 0);
    expect(payload.accountId).toBe('');
    expect(payload.remark).toBe('');
  });

  it('新建一项的默认值是一个门店档比例项', () => {
    expect(emptyItem()).toMatchObject({ partyType: 'member_store', calcType: 'percent' });
  });
});

describe('ruleScopeRef', () => {
  it('全局档一律空串，其余四档原样发', () => {
    // 008 的 CHECK 是充要条件：global 带引用是一条违规，非 global 不带引用也是一条。
    expect(ruleScopeRef('global', 'store-uuid')).toBe('');
    expect(ruleScopeRef('store', 'store-uuid')).toBe('store-uuid');
    expect(ruleScopeRef('device', 'device-uuid')).toBe('device-uuid');
    // 换档位之后引用清空了（还没重选），发出去的是空串——由后端回 400 说「必须给引用」，
    // 而不是在这里编一个。
    expect(ruleScopeRef('store', undefined)).toBe('');
  });
});
