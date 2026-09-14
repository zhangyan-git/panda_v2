/**
 * 后端的时间字段是 Go 的 *time.Time，只认 RFC3339；表单控件的值不是。
 *
 * 一个值的形态取决于它是怎么来的：用户刚在日历里选的拿到的是 dayjs（ProForm 默认的
 * dateFormatter 在弹窗表单里实测不生效），从接口带回、没被改过的则是原样的字符串。
 * 所以在这里显式统一，不依赖库的隐式转换——那两者的格式本来就不一样，靠库去猜，
 * 猜错的表现是后端回 400 或者时间整体偏移几个小时，都不容易一眼看出来。
 *
 * 不 import dayjs：这里只用得到 toDate() 这一个方法，鸭子类型够了，而引一个类型
 * 包只为写个类型标注，会让「谁依赖 dayjs」这件事变得说不清。
 *
 * 字符串走 new Date() 解析。'YYYY-MM-DD HH:mm:ss' 这种不带时区的串按浏览器本地
 * 时区解释，再转成 UTC —— 正是想要的：用户选的是本地时间。
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

/**
 * RFC3339 → 'YYYY-MM-DD HH:mm'（浏览器本地时区），只给人看。
 *
 * 表格里交给 ProTable 的 valueType: 'dateTime' 就行，这是给**表格之外**的地方用的
 * （卡片底部那行小字）。不引 dayjs：这个仓库没直接依赖它，而为了格式化一个时间戳
 * 去 import 一个 antd 的传递依赖，装得上装不上全看提升结果。秒不显示——这几处看的
 * 都是「上次大概什么时候」，多两位只让那行小字更难扫。
 */
export function formatDateTime(value?: string | null): string {
  if (!value) return '';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(
    date.getHours(),
  )}:${pad(date.getMinutes())}`;
}
