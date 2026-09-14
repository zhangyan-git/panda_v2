// 省市区级联：值是编码路径（`['33','3301','330106']`），名字与编码由 region.ts 换算。
//
// 存编码而不是只存名字，是因为名字不是稳定的键——数据源一过期就会冒出「下城区」这种
// 已撤销的名字，而编码事后算不出来。换算成六个字段的那一步在 `regionFields()` 里，
// 页面在提交时调一次即可。
import { ProFormField } from '@ant-design/pro-components';
import { Cascader } from 'antd';
import type { CascaderProps, FormItemProps } from 'antd';
import type { ComponentProps, CSSProperties } from 'react';

import { REGION_DATA } from '../region';
import type { RegionNode } from '../region';

export type RegionCascaderProps = {
  value?: string[];
  onChange?: (value: string[] | undefined) => void;
  /** 默认就是完整区划数据；留出口子是为了测试和将来换数据源。 */
  options?: readonly RegionNode[];
  placeholder?: string;
  disabled?: boolean;
  style?: CSSProperties;
};

export function RegionCascader({
  value,
  onChange,
  options = REGION_DATA,
  placeholder = '请选择省 / 市 / 区',
  disabled,
  style = { width: '100%' },
}: RegionCascaderProps) {
  return (
    <Cascader
      allowClear
      // 只有选到区一级才算选完：半截路径存进库就是一个定位不到的区划。
      changeOnSelect={false}
      disabled={disabled}
      options={options as CascaderProps['options']}
      placeholder={placeholder}
      showSearch
      style={style}
      value={value}
      onChange={(next) => onChange?.(normalizePath(next))}
    />
  );
}

export type ProFormRegionCascaderProps = Omit<RegionCascaderProps, 'value' | 'onChange'> &
  Omit<ComponentProps<typeof ProFormField>, 'children' | 'value' | 'onChange' | 'name'> & {
    // 与 ProFormImageUpload 同样收紧 name：这个包装就是给表单用的。
    name: NonNullable<FormItemProps['name']>;
  };

/** 与 ProFormImageUpload 同理：用 ProFormField 才能拿到 grid 表单的 Col 包装。 */
export function ProFormRegionCascader({
  options,
  placeholder,
  disabled,
  style,
  ...itemProps
}: ProFormRegionCascaderProps) {
  return (
    <ProFormField {...itemProps}>
      <RegionCascader disabled={disabled} options={options} placeholder={placeholder} style={style} />
    </ProFormField>
  );
}

/** 清空（`[]` 或 `undefined`）统一收敛成 `undefined`，让 regionFields 走原值兜底。 */
function normalizePath(value: readonly (string | number | null)[] | undefined): string[] | undefined {
  if (!value || value.length === 0) return undefined;
  const path: string[] = [];
  for (const item of value) {
    // 路径里出现 null 就是这一级没选（antd 的类型允许它），与没选等义。
    if (item === null || item === undefined) return undefined;
    path.push(String(item));
  }
  return path;
}
