import { describe, expect, it } from 'vitest';
import { toRFC3339 } from './datetime';

/**
 * 这个函数是 ProTable 日期列 transform 的落点：dateTimeRange 交给它的是 dayjs 对象，
 * 后端要的是 RFC3339 字符串。转换出错的表现是「查询没报错但筛选没生效」，
 * 所以边界（空值、非法值）和 dayjs 对象必须各有一条用例钉住。
 */
describe('toRFC3339', () => {
  it('converts a dayjs-like value through toDate', () => {
    const pickerValue = { toDate: () => new Date('2026-09-13T12:00:00+08:00') };
    expect(toRFC3339(pickerValue)).toBe('2026-09-13T04:00:00.000Z');
  });

  it('converts a Date', () => {
    expect(toRFC3339(new Date('2026-09-13T04:00:00Z'))).toBe('2026-09-13T04:00:00.000Z');
  });

  it('converts a parseable string', () => {
    expect(toRFC3339('2026-09-13T12:00:00+08:00')).toBe('2026-09-13T04:00:00.000Z');
  });

  it.each([undefined, null, ''])('returns undefined for the empty value %j', (value) => {
    // 返回 undefined 而不是空串：查询参数里空串会被 toPageParams 当成「有筛选」发出去，
    // 后端收到 startTime= 只会 400。
    expect(toRFC3339(value)).toBeUndefined();
  });

  it('returns undefined for an unparseable value', () => {
    expect(toRFC3339('不是时间')).toBeUndefined();
  });

  it('returns undefined when the picker object yields an invalid date', () => {
    expect(toRFC3339({ toDate: () => new Date('nope') })).toBeUndefined();
  });
});
