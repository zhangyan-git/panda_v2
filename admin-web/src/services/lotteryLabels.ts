/**
 * 抽奖域那些枚举码的中文文案。
 *
 * 取值来自 lottery-service model 里的常量（也就是 migrations/lottery 的 CHECK 约束），
 * 不是接口给的——接口回的就是库里那些英文码。所以**改枚举必须同时改迁移和这里**，加了
 * 新码而没登记，界面上就退回显示原始码。
 *
 * 集中放一个文件，理由与 orderLabels / couponLabels 相同：同一个码会出现在好几个页面
 * （期次状态在期次列表、活动详情、中奖记录里都有），分开写一定漂移，而漂移的表现是同一
 * 期在两处显示成两种状态——不报错，只让人怀疑数据本身。
 *
 * 表在这里，翻码的那两个函数在 services/labels.ts。
 */

import type { EnumMeta } from './labels';

/** lottery_activations.status */
export const ACTIVATION_STATUS: Record<string, EnumMeta> = {
  enabled: { text: '已开通', color: 'success' },
  // 「已停用」而不是「未开通」：开通是一个动作，停用之后这条记录还在，默认活动与已开奖
  // 的期次也都还在。说成「未开通」会让人以为历史被清了。
  disabled: { text: '已停用', color: 'default' },
};

/**
 * lottery_campaigns.status。
 *
 * draft 与 paused 分开：draft 是「还没开始过」，paused 是「开着开着停了」。两者的区别在于
 * 前者还没有任何期次、后者有——把它们并成「未启用」会让运营看不出这个活动是不是已经发过奖。
 */
export const CAMPAIGN_STATUS: Record<string, EnumMeta> = {
  draft: { text: '草稿', color: 'default' },
  enabled: { text: '进行中', color: 'success' },
  paused: { text: '已暂停', color: 'warning' },
  ended: { text: '已结束', color: 'default' },
};

/**
 * lottery_rounds.status。
 *
 * open 与 closed 是**同一期的两个阶段**，不是两种期次：closed 表示参与**次数**已收满门槛、
 * 停止收人，但还没开奖。所以它们都不算终态，界面上要区分开——一个 closed 的期次等着开奖，
 * 一个 open 的期次还差次数，运营要做的动作不一样（等，或者人工开奖/作废）。
 *
 * closed 是**开奖前**的最后一个状态，worker 扫到它就会开掉，所以它通常只存在一瞬间。一个
 * open 的期次**不会有任何东西来动它**——没满就一直开着。
 */
export const ROUND_STATUS: Record<string, EnumMeta> = {
  open: { text: '进行中', color: 'processing' },
  closed: { text: '已满员待开奖', color: 'gold' },
  drawn: { text: '已开奖', color: 'success' },
  cancelled: { text: '已作废', color: 'default' },
};

/**
 * lottery_participations.status。
 *
 * **reversed 不并进 failed**：failed 是「没扣成」（余额不足、期次关了），reversed 是
 * 「扣了又原路退回」。对用户的钱是同一结果，但过程不同——把它显示成「失败」会让一次
 * 补偿路径看不见，而那正是最需要被发现的一类异常。
 *
 * pending 不是「处理中」那么轻松：它意味着**我们不知道扣没扣**（跨服务调用超时），
 * 由修复 worker 用同一个 request_id 重跑去收尾。文案要让人知道这一条需要盯着。
 */
export const PARTICIPATION_STATUS: Record<string, EnumMeta> = {
  pending: { text: '待确认', color: 'gold' },
  confirmed: { text: '已参与', color: 'success' },
  failed: { text: '未参与', color: 'error' },
  reversed: { text: '已退卡', color: 'warning' },
};

/**
 * participations.failure_code：没参与成的原因。
 *
 * 空串是合法的（没有失败），登记成「—」而不是漏掉——enumMeta 对没登记的码会原样回显，
 * 而空串原样回显出来就是一格空白，与「这一列没数据」分不出来。
 */
export const PARTICIPATION_FAILURE: Record<string, EnumMeta> = {
  '': { text: '—', color: 'default' },
  insufficient_fortune_cards: { text: '福卡不足', color: 'orange' },
  round_closed: { text: '期次已关闭', color: 'default' },
  // 参数被账户域判非法：这是**我们自己的 bug**，不是用户的错。文案里不写「参数错误」
  // 那样的话——运维看到它应当去翻日志，而不是去问用户填错了什么。
  invalid_request: { text: '扣卡参数异常（系统问题）', color: 'red' },
  round_not_open: { text: '期次未开放', color: 'default' },
};

/**
 * lottery_wins.status。
 *
 * **本轮只有 pending 会出现**（核销整块延后，见 plans 与 service 包的文件头）。其余五个
 * 取值照登是因为迁移里的 CHECK 已经把它们写全了——界面上登记一个今天到不了的码，好过
 * 下一轮加核销时回来发现这一格原样显示 `redeemed`。
 */
export const WIN_STATUS: Record<string, EnumMeta> = {
  pending: { text: '待领取', color: 'gold' },
  claimed: { text: '已领取', color: 'processing' },
  redeemed: { text: '已核销', color: 'success' },
  expired: { text: '已过期', color: 'default' },
  revoked: { text: '已撤销', color: 'error' },
  superseded: { text: '已重抽', color: 'warning' },
};

/**
 * lottery_win_events.event_type。本轮只写得到 created。
 *
 * created 的文案是「开奖产生」而不是「已创建」：它是**系统在开奖那一刻**写下的，不是有人
 * 建了一条记录。后台看这条流水的人问的是「这条奖是怎么来的」，答案就是这句话。
 */
export const WIN_EVENT_TYPE: Record<string, EnumMeta> = {
  created: { text: '开奖产生', color: 'blue' },
  claimed: { text: '领取', color: 'processing' },
  testimonial_updated: { text: '更新感言', color: 'cyan' },
  redeemed: { text: '核销', color: 'success' },
  swapped: { text: '换奖', color: 'purple' },
  superseded: { text: '被重抽取代', color: 'warning' },
  revoked: { text: '撤销', color: 'error' },
  expired: { text: '过期', color: 'default' },
};

/** win_events.actor_type：这次变动是谁做的 */
export const LOTTERY_ACTOR_TYPE: Record<string, EnumMeta> = {
  user: { text: '用户', color: 'blue' },
  merchant: { text: '商户', color: 'geekblue' },
  admin: { text: '管理员', color: 'purple' },
  // 开奖那一条的 actor 就是 system——没有人「做了」这件事，是收满门槛触发的。
  system: { text: '系统', color: 'default' },
};

/** lottery_draws.mode：这次开奖是谁发起的 */
export const DRAW_MODE: Record<string, EnumMeta> = {
  auto: { text: '自动开奖', color: 'cyan' },
  manual: { text: '人工开奖', color: 'purple' },
};

/**
 * lottery_draws.trigger：因为什么开的。
 *
 * 只有两条路，都是**人推着走的**：threshold 是参与**次数**收满了门槛（在确认那一步把期次
 * 置 closed，worker 随后开掉），manual 是管理员直接开的。这里曾经还有一档 deadline
 * （时间先到了，由 worker 扫出来），2026-09-15 随期次窗口一起删了——一个没满的期次会一直
 * 开着等，不会被时间挑走。
 *
 * 「这一期为什么开了」的答案就在这一列。
 */
export const DRAW_TRIGGER: Record<string, EnumMeta> = {
  threshold: { text: '次数达标', color: 'blue' },
  manual: { text: '人工', color: 'purple' },
};

// 下面这两个不是枚举表的用法，单独说明。

/**
 * 期次进度的显示文案：「3 / 10 次」。
 *
 * 分母是门槛，**数的是参与次数**，不是人数——同一个人可以在同一期里参与多次，每参与一次
 * 就 +1。
 *
 * 已开奖的期次显示**实际中奖人数**而不是名额：名额是「打算发几个」，实发可能更少
 * （参与的人不够时全员中奖）。把名额当结果显示，会让人以为没开奖成功。
 *
 * 放在这里而不是页面里：活动列表、活动详情、期次列表、开通列表四处都显示这个数，
 * 分开写一定漂移。
 */
export function roundProgressLabel(participantCount: number, participantTarget: number): string {
  if (!participantTarget) return String(participantCount);
  return `${participantCount} / ${participantTarget}`;
}

/**
 * 中奖名额的显示文案。
 *
 * 三个数在界面上**必须分得清**，它们最容易混：
 *   winnerCount      —— 名额（开期时按奖池冻结下来的；当前恒为 1，但冻结值本身还是从
 *                       奖池求和来的，不是写死的 1——将来要改成发多份时入手点在那里）；
 *   actualWinnerCount—— 实际开出几个（够了就是名额，不够就是参与人数）；
 *   total            —— 这个活动一共开出过多少条中奖记录。
 * 这个函数只负责前两个：没开奖时说「名额 N」，开奖后说「实发 N / 名额 M」。
 */
export function winnerCountLabel(row: {
  status: string;
  winnerCount: number;
  actualWinnerCount: number;
}): string {
  if (row.status !== 'drawn') return `名额 ${row.winnerCount}`;
  // 全员中奖（实发 < 名额）是正常局面，不是异常：参与的人不够，剩下的名额流掉。
  // 所以这里只是如实并列，不加任何「不足」之类的告警语气。
  return row.actualWinnerCount === row.winnerCount
    ? `实发 ${row.actualWinnerCount}`
    : `实发 ${row.actualWinnerCount} / 名额 ${row.winnerCount}`;
}
