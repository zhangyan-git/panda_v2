import { describe, expect, it } from 'vitest';
import {
  ALLOCATION_MODE,
  BIZ_TYPE,
  CALC_TYPE,
  PARTY_TYPE,
  PERCENT_MAX_HUNDREDTHS,
  RECEIVER_STATUS,
  RECEIVER_TYPE,
  RECORD_STATUS,
  SCOPE_NOT_HIT_TODAY,
  SCOPE_TYPE,
  TASK_STATUS,
  formatPercent,
  percentSumHundredths,
  scopeNeedsRef,
  scopeNeverHits,
} from './settlementLabels';

/**
 * 与 membershipLabels.test.ts / orderLabels.test.ts 同一个理由：钉的是**取值全集**而不是
 * 「有没有文案」。前者能在后端加码时失败，后者只在漏写文案时失败——而多出来的那个码才是常见的。
 *
 * 这些集合来自 payment-service 的 internal/model/settlement.go，也就是
 * migrations/payment 的 CHECK 约束。逐一对着那份迁移核过，所以
 * **迁移里加了码而这里没跟，这一份会红**。
 */
describe('分账枚举文案表', () => {
  it('覆盖 settlement_rules.biz_type 的四个取值', () => {
    expect(Object.keys(BIZ_TYPE).sort()).toEqual([
      'addon_product',
      'coffee',
      'membership',
      'store_consume',
    ]);
  });

  it('覆盖 settlement_rules.scope_type 的五个取值', () => {
    // 顺序也是命中顺序（从具体到宽泛），所以这里不只钉全集，连顺序一起钉住——
    // 换掉顺序会让某个档位的规则悄悄不再被命中。
    expect(Object.keys(SCOPE_TYPE)).toEqual(['device', 'store', 'brand', 'product', 'global']);
    expect(Object.keys(SCOPE_TYPE).sort()).toEqual([
      'brand',
      'device',
      'global',
      'product',
      'store',
    ]);
  });

  it('覆盖 settlement_rule_items.calc_type 的三个取值', () => {
    expect(Object.keys(CALC_TYPE).sort()).toEqual(['fixed', 'percent', 'remainder']);
  });

  it('覆盖 party_type 的五个取值', () => {
    expect(Object.keys(PARTY_TYPE).sort()).toEqual([
      'agent',
      'city_center',
      'member_store',
      'partner',
      'platform',
    ]);
  });

  it('覆盖 allocation_mode 的两个取值', () => {
    expect(Object.keys(ALLOCATION_MODE).sort()).toEqual(['fixed_then_remaining', 'normal']);
  });

  it('覆盖规则与账户共用的那两个状态', () => {
    expect(Object.keys(RECORD_STATUS).sort()).toEqual(['disabled', 'enabled']);
  });

  it('覆盖 settlement_tasks.status 的六个取值', () => {
    expect(Object.keys(TASK_STATUS).sort()).toEqual([
      'cancelled',
      'failed',
      'pending',
      'returned',
      'submitted',
      'succeeded',
    ]);
  });

  it('覆盖 settlement_receivers.status 的五个取值', () => {
    // 比任务少一个 submitted：明细没有「已提交但没结果」这一档。
    expect(Object.keys(RECEIVER_STATUS).sort()).toEqual([
      'cancelled',
      'failed',
      'pending',
      'returned',
      'succeeded',
    ]);
  });

  it('覆盖 receiver_type 的两个取值', () => {
    expect(Object.keys(RECEIVER_TYPE).sort()).toEqual(['MERCHANT_ID', 'PERSONAL_OPENID']);
  });

  it('每一格文案都不是空的，也没有退回原始码', () => {
    for (const table of [
      BIZ_TYPE,
      SCOPE_TYPE,
      CALC_TYPE,
      PARTY_TYPE,
      ALLOCATION_MODE,
      RECORD_STATUS,
      TASK_STATUS,
      RECEIVER_STATUS,
      RECEIVER_TYPE,
    ]) {
      for (const [code, meta] of Object.entries(table)) {
        expect(meta.text, code).toBeTruthy();
        expect(meta.text, code).not.toBe(code);
        expect(meta.color, code).toBeTruthy();
      }
    }
  });
});

/**
 * 「今天命中不了」那两档。
 *
 * 它不是一个文案问题，是一个**钱的问题**：配了一条按品牌的规则，列表上看起来完全正常，
 * 而每一笔订单都命不中它、整单归平台。所以这个集合要被钉住——多一档（比如把 store 也写进去）
 * 会让页面在一个其实生效的档位上撒谎。
 */
describe('今天命中不了的范围档位', () => {
  it('只有品牌与商品两档', () => {
    expect([...SCOPE_NOT_HIT_TODAY].sort()).toEqual(['brand', 'product']);
  });

  it('门店与设备是命中的', () => {
    expect(scopeNeverHits('store')).toBe(false);
    expect(scopeNeverHits('device')).toBe(false);
    expect(scopeNeverHits('global')).toBe(false);
    expect(scopeNeverHits('brand')).toBe(true);
    expect(scopeNeverHits('product')).toBe(true);
  });

  it('空值不算命中不了', () => {
    // 表单刚打开、还没选档位时会走到这里，那时候不该先弹一句「配了不会命中」。
    expect(scopeNeverHits(undefined)).toBe(false);
    expect(scopeNeverHits('')).toBe(false);
  });
});

/**
 * 范围引用的必填判据。
 *
 * `settlement_rules` 上那条 CHECK 是**充要条件**（`(scope_type='global') = (scope_ref='')`），两个方向都要管：
 * 选了全局却带着一个门店 id、或者选了门店却没给 id，两条都是 CHECK 违规，用户看到的会是一句
 * 没有线索的兜底。所以这个判据要被钉住。
 */
describe('范围引用要不要填', () => {
  it('全局档不要引用', () => {
    expect(scopeNeedsRef('global')).toBe(false);
  });

  it('其余四档都要', () => {
    expect(scopeNeedsRef('device')).toBe(true);
    expect(scopeNeedsRef('store')).toBe(true);
    expect(scopeNeedsRef('brand')).toBe(true);
    expect(scopeNeedsRef('product')).toBe(true);
  });

  it('空值不要引用', () => {
    expect(scopeNeedsRef(undefined)).toBe(false);
    expect(scopeNeedsRef('')).toBe(false);
  });
});

/**
 * 比例合计。
 *
 * 这是这一份里最要紧的几个断言：后端遇到「合计 > 100%」时**不会报错**，`computeSettlement`
 * 会静默走那条「分出去的钱比收进来的多」的兜底、整单归平台。配错的人与收到钱的人都看不见异常。
 * 页面上的这句实时提示是最后一道能让人回头看一眼的地方，所以它自己的判据要准。
 */
describe('比例合计', () => {
  const percent = (ratioPercent: number) => ({ calcType: 'percent', ratioPercent });
  const fixed = (ratioPercent: number) => ({ calcType: 'fixed', ratioPercent });

  it('只有按比例的项参与合计', () => {
    // 固定额项与平台自留项的比例是 0（`settlement_rule_items` 上那条 CHECK 钉着），它们本来就不该出现在这个和里。
    expect(percentSumHundredths([percent(45), fixed(100), { calcType: 'remainder' }])).toBe(4500);
  });

  it('刚好 100% 不算超', () => {
    expect(percentSumHundredths([percent(60), percent(40)])).toBe(PERCENT_MAX_HUNDREDTHS);
  });

  it('三项凑满 100 时**不多出一点**（浮点会）', () => {
    // 这一条是照 payment-service 的 TestSettlementRulePercentOverflow 抄的：95.29 + 2.93 + 1.78
    // 在十进制上正好 100，整数百分点求和也是 10000；但 float64 加起来是 100.00000000000002，
    // 乘 100 之后比 10000 大出 2e-12 —— 一条**完全合法**的规则会被浮点判据误拒。
    const sum = percentSumHundredths([percent(95.29), percent(2.93), percent(1.78)]);
    expect(sum).toBe(10000);

    // 反证：直接把浮点数加起来确实会多出一点。这一句是上面那条整数算法存在的理由，
    // 真有人把它改回浮点求和时，它会红。
    expect(95.29 + 2.93 + 1.78).toBeGreaterThan(100);
  });

  it('超出一分就算超', () => {
    expect(percentSumHundredths([percent(60), percent(40.01)])).toBeGreaterThan(
      PERCENT_MAX_HUNDREDTHS,
    );
  });

  it('空列表与缺省值是 0', () => {
    expect(percentSumHundredths([])).toBe(0);
    expect(percentSumHundredths([{}, { ratioPercent: null }])).toBe(0);
  });
});

describe('百分数展示', () => {
  it('去掉小数末尾的零', () => {
    expect(formatPercent(45)).toBe('45%');
    expect(formatPercent(45.5)).toBe('45.5%');
    expect(formatPercent(45.55)).toBe('45.55%');
  });

  it('零与空值是 0%', () => {
    expect(formatPercent(0)).toBe('0%');
    expect(formatPercent(null)).toBe('0%');
    expect(formatPercent(undefined)).toBe('0%');
  });

  it('浮点尾巴不会显示出来', () => {
    // 45 * 100 / 100 这类来回换算容易留下 45.000000000000004 这种尾巴。
    expect(formatPercent(45.000000000000004)).toBe('45%');
    expect(formatPercent(99.999999999)).toBe('100%');
  });
});
