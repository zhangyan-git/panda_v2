/**
 * 「分配权限」弹窗里每个分组是独立的 Checkbox.Group。
 * antd 的 Checkbox.Group 只把它自己注册过的值回传给 onChange
 * （见 antd/es/checkbox/Group.js 的 `newValue.filter(val => registeredValues.includes(val))`），
 * 因此回调结果只包含本组的权限 ID。直接把它当成全局已选会把其它分组的勾选整体清空。
 *
 * @param current 当前全局已选权限 ID
 * @param groupIds 本分组包含的权限 ID
 * @param selectedInGroup 本分组回调回来的已选权限 ID
 * @returns 合并后的全局已选权限 ID，其它分组的已选保持不变
 */
export function mergeGroupSelection(
  current: string[],
  groupIds: string[],
  selectedInGroup: string[],
): string[] {
  const next = new Set(current);
  const picked = new Set(selectedInGroup);
  groupIds.forEach((id) => {
    if (picked.has(id)) {
      next.add(id);
    } else {
      next.delete(id);
    }
  });
  return [...next];
}
