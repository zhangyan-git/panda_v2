import { describe, expect, it } from 'vitest';
import { scopeText } from './scopeText';
import type { CurrentUser } from './user';

const user = (scope: Partial<Pick<CurrentUser, 'scopeType' | 'scopeName'>>): CurrentUser => ({
  id: 'u',
  username: 'operator',
  name: '',
  email: '',
  merchantId: 'm',
  merchantName: '测试商户',
  scopeType: 'merchant',
  scopeId: '',
  scopeName: '',
  ...scope,
});

describe('merchant data scope wording', () => {
  it('names the three scope types', () => {
    // 商户档的名字是界面文案，服务端这一格刻意留空——所以空 scopeName 是正常的，不是坏数据。
    expect(scopeText(user({ scopeType: 'merchant' }))).toBe('全部门店');
    expect(scopeText(user({ scopeType: 'brand', scopeName: '北京一区' }))).toBe('品牌「北京一区」');
    expect(scopeText(user({ scopeType: 'store', scopeName: '国贸店' }))).toBe('门店「国贸店」');
  });

  it('falls back when the scope target is gone', () => {
    // 范围目标被删掉时服务端回空名；空引号（「」）比「按品牌」更难懂。
    expect(scopeText(user({ scopeType: 'brand', scopeName: '  ' }))).toBe('按品牌');
    expect(scopeText(user({ scopeType: 'store', scopeName: '' }))).toBe('按门店');
  });

  it('shows an unknown scope type as-is instead of pretending there is none', () => {
    expect(scopeText(user({ scopeType: 'region' }))).toBe('region');
    expect(scopeText(user({ scopeType: '' }))).toBe('未知');
  });
});
