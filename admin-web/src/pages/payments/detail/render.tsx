import { formatDateTime } from '../../../services/datetime';
import { formatYuan } from '../../../services/money';

/**
 * 支付单详情六个页签共用的几个显示零件。
 *
 * 为什么单独一个文件：这六个页签都是「一张一次性给全的表」，用的是 antd 的 Table 而不是
 * ProTable，所以 ProTable 那套 valueType: 'dateTime' 在这里不可用，六处都得自己格式化。
 * 抄六份的代价不是多几行，是六处对「空值显示成什么」慢慢给出六种答案。同目录的
 * orders/detail/fortuneNotice.ts 是同一个定位（一个 tab 文件夹里的共用零件）。
 */

/**
 * 空值一律显示成「—」。
 *
 * 空串与 null 都算空：接口对「没发生过」给 null（closedAt），对「没有这个值」给空串
 * （failureCode，`payments.failure_code` 的列定义是 NOT NULL DEFAULT ''）。这两种在页面上都该是「没有」，
 * 但要与 0 分得开——0 是有效值（sortOrder、金额），照实显示。
 */
export const dash = (value?: string | number | null) =>
  value === null || value === undefined || value === '' ? '—' : String(value);

/** 分 → 「¥12.80」。0 显示成「¥0.00」而不是「—」：那是真金额。 */
export const money = (fen?: number | null) =>
  fen === null || fen === undefined ? '—' : `¥${formatYuan(fen)}`;

/** RFC3339 → 本地时间。antd Table 里没有 ProTable 的 valueType，所以每处都要自己调。 */
export const time = (value?: string | null) => (value ? formatDateTime(value) : '—');

/** 是不是「没有这个值」——给 jsx 里做条件渲染用（dash 返回的是字符串，判不了）。 */
const empty = (value: unknown) => value === null || value === undefined;

/**
 * 原样贴一段 JSON / 原始报文。
 *
 * 字符串按原文输出（回调通知的 body 就是一段文本，JSON.stringify 会给它套上引号并转义）；
 * 其余走 JSON.stringify 缩进。
 *
 * `rows` 是给行内容用的矮高度。**不换行截断、只在框内滚动**：这几块是排查时唯一的一手
 * 材料，裁掉一半等于逼人去查库。
 */
export function RawBlock({
  value,
  maxHeight = 160,
}: {
  value: unknown;
  maxHeight?: number;
}) {
  if (empty(value)) return <>{'—'}</>;
  const text = typeof value === 'string' ? value : JSON.stringify(value, null, 2);
  if (!text) return <>{'—'}</>;
  return (
    <pre
      style={{
        margin: 0,
        padding: 8,
        background: '#fafafa',
        borderRadius: 4,
        maxHeight,
        overflow: 'auto',
        fontSize: 12,
        whiteSpace: 'pre-wrap',
        wordBreak: 'break-all',
      }}
    >
      {text}
    </pre>
  );
}
