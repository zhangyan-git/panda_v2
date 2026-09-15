import { describe, expect, it } from 'vitest';
import type { OrderStatus } from '../../../services/order';
import { pendingFortuneNotice } from './fortuneNotice';

// 这一组守的是**客服照着这句话能不能做成事**：详情页那个「标记完成」按钮只在 paid 上出现
// （见 index.tsx），所以只有 paid 的说法里能出现「标记完成」。别处出现一次，客服就会去找
// 一个不在那儿的按钮——文案不会报错，只会让人白等，这正是它值得被一条用例钉住的原因。
describe('pendingFortuneNotice', () => {
  const canComplete: OrderStatus[] = ['paid'];
  const neverCompletes: OrderStatus[] = ['cancelled', 'expired', 'refunding', 'refunded'];
  const all: OrderStatus[] = ['pending_payment', ...canComplete, 'completed', ...neverCompletes];

  it('只有还点得到那个按钮的状态才把它指给客服', () => {
    // 断言的是**带引号的按钮名**，不是「标记完成」四个字：「福卡在订单标记完成时才发放」
    // 说的是规则、每种状态都该有，而「用右上角的『标记完成』」是把人指过去点。
    for (const status of all) {
      if (canComplete.includes(status)) continue;
      expect(
        pendingFortuneNotice(status).description,
        `状态 ${status} 不该把客服指去点一个不在那儿的按钮`,
      ).not.toContain('「标记完成」');
    }
    expect(pendingFortuneNotice('paid').description).toContain('「标记完成」');
  });

  it('不会再走到完成的那些状态要说清「不会发」，不是「还没发」', () => {
    for (const status of neverCompletes) {
      const { type, message, description } = pendingFortuneNotice(status);
      expect(type, `${status} 不该是一个平静的提示`).toBe('warning');
      expect(message).toContain('不会发放福卡');
      expect(description).toContain('不会再走到完成');
      // 「尚未完成」正是要避免的那句：它暗示再等等就有了。
      expect(message).not.toContain('尚未');
    }
  });

  it('待支付的单不能说成「不会再走到完成」', () => {
    // 它付款之后照样能完成，只是眼下还没轮到福卡。
    const { message, description } = pendingFortuneNotice('pending_payment');
    expect(message).toContain('尚未支付');
    expect(description).not.toContain('不会再走到完成');
  });

  it('三种说法都带上状态的中文名，而不是库里的英文码', () => {
    expect(pendingFortuneNotice('paid').message).toContain('已支付');
    expect(pendingFortuneNotice('pending_payment').message).toContain('待支付');
    expect(pendingFortuneNotice('cancelled').message).toContain('已取消');
    expect(pendingFortuneNotice('refunded').message).toContain('已退款');
  });
});
