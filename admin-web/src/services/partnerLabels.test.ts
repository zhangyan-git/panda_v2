import { describe, expect, it } from 'vitest';
import { API_KEY_STATUS, PARTNER_ERROR_CODE, PARTNER_STATUS } from './partnerLabels';

/**
 * 与 paymentLabels.test.ts / orderLabels.test.ts 同一个理由：钉的是**取值全集**而不是
 * 「有没有文案」。前者能在后端加码时失败，后者只在漏写文案时失败——而多出来的那个码才是
 * 常见的。
 *
 * 三张表的来源是 partner-service 的 model 常量（migrations/partner 的 CHECK 约束）与
 * internal/ingress 的 errorCodes（逐一对着源码核过，2026-09）。**那边加码而这里没跟，这一份
 * 会红**——那正是我们要的：后台没有「列出错误码」的接口，这一份是它的第二份。
 */
describe('开放平台枚举文案表', () => {
  it('覆盖 partner_accounts.status 的两个取值', () => {
    expect(Object.keys(PARTNER_STATUS).sort()).toEqual(['disabled', 'enabled']);
  });

  it('覆盖 partner_api_keys.status 的两个取值', () => {
    expect(Object.keys(API_KEY_STATUS).sort()).toEqual(['disabled', 'enabled']);
  });

  it('与合作方状态是两张表，不是同一个对象', () => {
    // 同值但不同义：「这家合作方停用」是整条链路断了，「这一把密钥停用」通常是掐掉一把
    // 怀疑泄露的钥匙。抄成同一个对象的那天，有人会把一句文案改得更贴切，另一个场景就错了。
    expect(API_KEY_STATUS).not.toBe(PARTNER_STATUS);
  });

  it('覆盖 ingress 的十七个内部错误码', () => {
    // RATE_LIMITED 与 RATE_LIMIT_DEGRADED 必须都在：后者会出现在一条 2xx 的记录上
    // （限流器没建起来、这次放行了），合并成一个码就看不出「这次其实成功了」。
    expect(Object.keys(PARTNER_ERROR_CODE).sort()).toEqual([
      'API_KEY_DISABLED',
      'API_KEY_EXPIRED',
      'BODY_TOO_LARGE',
      'BODY_UNREADABLE',
      'CREDENTIAL_LOOKUP_FAILED',
      'IP_NOT_ALLOWED',
      'MISSING_CREDENTIALS',
      'NONCE_REPLAYED',
      'NONCE_STORE_UNAVAILABLE',
      'PARTNER_DISABLED',
      'PARTNER_EXPIRED',
      'RATE_LIMITED',
      'RATE_LIMIT_DEGRADED',
      'SECRET_UNREADABLE',
      'SIGNATURE_MISMATCH',
      'TIMESTAMP_OUT_OF_RANGE',
      'UNKNOWN_API_KEY',
    ]);
  });
});
