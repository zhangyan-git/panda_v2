import { describe, expect, it } from 'vitest';
import { scopeText } from './scopeText';
import type { CurrentUser } from './user';

const user = (
  scope: Partial<Pick<CurrentUser, 'scopeType' | 'scopeNames'>>,
): CurrentUser => ({
  id: 'u',
  username: 'operator',
  name: '',
  email: '',
  merchantId: 'm',
  merchantName: '测试商户',
  scopeType: 'merchant',
  scopeIds: [],
  scopeNames: [],
  ...scope,
});

describe('merchant data scope wording', () => {
  it('names the three scope types', () => {
    // 商户档的名字是界面文案，服务端这一格刻意留空——所以空 scopeNames 是正常的，不是坏数据。
    expect(scopeText(user({ scopeType: 'merchant' }))).toBe('全部门店');
    expect(scopeText(user({ scopeType: 'brand', scopeNames: ['北京一区'] }))).toBe(
      '品牌「北京一区」',
    );
    expect(scopeText(user({ scopeType: 'store', scopeNames: ['国贸店'] }))).toBe('门店「国贸店」');
  });

  it('lists every target when the scope holds more than one', () => {
    expect(scopeText(user({ scopeType: 'brand', scopeNames: ['北京一区', '北京二区'] }))).toBe(
      '品牌「北京一区、北京二区」',
    );
    expect(scopeText(user({ scopeType: 'store', scopeNames: ['国贸店', '望京店'] }))).toBe(
      '门店「国贸店、望京店」',
    );
  });

  it('leaves out a target whose name could not be resolved', () => {
    // 名称与 scopeIds 同序等长，查不到的那个占一个空串；它不该拼进这句话里，
    // 否则显示出来的是「品牌「A、、B」」——看着像坏数据，其实只是有一个名字没查到。
    expect(scopeText(user({ scopeType: 'brand', scopeNames: ['北京一区', '', '北京二区'] }))).toBe(
      '品牌「北京一区、北京二区」',
    );
  });

  it('falls back when the scope target is gone', () => {
    // 范围目标被删掉时服务端回空名；空引号（「」）比「按品牌」更难懂。
    expect(scopeText(user({ scopeType: 'brand', scopeNames: ['  '] }))).toBe('按品牌');
    expect(scopeText(user({ scopeType: 'store', scopeNames: [] }))).toBe('按门店');
  });

  it('shows an unknown scope type as-is instead of pretending there is none', () => {
    expect(scopeText(user({ scopeType: 'region' }))).toBe('region');
    expect(scopeText(user({ scopeType: '' }))).toBe('未知');
  });
});
