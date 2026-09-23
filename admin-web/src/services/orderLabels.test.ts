import { describe, expect, it } from 'vitest';
import {
  ACTOR_TYPE,
  AFTER_SALE_SCOPE,
  AFTER_SALE_STATUS,
  AGGREGATE_TYPE,
  FULFILLMENT_STATUS,
  ORDER_LINE_TYPE,
  orderComposition,
  ORDER_SOURCE,
  ORDER_STATUS,
  PAYMENT_LINE_STATUS,
} from './orderLabels';

/**
 * 这几张表是页面显示的全部依据，取值来自 order-service 的 model 常量（也就是各表
 * 迁移里的 CHECK 约束）。逐条钉住码→文案，是为了让「枚举加了一个值而页面没跟上」
 * 在测试里就能看出来，而不是等运营在界面上看见一个 pending_payment。
 *
 * 钉的是**取值全集**而不是「有没有文案」：前者能在后端加码时失败，后者只在漏写
 * 文案时失败——而漏写文案本来就是少数，多出来的那个码才是常见的。
 */
describe('枚举文案表', () => {
  it('覆盖 orders.status 的七个取值', () => {
    expect(Object.keys(ORDER_STATUS).sort()).toEqual([
      'cancelled', 'completed', 'expired', 'paid', 'pending_payment', 'refunded', 'refunding',
    ]);
  });

  it('覆盖 orders.fulfillment_status 的七个取值', () => {
    expect(Object.keys(FULFILLMENT_STATUS).sort()).toEqual([
      'cancelled', 'completed', 'failed', 'making', 'none', 'pending', 'ready',
    ]);
  });

  it('覆盖 orders.source 的四个取值', () => {
    // orders_source_check 今天有四个取值：device 是线下刷卡机设备回调建的单，
    // renewal 是会员续费代扣建的单。这里原先钉的是两个值
    // ——那份清单本身就是**不完整**的，测试把它钉住了，于是漏登记一直没被发现。
    //
    // 这条也顺带钉住 order.ts 的 OrderSource：那个联合类型与这张表要一一对应，多一个
    // 少一个都会让「筛选下拉里能选、类型上传不出去」这种错配活到线上。
    expect(Object.keys(ORDER_SOURCE).sort()).toEqual(['device', 'miniapp', 'renewal', 'screen_qr']);
  });

  it('覆盖 order_lines.line_type 的三个取值', () => {
    expect(Object.keys(ORDER_LINE_TYPE).sort()).toEqual(['addon', 'drink', 'membership']);
  });

  // order_payment_lines.line_type 不在这里：它存的是支付方式的 code，那套文案与支付域共用
  // 一份，见 paymentMethodLabels.test.ts。这里的「出资渠道」词表已经退场（migrations/order）。
  it('覆盖 order_payment_lines.status 的五个取值', () => {
    expect(Object.keys(PAYMENT_LINE_STATUS).sort()).toEqual([
      'failed', 'released', 'reserved', 'reversed', 'succeeded',
    ]);
  });

  it('覆盖 order_after_sales.status 的七个取值', () => {
    expect(Object.keys(AFTER_SALE_STATUS).sort()).toEqual([
      'approved', 'cancelled', 'failed', 'pending', 'refunded', 'refunding', 'rejected',
    ]);
  });

  it('覆盖 order_after_sales.scope 的四个取值', () => {
    expect(Object.keys(AFTER_SALE_SCOPE).sort()).toEqual(['addon', 'all', 'drink', 'membership']);
  });

  it('覆盖 order_state_transitions.actor_type 的四个取值', () => {
    expect(Object.keys(ACTOR_TYPE).sort()).toEqual(['admin', 'merchant', 'system', 'user']);
  });

  it('覆盖 order_state_transitions.aggregate_type 的四个取值', () => {
    expect(Object.keys(AGGREGATE_TYPE).sort()).toEqual([
      'after_sale', 'order', 'order_line', 'payment_line',
    ]);
  });

  it('每个取值都有非空文案', () => {
    for (const map of [
      ORDER_STATUS,
      FULFILLMENT_STATUS,
      ORDER_SOURCE,
      ORDER_LINE_TYPE,
      PAYMENT_LINE_STATUS,
      AFTER_SALE_STATUS,
      AFTER_SALE_SCOPE,
      ACTOR_TYPE,
      AGGREGATE_TYPE,
    ]) {
      for (const [value, meta] of Object.entries(map)) {
        expect(meta.text, value).toBeTruthy();
      }
    }
  });
});

describe('approved 的文案', () => {
  it('「已通过」而不是「已退款」', () => {
    // 审核通过是「同意退」，钱由 payment-service 退，**发起**与**退成**是后面两步。
    // 文案写成「已退款」会让审核人以为钱出去了，从而不去追下一步。
    expect(AFTER_SALE_STATUS.approved.text).toBe('已通过');
    expect(AFTER_SALE_STATUS.refunded.text).toBe('已退款');
  });
});

describe('orderComposition', () => {
  const flags = (drink: boolean, addon: boolean, membership: boolean) => ({
    hasDrinkLine: drink,
    hasAddonLine: addon,
    hasMembershipLine: membership,
  });

  it('顺序固定为饮品 / 加购 / 会员，不跟着入参的顺序走', () => {
    expect(orderComposition(flags(true, true, true))).toEqual(['drink', 'addon', 'membership']);
  });

  it('合并单三种都有——它就是同时在三个分类列表里出现的那种单', () => {
    // 这条是「后台按订单类型分类」的核心口径：列表之间不互斥，一张单可以同时在几个列表里。
    expect(orderComposition(flags(true, true, false))).toEqual(['drink', 'addon']);
  });

  it('纯会员单只有会员——它的履约状态是 none，没有要出杯的东西', () => {
    expect(orderComposition(flags(false, false, true))).toEqual(['membership']);
  });

  it('一种都没有时回空数组（不是 undefined）', () => {
    // 页面对空数组显示「—」；回 undefined 会让 .map 直接抛，而这是列表里的一格。
    expect(orderComposition(flags(false, false, false))).toEqual([]);
  });
});
