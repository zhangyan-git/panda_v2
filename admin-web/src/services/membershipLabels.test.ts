import { describe, expect, it } from 'vitest';
import {
  CHANGE_OPERATOR,
  CHANGE_TYPE,
  MEMBERSHIP_STATUS,
  MEMBER_PRICE_MODE,
  PLAN_PERIOD,
  PLAN_STATUS,
  memberPriceLabel,
  periodLabel,
} from './membershipLabels';

/**
 * 与 orderLabels.test.ts / lotteryLabels.test.ts 同一个理由：钉的是**取值全集**而不是
 * 「有没有文案」。前者能在后端加码时失败，后者只在漏写文案时失败——而多出来的那个码
 * 才是常见的。
 *
 * 这些集合来自 membership-service 的 model 常量，也就是 migrations/membership
 * 的 CHECK 约束。逐一对着那份迁移核过，所以**迁移里加了码而这里没跟，这一份会红**。
 */
describe('会员枚举文案表', () => {
  it('覆盖 membership_plans.status 的三个取值', () => {
    expect(Object.keys(PLAN_STATUS).sort()).toEqual(['active', 'disabled', 'draft']);
  });

  it('覆盖 period 的两个取值', () => {
    expect(Object.keys(PLAN_PERIOD).sort()).toEqual(['month', 'year']);
  });

  it('覆盖 member_price_mode 的两个取值', () => {
    expect(Object.keys(MEMBER_PRICE_MODE).sort()).toEqual(['auto', 'coupon']);
  });

  it('覆盖 memberships.status 的四个取值', () => {
    expect(Object.keys(MEMBERSHIP_STATUS).sort()).toEqual([
      'active',
      'expired',
      'frozen',
      'revoked',
    ]);
  });

  it('覆盖 change_type 的十四个取值', () => {
    // 其中几个今天写不出来（签约、退款、到期扫描那几条路都还没实现），照登是因为迁移里的
    // CHECK 已经把它们写全了——见 membershipLabels 里的说明。
    //
    // charge_failed / suspend 来自代扣链路：它们在**已经落地**的那条路上，
    // 后端集成用例（charge_integration_test.go）直接断言这两个码会被写出来。这一份漏登过
    // 一次，症状是时间线与筛选下拉里原样显示英文码。
    expect(Object.keys(CHANGE_TYPE).sort()).toEqual([
      'activate',
      'admin_adjust',
      'auto_renew_off',
      'auto_renew_on',
      'charge_failed',
      'expire',
      'freeze',
      'refund_adjust',
      'renew',
      'revoke',
      'subscribe',
      'suspend',
      'unfreeze',
      'unsubscribe',
    ]);
  });

  it('覆盖 operator_type 的四个取值', () => {
    expect(Object.keys(CHANGE_OPERATOR).sort()).toEqual(['admin', 'system', 'user', 'worker']);
  });
});

describe('periodLabel', () => {
  it('单位与个数一起读', () => {
    expect(periodLabel('month', 1)).toBe('1 月');
    expect(periodLabel('year', 1)).toBe('1 年');
  });

  it('多月也照实说，不折算成「1 年」', () => {
    // 12 个月与 1 年对买的人可能是同一个东西，但库里存的是两个不同的套餐定义
    // （period/periodCount 是对账时要用的两个数），这里替后端折算就把那个信息抹掉了。
    expect(periodLabel('month', 12)).toBe('12 月');
  });

  it('个数为零时只显示单位，不显示「0 月」', () => {
    // CHECK 是 period_count > 0，真出现 0 时说「0 月」看起来像这个套餐买不到任何东西。
    expect(periodLabel('month', 0)).toBe('月');
  });

  it('认不出的单位原样回显，不返回空串', () => {
    // 后端加了新单位而这张表没跟上时，宁可在界面上看见那个码。
    expect(periodLabel('week', 2)).toBe('2 week');
  });
});

describe('memberPriceLabel', () => {
  it('自动享的套餐不加张数', () => {
    expect(memberPriceLabel('auto', 0)).toBe('自动享会员价');
  });

  it('体验券模式把每期张数带上', () => {
    // 「会员价体验券」这句话答不了运营的下一个问题（一个月给几张），而那个数就在同一行上。
    expect(memberPriceLabel('coupon', 20)).toBe('会员价体验券 · 每期 20 张');
  });

  it('张数为零时退回纯模式名，不显示「每期 0 张」', () => {
    expect(memberPriceLabel('coupon', 0)).toBe('会员价体验券');
  });
});
