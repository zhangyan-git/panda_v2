/**
 * 后端的时间字段是 Go 的 *time.Time，只认 RFC3339；表单控件的值不是。
 *
 * 一个值的形态取决于它是怎么来的：用户刚在日历里选的拿到的是 dayjs，从接口带回、没被改过
 * 的则是原样的字符串。所以在这里显式统一，不依赖库的隐式转换——那两者的格式本来就不一样，
 * 靠库去猜，猜错的表现是后端回 400 或者时间整体偏移几个小时，都不容易一眼看出来。
 *
 * 不 import dayjs：这里只用得到 toDate() 这一个方法，鸭子类型够了，而引一个类型包只为写个
 * 类型标注，会让「谁依赖 dayjs」这件事变得说不清。
 *
 * 字符串走 new Date() 解析。'YYYY-MM-DD HH:mm:ss' 这种不带时区的串按浏览器本地时区解释，
 * 再转成 UTC —— 正是想要的：用户选的是本地时间。
 */
export function toRFC3339(value?: unknown): string | undefined {
  if (!value) return undefined;
  const toDate = (value as { toDate?: unknown }).toDate;
  const date =
    typeof toDate === 'function'
      ? (toDate as () => Date).call(value)
      : new Date(String(value));
  return Number.isNaN(date.getTime()) ? undefined : date.toISOString();
}
