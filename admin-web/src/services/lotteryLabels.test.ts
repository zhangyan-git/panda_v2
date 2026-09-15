import { describe, expect, it } from 'vitest';
import {
  ACTIVATION_STATUS,
  CAMPAIGN_STATUS,
  DRAW_MODE,
  DRAW_TRIGGER,
  LOTTERY_ACTOR_TYPE,
  PARTICIPATION_FAILURE,
  PARTICIPATION_STATUS,
  PRIZE_KIND,
  ROUND_STATUS,
  WIN_EVENT_TYPE,
  WIN_STATUS,
  roundProgressLabel,
  winnerCountLabel,
} from './lotteryLabels';

/**
 * 与 orderLabels.test.ts / fortuneCardLabels.test.ts 同一个理由：钉的是**取值全集**
 * 而不是「有没有文案」。前者能在后端加码时失败，后者只在漏写文案时失败——而多出来的
 * 那个码才是常见的。
 *
 * 这些集合来自 lottery-service 的 model 常量，也就是 migrations/lottery/001_lottery_core.sql
 * 的 CHECK 约束。逐一对着那份迁移核过，所以**迁移里加了码而这里没跟，这一份会红**。
 */
describe('抽奖枚举文案表', () => {
  it('覆盖 lottery_activations.status 的两个取值', () => {
    expect(Object.keys(ACTIVATION_STATUS).sort()).toEqual(['disabled', 'enabled']);
  });

  it('覆盖 lottery_campaigns.status 的四个取值', () => {
    expect(Object.keys(CAMPAIGN_STATUS).sort()).toEqual(['draft', 'enabled', 'ended', 'paused']);
  });

  it('覆盖 lottery_rounds.status 的四个取值', () => {
    expect(Object.keys(ROUND_STATUS).sort()).toEqual(['cancelled', 'closed', 'drawn', 'open']);
  });

  it('覆盖 lottery_participations.status 的四个取值', () => {
    expect(Object.keys(PARTICIPATION_STATUS).sort()).toEqual([
      'confirmed',
      'failed',
      'pending',
      'reversed',
    ]);
  });

  it('覆盖 failure_code 的四个取值加空串', () => {
    // 空串是合法的默认值（没有失败），所以它必须登记：enumMeta 对没登记的码原样回显，
    // 而空串原样回显出来就是一格空白，与「这一列没数据」分不出来。
    expect(Object.keys(PARTICIPATION_FAILURE).sort()).toEqual([
      '',
      'insufficient_fortune_cards',
      'invalid_request',
      'round_closed',
      'round_not_open',
    ]);
  });

  it('覆盖 prize_kind 的四个取值', () => {
    expect(Object.keys(PRIZE_KIND).sort()).toEqual(['coffee', 'coupon', 'custom', 'physical']);
  });

  it('覆盖 lottery_wins.status 的六个取值', () => {
    // 本轮只有 pending 可达，其余五个是最终状态机的一部分——登记它们是为了下一轮加核销时
    // 不必回来发现这一格在显示英文码（见 lotteryLabels 里那段说明）。
    expect(Object.keys(WIN_STATUS).sort()).toEqual([
      'claimed',
      'expired',
      'pending',
      'redeemed',
      'revoked',
      'superseded',
    ]);
  });

  it('覆盖 win_events.event_type 的八个取值', () => {
    expect(Object.keys(WIN_EVENT_TYPE).sort()).toEqual([
      'claimed',
      'created',
      'expired',
      'redeemed',
      'revoked',
      'superseded',
      'swapped',
      'testimonial_updated',
    ]);
  });

  it('覆盖 actor_type 的四个取值', () => {
    expect(Object.keys(LOTTERY_ACTOR_TYPE).sort()).toEqual(['admin', 'merchant', 'system', 'user']);
  });

  it('覆盖 lottery_draws.mode 的两个取值', () => {
    expect(Object.keys(DRAW_MODE).sort()).toEqual(['auto', 'manual']);
  });

  it('覆盖 lottery_draws.trigger 的三个取值', () => {
    expect(Object.keys(DRAW_TRIGGER).sort()).toEqual(['deadline', 'manual', 'threshold']);
  });
});

describe('roundProgressLabel', () => {
  it('有门槛时显示「已参与 / 门槛」', () => {
    expect(roundProgressLabel(3, 10)).toBe('3 / 10');
  });

  it('门槛为 0 时只显示人数，不显示「3 / 0」', () => {
    // 门槛理论上不会是 0（CHECK participant_target > 0），但真出现时「3 / 0」看起来像
    // 进度爆了，比一个光秃秃的 3 更容易让人以为数据坏了。
    expect(roundProgressLabel(3, 0)).toBe('3');
  });
});

describe('winnerCountLabel', () => {
  it('没开奖时说名额', () => {
    expect(winnerCountLabel({ status: 'open', winnerCount: 5, actualWinnerCount: 0 })).toBe(
      '名额 5',
    );
  });

  it('开满了只报实发，不重复名额', () => {
    expect(winnerCountLabel({ status: 'drawn', winnerCount: 5, actualWinnerCount: 5 })).toBe(
      '实发 5',
    );
  });

  it('没开满时两个数都摆出来', () => {
    // 全员中奖（参与人数少于名额）是正常局面，不是异常：剩下的名额流掉。
    expect(winnerCountLabel({ status: 'drawn', winnerCount: 5, actualWinnerCount: 3 })).toBe(
      '实发 3 / 名额 5',
    );
  });
});
