/**
 * 密钥「访问控制」那几格在**表单与接口之间**的换算。
 *
 * 只有 IP 白名单一样东西：接口收的是一个字符串数组，表单里那一格是一个文本域。放在这里
 * 而不是各自写在两个弹窗里（签发 / 修改），是因为这两条路共用同一份后端校验——分开写的
 * 那天会出现「签发时按行拆、修改时按逗号拆」，而白名单写坏的表现是这家合作方从下一个请求
 * 起**全部 401**（中间件解析失败即整条作废，失败关闭），排查起来是在对一串 IP 的格式。
 */

/**
 * 每分钟额度的默认值，与 partner-service 的 `model.DefaultRateLimitPerMinute` 同值。
 *
 * 它只在提示语里出现（「留空 = 60/分钟」）。**不在这里替用户填进表单**：那样提交上去的是
 * 一个看起来由人填的 60，而它其实是后端那条 DEFAULT——将来后端改默认值，所有老密钥会显示
 * 成曾经被人手填过的 60。
 */
export const DEFAULT_RATE_LIMIT_PER_MINUTE = 60;

/**
 * 文本域 → 接口要的数组。
 *
 * 换行与逗号都当分隔符：CIDR 与裸地址里都不可能含这两个字符，多认一种只是让从别处粘进来
 * 的「a, b」不用手工换行。
 *
 * **空输入回空数组，而空数组在后端的语义是「不限制来源」**（不是全拒）。所以清空这一格
 * 是一次有后果的操作，提示语要说清楚。
 *
 * 不做格式校验：解析在服务端（ingress.ParseIPAllowList），回的是「白名单里有既不是 CIDR
 * 也不是 IP 地址的条目」这句中文。在这里再实现一遍正则，只会得到第二套「什么算合法」。
 */
export function parseIPWhitelist(text?: string | null): string[] {
  return (text ?? '')
    .split(/[\n,]/)
    .map((entry) => entry.trim())
    .filter(Boolean);
}

/** 接口给的数组 → 文本域里一行一条。**不去重也不排序**：回显的必须是他自己填的那一份。 */
export function formatIPWhitelist(list?: string[] | null): string {
  return (list ?? []).join('\n');
}
