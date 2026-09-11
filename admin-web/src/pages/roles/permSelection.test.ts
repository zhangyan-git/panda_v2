import { describe, expect, it } from 'vitest';
import { mergeGroupSelection } from './permSelection';

const brands = ['b-read', 'b-write', 'b-delete'];
const roles = ['r-read', 'r-write', 'r-delete'];
const all = [...brands, ...roles];

// 结果顺序不影响语义，断言前统一排序。
const merged = (current: string[], groupIds: string[], selectedInGroup: string[]) =>
  mergeGroupSelection(current, groupIds, selectedInGroup).sort();

describe('mergeGroupSelection', () => {
  it('keeps other groups selected when one item is unchecked', () => {
    expect(merged(all, brands, ['b-read', 'b-write'])).toEqual(
      ['r-read', 'r-write', 'r-delete', 'b-read', 'b-write'].sort(),
    );
  });

  it('keeps other groups selected when one item is checked', () => {
    expect(merged(['r-read'], brands, ['b-read'])).toEqual(['r-read', 'b-read'].sort());
  });

  it('ignores ids outside this group in the callback', () => {
    // antd 只回传本组注册过的值；即便回调里混入外部 ID，也不能把它们当成本组结果。
    expect(merged(['b-read', 'r-read'], brands, ['r-read'])).toEqual(['r-read']);
  });

  it('preserves bindings that the dialog cannot display', () => {
    // 角色残留的已删除权限 ID 不在 allPermIds 中，保存时不能被误删。
    const stale = 'ghost-perm';
    expect(merged(['r-read', stale], brands, ['b-read'])).toEqual(['r-read', stale, 'b-read'].sort());
  });

  it('clears the whole group without touching others', () => {
    expect(merged(all, brands, [])).toEqual([...roles].sort());
  });

  it('selects the whole group without touching others', () => {
    expect(merged(['r-read'], brands, brands)).toEqual(['r-read', ...brands].sort());
  });
});
