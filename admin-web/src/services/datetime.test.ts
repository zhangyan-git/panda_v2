import { describe, expect, it } from 'vitest';
import { formatDateTime, toRFC3339 } from './datetime';

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

/**
 * 设备卡片底部那行小字用它。要点是**补零**和**本地时区**：后端给的是带 Z 的 RFC3339，
 * 而管理员看的是自己手表上的时间。这两件事错了都不会抛异常，只会显示成
 * 「2026-9-3 4:0」或者差 8 小时，而差 8 小时在一眼扫过去时特别像「就是那时候」。
 */
describe('formatDateTime', () => {
  it('pads month, day, hour and minute', () => {
    // 造一个本地时间再转成 ISO，断言的是「读回来还是同一个本地时刻」，
    // 所以这条用例在任何时区下都成立。
    const local = new Date(2026, 8, 3, 4, 5);
    expect(formatDateTime(local.toISOString())).toBe('2026-09-03 04:05');
  });

  it('drops the seconds', () => {
    const local = new Date(2026, 8, 3, 4, 5, 59);
    expect(formatDateTime(local.toISOString())).toBe('2026-09-03 04:05');
  });

  it.each([undefined, null, ''])('returns an empty string for %j', (value) => {
    // 卡片里「没值就不摆这一项」，空串正好让它在 filter(Boolean) 里消失。
    expect(formatDateTime(value)).toBe('');
  });

  it('returns the raw value when it cannot be parsed', () => {
    // 宁可显示一个看不懂的原值，也不显示一个像模像样的错时间。
    expect(formatDateTime('不是时间')).toBe('不是时间');
  });
});
