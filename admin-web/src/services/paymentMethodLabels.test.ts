import { describe, expect, it } from 'vitest';
import { PAYMENT_METHOD, paymentMethodLabel } from './paymentMethodLabels';

/**
 * 这一份盯的是**支付方式那一列的值域**。
 *
 * 与其它 labels 的测试不同，它不是「后端加了个枚举码这里要红」：支付方式的值不在任何一张
 * CHECK 约束里（词表已经退场，见 migrations/payment），合法取值只有 payment-service
 * catalog 里那几个常量。所以这里钉的是**这份表的键集**：
 *
 *   - 多出一个键 = 有人把已经退场的那套「出资渠道」字面量加了回来（wechat / unionpay /
 *     wallet / other）：回填之后库里不该再有它们，加回来只会让界面继续显示「其他」。
 *   - 少一个键 = 有人给 catalog 加了第七种支付方式而忘了登记，界面上会裸奔一个英文 code。
 *
 * 两条都属于「看一眼代码看不出来、上线之后才在界面上显形」的错。
 */
describe('支付方式文案表', () => {
  it('键集就是 catalog 的七条 code 加上订单侧两个设备标签', () => {
    // 前七条来自 payment-service/internal/catalog 的 Code* 常量；后两条是订单侧自己写的
    // 标签（设备单没有支付单，见 order-service repository/device_order.go）。
    //
    // wechat_papay 是第七个：catalog 里它不是一条收银通道（Create 会拒绝），但它确实会写进
    // orders.payment_method —— 会员续费代扣建的单就是它。那一列是这张表渲染的列。
    expect(Object.keys(PAYMENT_METHOD).sort()).toEqual([
      'card_pay',
      'coffee_bean',
      'pickup_code',
      'ums_h5_alipay',
      'ums_h5_upqr',
      'ums_h5_wechat',
      'ums_h5_wechat_minipay',
      'ums_miniapp_wechat',
      'wechat_papay',
    ]);
  });

  it('退场的那套出资渠道字面量一个都不在里面', () => {
    // 这几条从前在这张表里，value 就是老词表。回填把库里的值换成了 code，
    // 表里再留着它们只会让人以为那是今天的取值。
    for (const gone of ['wechat', 'unionpay', 'wallet', 'other', 'balance', 'cash', 'wechat_pay']) {
      expect(PAYMENT_METHOD[gone], gone).toBeUndefined();
    }
  });

  it('每条都有非空文案', () => {
    for (const [value, meta] of Object.entries(PAYMENT_METHOD)) {
      expect(meta.text, value).toBeTruthy();
    }
  });

  it('认识的值给中文', () => {
    // 支付宝这一档单独钉住：它当年在出资渠道词表里没有档位，只能落 other，后台把一笔
    // 支付宝单显示成「其他」——这就是整套词表被退掉的原因。
    expect(paymentMethodLabel('ums_h5_alipay')).toBe('支付宝（银联商务 H5）');
    expect(paymentMethodLabel('coffee_bean')).toBe('咖啡豆');
    // 设备单的两条也必须认得出，它们与支付方式共用这一列。
    expect(paymentMethodLabel('card_pay')).toBe('刷卡机');
    expect(paymentMethodLabel('pickup_code')).toBe('取货码（设备余额）');
    // 续费单那一列同理：它写的是代扣通道的 code。
    expect(paymentMethodLabel('wechat_papay')).toBe('微信代扣');
  });

  it('不认识的值原样回显', () => {
    // 与枚举表相反的地方：这张表的键集是抄来的、不是库里的约束，多出一个值只可能是别处
    // 写了个新 code。原样回显才查得到它是谁。
    expect(paymentMethodLabel('wechat_v3')).toBe('wechat_v3');
  });

  it.each([undefined, null, '', '   '])('空值给占位符 %j', (value) => {
    expect(paymentMethodLabel(value)).toBe('—');
  });

  it('两端空白不参与匹配', () => {
    // 自由字符串是别人写进来的，前后带空格是常态。
    expect(paymentMethodLabel('  coffee_bean  ')).toBe('咖啡豆');
  });
});
