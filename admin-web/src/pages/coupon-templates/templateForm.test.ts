import { describe, expect, it } from 'vitest';
import type { CouponTemplate } from '../../services/coupon';
import { fenToYuan, toFormValues, toPayload, yuanToFen } from './templateForm';

/**
 * 这一组钉的是**编辑保存之后库里还剩什么**。
 *
 * 模板的更新是 PUT 全量覆盖（后端 templateUpdateQuery 的 SET 列表是写死的），所以
 * 「表单没渲染某个字段」与「把这个字段清空」在线上是同一件事：打开模板 → 点保存 →
 * 列表里那一格从「是」变成「否」、从文字变成「—」。界面上没有任何提示，接口回 200，
 * 而券模板没有 before-image，丢了就找不回来了。
 *
 * 所以这里的断言一律是对**提交载荷**的，不是对表单回显的：回显对了但没发出去，
 * 线上一样是丢。反过来说，凡是库里存着的字段，都要在载荷里原样出现一次。
 */

// 一份「每个字段都有值」的模板。用满值而不是最小集，是因为只有满值才能暴露
// 「字段被漏掉」——最小集下漏掉的字段与空值长得一样。
const template: CouponTemplate = {
  id: 'tpl-1',
  couponTypeId: 'ctype-1',
  merchantId: 'merchant-1',
  name: '满 50 减 10',
  shortTitle: '满减券',
  description: '全场通用',
  coverImage: 'https://cdn.example.com/cover.png',
  useRuleDescription: '每单限用一张',
  faceValue: 1000,
  minPurchaseAmount: 5000,
  purchasePrice: 0,
  totalQuantity: 200,
  issuedQuantity: 3,
  reservedQuantity: 1,
  validityMode: 'fixed',
  validFrom: '2026-01-01T00:00:00Z',
  validTo: '2026-12-31T15:59:59Z',
  validDays: undefined,
  claimLimitMode: 'periodic',
  claimPeriodUnit: 'month',
  claimPeriodQuantity: 2,
  // 库里这一档与 externalUseMethod 是一对（CHECK: redemption_type <> 'external_code'
  // OR external_use_method IS NOT NULL），所以外采券那条路径单独测。
  redemptionType: 'platform',
  externalUseMethod: undefined,
  auditStatus: 'approved',
  auditRemark: '',
  status: 'active',
  isHot: true,
  isRecommended: true,
  sortOrder: 7,
  visible: true,
  brandIds: ['brand-1'],
  storeIds: ['store-1', 'store-2'],
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-02T00:00:00Z',
};

describe('券模板表单 ↔ 载荷', () => {
  it('打开再原样保存：库里有的字段一个不少地发回去', () => {
    const payload = toPayload(toFormValues(template));

    // 这一批是这次修的：以前表单不渲染、TemplateInput 也不登记，保存即清零。
    expect(payload.coverImage).toBe('https://cdn.example.com/cover.png');
    expect(payload.useRuleDescription).toBe('每单限用一张');
    expect(payload.isHot).toBe(true);
    expect(payload.isRecommended).toBe(true);
    expect(payload.sortOrder).toBe(7);
    expect(payload.claimPeriodUnit).toBe('month');
    expect(payload.claimPeriodQuantity).toBe(2);

    // 这一批本来就在，一起钉住——它们中的任何一个被漏掉是同一类事故。
    expect(payload.description).toBe('全场通用');
    expect(payload.shortTitle).toBe('满减券');
    expect(payload.visible).toBe(true);
    expect(payload.brandIds).toEqual(['brand-1']);
    expect(payload.storeIds).toEqual(['store-1', 'store-2']);
    expect(payload.merchantId).toBe('merchant-1');
    expect(payload.claimLimitMode).toBe('periodic');
    expect(payload.redemptionType).toBe('platform');
  });

  it('金额来回换算：分 ↔ 元只在这里发生', () => {
    const values = toFormValues(template);
    expect(values.faceValue).toBe(10);
    expect(values.minPurchaseAmount).toBe(50);

    const payload = toPayload(values);
    expect(payload.faceValue).toBe(1000);
    expect(payload.minPurchaseAmount).toBe(5000);
    // 1.15 在 IEEE754 下是 114.99999999999999，截断会少收一分钱。
    expect(yuanToFen(1.15)).toBe(115);
    expect(fenToYuan(115)).toBe(1.15);
  });

  it('「按周期」切走时旧值必须删掉，而不是留在表单里发出去', () => {
    // antd 表单卸载字段时默认 preserve：切到「仅一次」之后 claimPeriodUnit 还在
    // store 里。库里那条 CHECK 要求非 periodic 时两列都为空，所以照原样发出去
    // 不会「静默算错」，而是直接 500 —— 用户看到的是一句讲不清的约束失败。
    const values = {
      ...toFormValues(template),
      claimLimitMode: 'once_ever' as const,
    };
    const payload = toPayload(values);

    expect(payload.claimPeriodUnit).toBeUndefined();
    expect(payload.claimPeriodQuantity).toBeUndefined();
    expect('claimPeriodUnit' in payload).toBe(false);
    expect('claimPeriodQuantity' in payload).toBe(false);
  });

  it('「按周期」时两个字段必须一起发（少一个同样是 500）', () => {
    const payload = toPayload(toFormValues(template));
    expect(payload.claimLimitMode).toBe('periodic');
    expect(payload.claimPeriodUnit).toBe('month');
    expect(payload.claimPeriodQuantity).toBe(2);
  });

  it('有效期两档互斥：切到「领取后天数」不再发 valid_to', () => {
    // valid_to 一旦有值，发券处 COALESCE(valid_to, NOW()+days) 会压过 valid_days，
    // 管理员刚填的天数被静默吃掉——这条比上面那条更坏，它不报错。
    const payload = toPayload({
      ...toFormValues(template),
      validityMode: 'relative',
      validDays: 30,
    });
    expect(payload.validDays).toBe(30);
    expect('validFrom' in payload).toBe(false);
    expect('validTo' in payload).toBe(false);

    const fixed = toPayload({ ...toFormValues(template), validityMode: 'fixed' });
    // 只比时刻，不比字面：toRFC3339 会归一成 .000Z，后端解析出来是同一个瞬间。
    expect(new Date(fixed.validFrom!).toISOString()).toBe('2026-01-01T00:00:00.000Z');
    expect(new Date(fixed.validTo!).toISOString()).toBe('2026-12-31T15:59:59.000Z');
    expect('validDays' in fixed).toBe(false);
  });

  it('外部核销方式只跟着 external_code 走', () => {
    // 后台今天没有 external_code 这一档，所以这一支走不到；写在这儿是为了它将来
    // 出现时不必再想一遍「切走之后旧值怎么办」——那条 CHECK 是一样的。
    const platform = toPayload({
      ...toFormValues(template),
      externalUseMethod: 'download_qr',
    });
    expect('externalUseMethod' in platform).toBe(false);

    const external = toPayload({
      ...toFormValues(template),
      redemptionType: 'external_code',
      externalUseMethod: 'download_qr',
    });
    expect(external.externalUseMethod).toBe('download_qr');
  });

  it('范围清空后是空数组而不是 undefined', () => {
    // JSON.stringify 会把 undefined 的 key 整个丢掉，PUT 全量覆盖时旧范围还在库里
    // 没被删——界面上看着清空了，实际没清掉。
    const payload = toPayload({ ...toFormValues(template), brandIds: undefined, storeIds: [] });
    expect(payload.brandIds).toEqual([]);
    expect(payload.storeIds).toEqual([]);
    expect(JSON.parse(JSON.stringify(payload)).brandIds).toEqual([]);
  });

  it('新建时的空表单：两个开关落到 false、排序落到 0，而不是被丢掉', () => {
    const payload = toPayload({
      couponTypeId: 'ctype-1',
      name: '新券',
      faceValue: 5,
      totalQuantity: 10,
      validityMode: 'relative',
      validDays: 7,
      claimLimitMode: 'once_ever',
      redemptionType: 'platform',
    });
    expect(payload.isHot).toBe(false);
    expect(payload.isRecommended).toBe(false);
    expect(payload.sortOrder).toBe(0);
    expect(payload.minPurchaseAmount).toBe(0);
    expect(payload.purchasePrice).toBe(0);
  });

  it('库里的两列是 null 而不是缺字段时，表单回显归一成 undefined', () => {
    const payload = toPayload(
      toFormValues({ ...template, claimLimitMode: 'once_ever', claimPeriodUnit: null as unknown as string, claimPeriodQuantity: null as unknown as number }),
    );
    expect('claimPeriodUnit' in payload).toBe(false);
    expect('claimPeriodQuantity' in payload).toBe(false);
  });
});
