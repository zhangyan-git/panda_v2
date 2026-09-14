/**
 * 后台列表接口的统一分页包裹，对应后端 platform/api.PageResponse。
 *
 * items 是当前页数据，total 是满足筛选条件的总记录数（不是本页条数）——
 * ProTable 的 total 必须给后者，给了本页条数分页器就只剩一页。
 * 字段名是 camelCase，与其余后台字段一致。
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
 * 「要全集」的调用点（Transfer 的候选、级联下拉）用的大页参数。
 *
 * 这些地方要的是整个集合，不是第一页：分页之后它们只拿到第 1 页，静默少数据
 * 且不报错。上限 200 与后端 api.MaxPageSize 对齐，且这些集合本身远小于 200
 * （权限 35、角色 2、菜单 17，品牌/门店各个位数）。
 *
 * ⚠️ 目标接口必须**真的**接受 200：被服务端拒掉时形态很隐蔽——调用方多半写成
 * `catch {}`（字典取不到就退回显示原始 id），于是没有报错，只是页面上安静地铺
 * 一串 uuid。2026-09 优惠券模块就是被自己那档更紧的 100 卡住的
 * （coupon-service `internal/dto/page.go` 现在与平台同值，并有测试钉住）。
 */
export const FULL_PAGE_PARAMS: PageQuery = { page: 1, pageSize: 200 };
