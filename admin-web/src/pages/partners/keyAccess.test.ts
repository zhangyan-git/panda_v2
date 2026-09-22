import { describe, expect, it } from 'vitest';
import { formatIPWhitelist, parseIPWhitelist } from './keyAccess';

/**
 * 这里钉的是**空输入的语义**：空 → 空数组 → 后端当作「不限制来源」。
 *
 * 它是最容易写错的一处：文本域里几个空行（粘贴留下的）如果被当成条目发上去，后端会回
 * 「白名单里有既不是 CIDR 也不是 IP 地址的条目」——一句看起来像填错了、实际只是多了个
 * 换行的话。反过来，把空格串过滤没了却留下一个 undefined，会让这一列在提交时变成 null，
 * 而后端收 null 与收空数组不是一件事（前者不是合法的 dto.APIKeyInput）。
 */
describe('IP 白名单的表单换算', () => {
  it('换行与逗号都当分隔符，两端空白丢掉', () => {
    expect(parseIPWhitelist('10.0.0.0/8\n 192.168.1.1 , 172.16.0.0/12')).toEqual([
      '10.0.0.0/8',
      '192.168.1.1',
      '172.16.0.0/12',
    ]);
  });

  it('空文本、空行、只有分隔符 —— 一律回空数组（= 不限制来源）', () => {
    expect(parseIPWhitelist('')).toEqual([]);
    expect(parseIPWhitelist('\n\n  \n')).toEqual([]);
    expect(parseIPWhitelist(', ,')).toEqual([]);
    expect(parseIPWhitelist(undefined)).toEqual([]);
    expect(parseIPWhitelist(null)).toEqual([]);
  });

  it('条目原样保留，不归一化也不去重', () => {
    // 后端存的是运营填的原串（「回显成归一化后的形状会让他以为填错了」），所以 10.0.0.1
    // 不会被补成 10.0.0.1/32，重复的条目也不会被悄悄合并——那两件事都会让页面上的东西
    // 与他填的不是同一份。
    expect(parseIPWhitelist('10.0.0.1\n192.168.1.0/24\n10.0.0.1')).toEqual([
      '10.0.0.1',
      '192.168.1.0/24',
      '10.0.0.1',
    ]);
  });

  it('回显：一行一条；空数组回空串（在表单里就是空文本域）', () => {
    expect(formatIPWhitelist(['10.0.0.0/8', '192.168.1.1'])).toBe('10.0.0.0/8\n192.168.1.1');
    expect(formatIPWhitelist([])).toBe('');
    expect(formatIPWhitelist(null)).toBe('');
    expect(formatIPWhitelist(undefined)).toBe('');
  });

  it('来回一趟是恒等的', () => {
    const list = ['10.0.0.0/8', '192.168.1.1', '172.16.0.0/12'];
    expect(parseIPWhitelist(formatIPWhitelist(list))).toEqual(list);
  });
});
