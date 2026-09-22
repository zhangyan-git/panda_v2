import { describe, expect, it } from 'vitest';
import {
  ACTOR_TYPE,
  AGREEMENT_CHARGE_STATUS,
  FUNDING_STATUS,
  NOTIFICATION_STATUS,
  PAYMENT_STATUS,
  PROVIDER_OPERATION,
  PROVIDER_RESULT,
  TRANSACTION_DIRECTION,
  TRANSACTION_KIND,
} from './paymentLabels';

/**
 * 与 orderLabels.test.ts / couponLabels.test.ts / lotteryLabels.test.ts 同一个理由：
 * 钉的是**取值全集**而不是「有没有文案」。前者能在后端加码时失败，后者只在漏写文案时失败
 * ——而多出来的那个码才是常见的。
 *
 * 这些集合来自 payment-service 的 model 常量，也就是 migrations/payment 各表的 CHECK 约束，
 * 逐一对着活库的 `pg_get_constraintdef` 核过（2026-09），所以**迁移里加了码而这里没跟，
 * 这一份会红**。
 *
 * 支付方式与渠道那三张表已经删了（009 迁移），方式不再是枚举而是一张代码里的常量表
 * （provider/catalog.go），所以这里没有它们的位置。
 */
describe('支付枚举文案表', () => {
  it('覆盖 payments.status 的六个取值', () => {
    // closed 是定义了但今天没有代码路径会写的那个（订单取消不关支付单，要等超时收成
    // expired）。它仍然要登记：CHECK 里有这个值，真出现时得看得见。
    expect(Object.keys(PAYMENT_STATUS).sort()).toEqual([
      'closed',
      'created',
      'expired',
      'failed',
      'pending',
      'succeeded',
    ]);
  });

  it('覆盖 payment_agreement_charges.status 的六个取值', () => {
    // 这一张是「包月订阅」详情抽屉的续费明细那一列用的（那一页走 membership-service 的
    // 代理，所以文案表挂在支付域这一份上——码是这张表来的）。
    expect(Object.keys(AGREEMENT_CHARGE_STATUS).sort()).toEqual([
      'cancelled',
      'charging',
      'failed',
      'pending',
      'skipped',
      'succeeded',
    ]);
  });

  // 「出资类型」那张 FUNDING_TYPE 不在这里了：四列今天存的是支付方式的 code，那套词表
  // 已经整个退场（payment/012），文案回到 paymentMethodLabels 那一份。
  it('覆盖 payment_fundings.status 的五个取值', () => {
    expect(Object.keys(FUNDING_STATUS).sort()).toEqual([
      'failed',
      'released',
      'reserved',
      'reversed',
      'succeeded',
    ]);
  });

  it('ACTOR_TYPE 与订单域那张是同一份', () => {
    expect(Object.keys(ACTOR_TYPE).sort()).toEqual(['admin', 'merchant', 'system', 'user']);
  });

  it('覆盖 payment_provider_calls.operation 的九个取值', () => {
    // agreement_* 与 reconcile 四个今天没有代码在写（代扣与对账的表建好了、逻辑没做），
    // 但它们是最终形态的一部分，登记着才不会在真调起来那天显示英文码。
    expect(Object.keys(PROVIDER_OPERATION).sort()).toEqual([
      'agreement_charge',
      'agreement_sign',
      'agreement_terminate',
      'close',
      'create',
      'query',
      'query_refund',
      'reconcile',
      'refund',
    ]);
  });

  it('覆盖 payment_provider_calls.result 的四个取值', () => {
    expect(Object.keys(PROVIDER_RESULT).sort()).toEqual([
      'failed',
      'success',
      'timeout',
      'unknown',
    ]);
  });

  it('覆盖 payment_notifications.status 的四个取值', () => {
    // ignored（签名验过但与我们无关）与 failed（没能处理）分开：一个是正常吞掉，
    // 一个是真出事了，合并之后「回调处理有失败」这条告警就再也发不出来。
    expect(Object.keys(NOTIFICATION_STATUS).sort()).toEqual([
      'failed',
      'ignored',
      'processed',
      'received',
    ]);
  });

  it('覆盖 payment_transactions 的 kind 与 direction', () => {
    expect(Object.keys(TRANSACTION_KIND).sort()).toEqual(['payment', 'refund', 'reversal']);
    expect(Object.keys(TRANSACTION_DIRECTION).sort()).toEqual(['in', 'out']);
  });
});
