import { describe, expect, it } from 'vitest';
import { scopeText } from './scopeText';

describe('数据范围列的中文说法', () => {
  it('商户档不看名称：那一格的名字是界面文案，服务端刻意留空', () => {
    expect(scopeText({ scopeType: 'merchant', scopeNames: [] })).toBe('商户全部');
  });

  it('品牌档与门店档把多个目标名连起来', () => {
    expect(scopeText({ scopeType: 'brand', scopeNames: ['北京一区', '北京二区'] })).toBe(
      '品牌：北京一区、北京二区',
    );
    expect(scopeText({ scopeType: 'store', scopeNames: ['国贸店'] })).toBe('门店：国贸店');
  });

  it('查不到名字的目标不占位、不留下顿号', () => {
    // scopeNames 与 scopeIds 同序等长，查不到的 id 占一个空串——它不该拼进这一格，
    // 否则界面上会出现「品牌：A、、B」这种像坏数据的东西。
    expect(scopeText({ scopeType: 'brand', scopeNames: ['北京一区', '', '北京二区'] })).toBe(
      '品牌：北京一区、北京二区',
    );
    expect(scopeText({ scopeType: 'store', scopeNames: ['  '] })).toBe('门店');
  });

  it('一个名字都没有时退回档位本身，而不是一个空的引号', () => {
    expect(scopeText({ scopeType: 'brand', scopeNames: [] })).toBe('品牌');
  });

  it('认不出的档位原样显示，不装作没有范围', () => {
    expect(scopeText({ scopeType: 'region', scopeNames: [] })).toBe('region');
    expect(scopeText({ scopeType: '', scopeNames: [] })).toBe('未知');
  });
});
