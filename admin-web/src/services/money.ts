/**
 * 金额在「分」与「元」之间的换算。
 *
 * 接口和库里金额一律是「分」的整数，页面按「元」录入和展示。这个换算是全仓唯一的一份
 * —— 单位错位（12.50 存成 12）不会报错，只会静默算错钱，而同一个数在录入页和展示页
 * 上各算各的，两处迟早会漂移。
 *
 * 用 Math.round 而不是直接截断：1.15 * 100 在 IEEE754 下是 114.99999999999999，
 * 截断会悄悄少收一分钱。输入框另外用 precision={2} 限死两位小数。
 */

/** 元 → 分。空值按 0 处理，凑不出分的部分四舍五入。 */
export const yuanToFen = (yuan?: number | null) => Math.round(Number(yuan ?? 0) * 100);

/** 分 → 元。 */
export const fenToYuan = (fen?: number | null) => Number(fen ?? 0) / 100;

/** 展示用，固定两位小数。**不拼 ¥**：有的列本来就没有币种前缀，拼了就得多一个开关。 */
export const formatYuan = (fen?: number | null) => fenToYuan(fen).toFixed(2);

/**
 * 带符号的展示：`+¥12.34` / `-¥0.10`。
 *
 * 给**流水**那种有方向的金额列用（充值为正、扣减为负）。只显示绝对值会让一次扣减看起来
 * 像又进了一笔钱，所以符号必须跟着数走。颜色不在这里给——同一个数在有的页是绿色进账、
 * 在有的页是红色出账，那是各页自己的事。
 */
export const formatSignedYuan = (fen: number) =>
  `${fen < 0 ? '-' : '+'}¥${formatYuan(Math.abs(fen))}`;
