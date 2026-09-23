import { describe, expect, it } from 'vitest';
import type { CampaignStatus } from '../../../services/lottery';
import { allowedCampaignTransitions, CAMPAIGN_CREATE_CHOICES, campaignActionLabel } from './transitions';

/**
 * 这一组钉的是**界面上给不给这个入口**。
 *
 * 背景是这张表曾经少了整整两类动作：草稿活动没有任何入口变「进行中」，任何状态都没有
 * 入口变「已结束」。后端 `checkCampaignTransition` 一直是允许这两条的，页面只是没画
 * 按钮——于是「建了个活动先配着，回头再启用」这条运营路径走不通。
 *
 * 断言写成**对后端那张表逐条列举**，而不是「至少包含 enabled」：少给一个入口和多给一个
 * 入口都是缺陷，只有逐条列出来才能同时抓住两头。每条断言旁边都记着后端为什么这么定。
 */

const ALL: CampaignStatus[] = ['draft', 'enabled', 'paused', 'ended'];

describe('allowedCampaignTransitions', () => {
  it('草稿可以启用（第一次开张），也可以直接结束', () => {
    expect(allowedCampaignTransitions('draft')).toEqual(['enabled', 'ended']);
  });

  it('进行中可以暂停、结束，也可以改回草稿', () => {
    // 改回草稿是后端明确允许的：它只是不再开新期，不影响已经开出去的历史。
    expect(allowedCampaignTransitions('enabled')).toEqual(['draft', 'paused', 'ended']);
  });

  it('已暂停可以恢复，也可以结束', () => {
    expect(allowedCampaignTransitions('paused')).toEqual(['enabled', 'ended']);
  });

  it('已结束是终态：一个入口都不给', () => {
    // 后端 ErrCampaignEnded 会把任何迁移拒掉。给它留按钮就是留一个必然报错的动作。
    expect(allowedCampaignTransitions('ended')).toEqual([]);
  });

  it('任何状态都不把自己列为可选项', () => {
    // 后端允许 enabled → enabled（容忍按钮点两下），但下拉里列一个与当前相同的选项是无意义的。
    for (const from of ALL) {
      expect(allowedCampaignTransitions(from)).not.toContain(from);
    }
  });

  it('draft 不能直接去 paused', () => {
    // 后端刻意拦掉这条：一个从没开过期的活动被标成「已暂停」，读到的是一次从来没发生过的
    // 运营动作。不想要一个草稿就结束它。
    expect(allowedCampaignTransitions('draft')).not.toContain('paused');
  });
});

describe('CAMPAIGN_CREATE_CHOICES', () => {
  it('不含「已暂停」：后端 CreateCampaign 拒掉它', () => {
    expect(CAMPAIGN_CREATE_CHOICES).not.toContain('paused');
    expect(CAMPAIGN_CREATE_CHOICES).toEqual(['draft', 'enabled', 'ended']);
  });

  it('含「进行中」：新建时选它就直接开第一期（CreateCampaign 认请求里的 status）', () => {
    expect(CAMPAIGN_CREATE_CHOICES).toContain('enabled');
  });
});

describe('campaignActionLabel', () => {
  it('去 enabled：从草稿去叫「启用」，从已暂停去叫「恢复」', () => {
    expect(campaignActionLabel('draft', 'enabled')).toBe('启用');
    expect(campaignActionLabel('paused', 'enabled')).toBe('恢复');
  });

  it('其余目标状态按目标本身命名', () => {
    expect(campaignActionLabel('enabled', 'paused')).toBe('暂停');
    expect(campaignActionLabel('enabled', 'ended')).toBe('结束');
    expect(campaignActionLabel('enabled', 'draft')).toBe('改回草稿');
  });
});
