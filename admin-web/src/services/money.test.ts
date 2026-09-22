import { describe, expect, it } from 'vitest';
import { fenToYuan, formatSignedYuan, formatYuan, yuanToFen } from './money';

/**
 * 换算本身只有三行，钉它是因为**错了不会报错，只会静默算错钱**：12.50 存成 12、
 * 或者一个负数流水少了符号，页面上都照样渲染。
 *
 * 下面第一组是 IEEE754 那几个真会咬人的数（0.1 + 0.2、1.15 * 100），不是凑数的例子。
 */
describe('money', () => {
  it('元转分：四舍五入而不是截断', () => {
    expect(yuanToFen(12.5)).toBe(1250);
    // 1.15 * 100 在 IEEE754 下是 114.99999999999999，截断会少收一分钱。
    expect(yuanToFen(1.15)).toBe(115);
    expect(yuanToFen(0.07)).toBe(7);
    expect(yuanToFen(0)).toBe(0);
    expect(yuanToFen(-3.5)).toBe(-350);
  });

  it('元转分：空值按 0 处理', () => {
    expect(yuanToFen(undefined)).toBe(0);
    expect(yuanToFen(null)).toBe(0);
  });

  it('分转元与展示', () => {
    expect(fenToYuan(1250)).toBe(12.5);
    expect(formatYuan(1250)).toBe('12.50');
    expect(formatYuan(0)).toBe('0.00');
    expect(formatYuan(-10)).toBe('-0.10');
    // 展示不拼 ¥：列标题里有「（元）」，拼了会变成「¥12.50 元」。
    expect(formatYuan(1250)).not.toContain('¥');
  });

  it('带符号展示：符号跟着数走', () => {
    expect(formatSignedYuan(10)).toBe('+¥0.10');
    expect(formatSignedYuan(-1234)).toBe('-¥12.34');
    // 扣减不能显示成又进了一笔：-¥12.34 的前缀必须是 '-'。
    expect(formatSignedYuan(-1).startsWith('-')).toBe(true);
  });
});
