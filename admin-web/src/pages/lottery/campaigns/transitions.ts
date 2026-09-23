import type { CampaignStatus } from '../../../services/lottery';

/**
 * 抽奖活动的状态迁移表。
 *
 * 这是后端 `checkCampaignTransition`（lottery-service/internal/service/campaign.go）的**镜像**。
 * 它只决定界面上给不给这个入口，真正的裁决永远在后端——两边万一漂了，结果只是前端多给或
 * 少给一个按钮，后端该拒的还是拒。
 *
 * 为什么要镜像，而不是「四个状态全列出来、让后端去拒」：这个仓库的规矩是别把注定 409 的
 * 按钮摆出来（期间页的「开奖」按钮就是这么收的）。运营点下去看到一句报错，不如下拉里
 * 根本没有这一项。
 *
 * 后端刻意拦掉的两条「回头」也照抄在这里：ended 之后没有任何迁移；draft 不能直接去 paused
 * ——一个从没开过期的活动标成「已暂停」，读到的是一次从来没发生过的运营动作。
 */
const CAMPAIGN_TRANSITIONS: Record<CampaignStatus, CampaignStatus[]> = {
  draft: ['enabled', 'ended'],
  enabled: ['draft', 'paused', 'ended'],
  paused: ['enabled', 'ended'],
  ended: [],
};

/**
 * 新建活动时，状态下拉里能选的值。
 *
 * **不含 `paused`**：后端 CreateCampaign 明确拒掉它（理由与上面 draft → paused 是同一个）。
 * 这一格原先把四个状态都列出来，选「已暂停」再提交回的是「当前状态不允许这样切换」——
 * 留一个注定被拒的选项，等于让人白填一遍表。要留着就先建成草稿。
 *
 * 与它相对的是编辑那一格：只列当前状态与 allowedCampaignTransitions 给的去向。
 */
export const CAMPAIGN_CREATE_CHOICES: CampaignStatus[] = ['draft', 'enabled', 'ended'];

/**
 * 从 from 出发允许去的状态。不含原状态自己——后端允许「enabled → enabled」，那是为了
 * 容忍按钮被点两下，不是给下拉用的一个选项。
 */
export function allowedCampaignTransitions(from: CampaignStatus): CampaignStatus[] {
  return CAMPAIGN_TRANSITIONS[from] ?? [];
}

/** 目标状态在按钮上怎么说。 */
const ACTION_LABEL: Record<CampaignStatus, string> = {
  draft: '改回草稿',
  enabled: '启用',
  paused: '暂停',
  ended: '结束',
};

/**
 * 行内按钮的文案。**按「从哪来」微调**：同样是去 enabled，从草稿去是「启用」（这个活动
 * 第一次开张），从已暂停去是「恢复」（接着跑）——这两件事在运营嘴里不是同一个词。
 */
export function campaignActionLabel(from: CampaignStatus, to: CampaignStatus): string {
  if (to === 'enabled' && from === 'paused') return '恢复';
  return ACTION_LABEL[to];
}
