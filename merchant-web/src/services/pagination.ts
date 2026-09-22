/**
 * 列表接口的统一分页包裹，对应后端 platform/api.PageResponse。
 *
 * items 是当前页数据，total 是满足筛选条件的总记录数（不是本页条数）——
 * ProTable 的 total 必须给后者，给了本页条数分页器就只剩一页。
 * 字段名是 camelCase，与其余接口字段一致。
 */
export type PageResult<T> = {
  items: T[];
  total: number;
  page: number;
  pageSize: number;
};

/** 列表接口的分页查询参数，page 从 1 开始，缺省时后端按第 1 页处理。 */
export type PageQuery = {
  page?: number;
  pageSize?: number;
};

/**
 * 把 ProTable 的分页参数转成接口认识的形状。
 *
 * Ant Design Pro 的表格用 current 表示页码，后端查询参数叫 page。名字对不上时
 * TypeScript 不报错、后端也只是「没收到 page」而回第 1 页——症状是翻到第 2 页
 * 拿回来的还是第 1 页的数据，看着像分页器坏了，所以要显式转一次。
 */
export function toPageParams<T extends { current?: number }>(
  params: T,
): Omit<T, 'current'> & { page: number } {
  const { current, ...rest } = params;
  return { ...rest, page: current ?? 1 };
}

/**
 * 「要全集」的调用点（把 id 换成名字的那种映射表）用的大页参数。
 *
 * 上限 200 与后端 api.MaxPageSize 对齐。这是**有损**的：数据超过 200 条时后面那些行会
 * 安静地映射不到，页面上就铺出一列 uuid，看起来像数据坏了。所以每一个用它拼映射表的地方
 * 都要**同时看 total**，把「没取全」这件事显式说出来（见设备列表页）。
 */
export const FULL_PAGE_PARAMS: PageQuery = { page: 1, pageSize: 200 };
