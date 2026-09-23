import { describe, expect, it } from 'vitest';
import {
  AUDIT_STATUS,
  CLAIM_LIMIT_MODE,
  CLAIM_TYPE,
  TEMPLATE_STATUS,
  USER_COUPON_STATUS,
  VALIDITY_MODE,
} from './couponLabels';
import { enumMeta, searchOptions } from './labels';

/**
 * 这几张表是页面显示的全部依据，取值来自 migrations/coupon 的
 * CHECK 约束。这里逐条钉住码→文案，是为了让「迁移加了枚举但页面没跟上」在测试里
 * 就能看出来，而不是等运营在界面上看见一个 COFFEE_CASH。
 */
describe('枚举文案表', () => {
  it('覆盖 coupon_templates.audit_status 的三个取值', () => {
    expect(Object.keys(AUDIT_STATUS).sort()).toEqual(['approved', 'pending', 'rejected']);
  });

  it('覆盖 coupon_templates.status 的四个取值', () => {
    expect(Object.keys(TEMPLATE_STATUS).sort()).toEqual(['active', 'closed', 'disabled', 'draft']);
  });

  it('覆盖 user_coupons.status 的六个取值', () => {
    expect(Object.keys(USER_COUPON_STATUS).sort()).toEqual([
      'claimed', 'expired', 'held', 'invalidated', 'redeemed', 'refunded',
    ]);
  });

  it('覆盖 user_coupons.claim_type 的六个取值', () => {
    expect(Object.keys(CLAIM_TYPE).sort()).toEqual([
      'admin_assign', 'claim_by_code', 'daily_gift', 'event_reward', 'purchase', 'user_claim',
    ]);
  });

  it('覆盖 coupon_templates.validity_mode 的两个取值', () => {
    expect(Object.keys(VALIDITY_MODE).sort()).toEqual(['fixed', 'relative']);
  });

  it('覆盖 coupon_templates.claim_limit_mode 的三个取值', () => {
    expect(Object.keys(CLAIM_LIMIT_MODE).sort()).toEqual([
      'once_ever', 'periodic', 'unlimited_after_use',
    ]);
  });

  it('每个取值都有非空文案', () => {
    for (const map of [
      AUDIT_STATUS,
      TEMPLATE_STATUS,
      USER_COUPON_STATUS,
      CLAIM_TYPE,
      VALIDITY_MODE,
      CLAIM_LIMIT_MODE,
    ]) {
      for (const [value, meta] of Object.entries(map)) {
        expect(meta.text, value).toBeTruthy();
      }
    }
  });
});

describe('enumMeta', () => {
  it('认识的码给中文文案', () => {
    expect(enumMeta(AUDIT_STATUS, 'approved').text).toBe('已通过');
  });

  it('不认识的码原样返回', () => {
    // 关键行为：宁可显示一个看不懂的新码，也不要显示成空白——空白看起来像「没配值」，
    // 而实际上这是「库里出现了界面还不认识的枚举」的信号。
    expect(enumMeta(AUDIT_STATUS, 'archived')).toEqual({ text: 'archived', color: 'default' });
  });

  it.each([undefined, null, ''])('空值给占位符 %j', (value) => {
    expect(enumMeta(AUDIT_STATUS, value).text).toBe('—');
  });
});

describe('searchOptions', () => {
  it('只保留 text，供 ProTable 的搜索下拉直接用', () => {
    expect(searchOptions({ a: { text: '甲', color: 'red' } })).toEqual({ a: { text: '甲' } });
  });
});
