// 省市区：数据源 + 「名字 ↔ 编码」两个方向上的纯函数。
//
// 数据来自 element-china-area-data（与 coffeevoucher_admin 同包同版本），刻意**不复用**
// 小程序自带的那份 pca-code.json —— 那份已经过期：杭州市底下至今挂着 2021 年就撤销的
// 「下城区」「江干区」。region.test.ts 里有一条新鲜度断言把这件事钉死，谁换数据源都会
// 立刻撞上。
//
// 数据只有 31 个省级，不含港澳台。
import { regionData } from 'element-china-area-data';
import type { DataItem } from 'element-china-area-data';

/** 省 → 市 → 区三层。实测 31 省 / 342 市 / 3056 区县，深度齐整。 */
export type RegionNode = DataItem;

export const REGION_DATA: readonly RegionNode[] = regionData;

/** 级联选择器的值：自上而下的编码链，如 `['33', '3301', '330106']`。 */
export type RegionPath = string[];

/**
 * 门店表上的六个区划字段：三个名字给人看，三个编码用来定位与迁移。
 *
 * 之所以名字和编码并存，见 `migrations/merchant/003_store_region_codes.sql` 的注释：
 * 名字不是稳定的键（数据源一过期就冒出「下城区」这种已撤销的名字），而编码是**事后
 * 算不出来的**，名字反倒随时能从编码推出来。
 */
export type RegionFields = {
  province: string;
  city: string;
  district: string;
  provinceCode: string;
  cityCode: string;
  districtCode: string;
};

const REGION_FIELD_KEYS = [
  'province',
  'city',
  'district',
  'provinceCode',
  'cityCode',
  'districtCode',
] as const satisfies readonly (keyof RegionFields)[];

export function emptyRegion(): RegionFields {
  return {
    province: '',
    city: '',
    district: '',
    provinceCode: '',
    cityCode: '',
    districtCode: '',
  };
}

/**
 * 名字三元组 → 编码路径。任何一层为空或查不到就返回 `undefined`。
 *
 * 返回 `undefined` 而不是「能查到几层算几层」，是因为调用方拿它当 `initialValues`：
 * 半截路径会让级联框显示成「浙江省 / 杭州市」而区一级空着，看着像已经选好了。
 */
export function namesToPath(
  names: readonly (string | null | undefined)[],
  data: readonly RegionNode[] = REGION_DATA,
): RegionPath | undefined {
  const path: string[] = [];
  let level: readonly RegionNode[] = data;
  for (const raw of names) {
    const name = (raw ?? '').trim();
    if (!name) return undefined;
    const node = level.find((item) => item.label === name);
    if (!node) return undefined;
    path.push(node.value);
    level = node.children ?? [];
  }
  return path.length > 0 ? path : undefined;
}

/**
 * 编码路径 → 节点链，逐层校验父子关系：`['33', '1101']` 这种拼错层级的路径查不到。
 *
 * 名字可以重名（全国有 28 组区县同名），只有「省/市/区」整个三元组能定位一个区划，
 * 所以这里按层往下找，而不是在三张平面表里各查一次。
 */
export function pathToNodes(
  path: readonly string[] | undefined | null,
  data: readonly RegionNode[] = REGION_DATA,
): RegionNode[] | undefined {
  if (!path || path.length === 0) return undefined;
  const nodes: RegionNode[] = [];
  let level: readonly RegionNode[] = data;
  for (const value of path) {
    const node: RegionNode | undefined = level.find((item) => item.value === value);
    if (!node) return undefined;
    nodes.push(node);
    level = node.children ?? [];
  }
  return nodes;
}

/** 编码路径 → 名字链。中间层不清空，缺哪层就少哪层。 */
export function pathToNames(
  path: readonly string[] | undefined | null,
  data: readonly RegionNode[] = REGION_DATA,
): string[] | undefined {
  return pathToNodes(path, data)?.map((node) => node.label);
}

/**
 * 级联选择器的值 → 六个区划字段，`original` 是这条记录当前库里的值。
 *
 * 兜底规则只有一条，但很关键：**路径解析不出来时原样保留 `original`**。库里存在
 * 「北京」（而不是「北京市」）这类自由文本，级联框解析不了；用户打开编辑、只改了
 * 门店名就保存，绝不能因为这一趟把老数据洗成空。
 */
export function regionFields(
  path: readonly string[] | undefined | null,
  original?: Partial<RegionFields> | null,
): RegionFields {
  const nodes = pathToNodes(path);
  if (!nodes) {
    return { ...emptyRegion(), ...pickStrings(original) };
  }
  const [province, city, district] = nodes;
  return {
    province: province.label,
    city: city?.label ?? '',
    district: district?.label ?? '',
    provinceCode: province.value,
    cityCode: city?.value ?? '',
    districtCode: district?.value ?? '',
  };
}

/** 只取字符串值：调用方传来的对象里可能有 `undefined`，展开它会把兜底值也覆盖掉。 */
function pickStrings(original?: Partial<RegionFields> | null): Partial<RegionFields> {
  const picked: Partial<RegionFields> = {};
  if (!original) return picked;
  for (const key of REGION_FIELD_KEYS) {
    const value = original[key];
    if (typeof value === 'string') picked[key] = value;
  }
  return picked;
}
