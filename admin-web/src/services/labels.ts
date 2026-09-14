/**
 * 枚举码 → 界面文案的共用零件。
 *
 * 每个域有一张自己的表（couponLabels / orderLabels），表放在域文件里，因为码是跟着
 * 那张表来的；但「怎么把码翻成文案」这件事每个域都一样，所以只有这一份实现。
 * 抄第二份的代价不是多几行代码，是两处对「取不到登记值时怎么办」慢慢给出两种答案。
 *
 * 与 services/money.ts、pagination.ts 同一个定位：跨域共用的口径，单独一个模块。
 */

export type EnumMeta = { text: string; color: string };

/**
 * 取不到登记值时退回原始码。
 *
 * 不返回空串：空白会让人以为这一格没配值，而实际上只是「库里出现了界面还不认识的
 * 新枚举」——那是迁移加了码、这里没跟上的信号，把码原样显示出来才查得到。
 */
export function enumMeta(map: Record<string, EnumMeta>, value?: string | null): EnumMeta {
  if (!value) return { text: '—', color: 'default' };
  return map[value] ?? { text: value, color: 'default' };
}

/** 给 ProTable 的搜索下拉用：valueEnum 只认 text。 */
export function searchOptions(map: Record<string, EnumMeta>): Record<string, { text: string }> {
  return Object.fromEntries(Object.entries(map).map(([value, meta]) => [value, { text: meta.text }]));
}
