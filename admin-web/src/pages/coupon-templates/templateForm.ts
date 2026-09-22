/**
 * 券模板表单值 ↔ 接口载荷之间的换算。
 *
 * 从 index.tsx 里搬出来只有一个理由：**这一层能测**。它管的是「哪些字段会被发出去」，
 * 而模板的 PUT 是**全量覆盖**——少发一个字段不是「不改它」，是**把它清空**。这类错误
 * 在界面上看不出来：保存成功、没有报错、列表里那一格从「是」变成「否」。只有断言载荷
 * 才抓得住，而断言载荷要求这些函数不在一个 900 行的页面文件里。
 *
 * 两条规矩，改这个文件时都要守住：
 *
 *  1. **凡是库里存着的、表单不编辑的字段，必须原样带回去**（见 toFormValues）。
 *     漏一个就是一次静默的清零，而模板没有 before-image。
 *  2. **凡是库里成对出现的字段，必须按模式显式清干净**（见 toPayload）。antd 表单
 *     卸载字段时默认 preserve，值还留在 store 里，切走模式之后旧值会照原样发出去，
 *     撞上库里那条 CHECK —— 表现是保存失败（500），而不是「填错了」。
 */
import type { CouponTemplate, TemplateInput } from '../../services/coupon';
import { toRFC3339 } from '../../services/datetime';

// 表单里金额用 InputNumber **按元**编辑（运营的习惯），提交时换成分。
export type TemplateFormValues = Omit<TemplateInput, 'faceValue' | 'minPurchaseAmount' | 'purchasePrice'> & {
  faceValue?: number;
  minPurchaseAmount?: number;
  purchasePrice?: number;
};

// 接口和库里金额一律是「分」的整数；页面按「元」录入和展示。换算只发生在这里，
// 别在别处再写一次 /100 —— 单位错位（12.50 存成 12）不会报错，只会静默算错钱。
//
// 用 Math.round 而不是直接截断：1.15 * 100 在 IEEE754 下是 114.99999999999999，
// 截断会悄悄少收一分钱。输入框另外用 precision={2} 限死两位小数。
export const yuanToFen = (yuan?: number) => Math.round(Number(yuan ?? 0) * 100);
export const fenToYuan = (fen?: number) => Number(fen ?? 0) / 100;

/**
 * 把接口的一行铺成表单值。
 *
 * 逐字段挑，不用展开：模板响应比表单多出 auditStatus/status/createdAt 等，展开会把
 * 它们混进提交载荷。**但反过来不成立**——表单要提交的每一个字段都必须在这里出现，
 * 否则它会被 PUT 清空。`TemplateInput` 上的字段与这里逐一对齐，两边一起改。
 */
export const toFormValues = (template: CouponTemplate): TemplateFormValues => ({
  couponTypeId: template.couponTypeId,
  merchantId: template.merchantId,
  name: template.name,
  shortTitle: template.shortTitle,
  description: template.description,
  coverImage: template.coverImage,
  useRuleDescription: template.useRuleDescription,
  totalQuantity: template.totalQuantity,
  validityMode: template.validityMode,
  validFrom: template.validFrom,
  validTo: template.validTo,
  validDays: template.validDays,
  claimLimitMode: template.claimLimitMode,
  // 这两个只在 claimLimitMode='periodic' 时有值（库里那两条 CHECK 保证互斥）。
  // 接口给的是一对可空列，表单里是 undefined，`?? undefined` 只是把 null 归一掉。
  //
  // 类型上要收窄一次：CouponTemplate 这一侧是 string（它是接口原样的形状），
  // 而提交侧的 TemplateInput 是枚举字面量。收窄不是「校验」，是照抄库里的约束——
  // 001 的 coupon_templates 上那两条 CHECK 已经把取值限死在这几个里，库里拿不到的
  // 值在这里也不该被造出来。真要做校验，位置在后端 validateTemplate。
  claimPeriodUnit: (template.claimPeriodUnit as TemplateFormValues['claimPeriodUnit']) ?? undefined,
  claimPeriodQuantity: template.claimPeriodQuantity ?? undefined,
  redemptionType: template.redemptionType,
  externalUseMethod: (template.externalUseMethod as TemplateFormValues['externalUseMethod']) ?? undefined,
  isHot: template.isHot,
  isRecommended: template.isRecommended,
  sortOrder: template.sortOrder,
  visible: template.visible,
  faceValue: fenToYuan(template.faceValue),
  minPurchaseAmount: fenToYuan(template.minPurchaseAmount),
  purchasePrice: fenToYuan(template.purchasePrice),
  // 适用范围必须原样带进表单：PUT 是全量覆盖，漏了这两个字段就是静默清空范围。
  // 接口保证是数组（空数组 = 该层不限），这里只是防御 null。
  brandIds: template.brandIds ?? [],
  storeIds: template.storeIds ?? [],
});

/**
 * 把表单值拼成提交载荷。创建与编辑走的是同一个函数（后端也是一条 validateTemplate）。
 */
export const toPayload = (values: TemplateFormValues): TemplateInput => {
  const payload: TemplateInput = {
    ...values,
    faceValue: yuanToFen(values.faceValue),
    minPurchaseAmount: yuanToFen(values.minPurchaseAmount),
    purchasePrice: yuanToFen(values.purchasePrice),
    claimLimitMode: values.claimLimitMode ?? 'once_ever',
    redemptionType: values.redemptionType ?? 'platform',
    // validFrom/validTo 是 Go 的 *time.Time，只认 RFC3339；表单的 dateFormatter 在这里
    // 不生效，所以提交前显式转一次（见 services/datetime.ts 里的说明）。
    validFrom: toRFC3339(values.validFrom),
    validTo: toRFC3339(values.validTo),
    // 多选框清空后给的是 undefined，而后端要的是「空数组 = 不限」。不归一的话
    // JSON.stringify 会把 undefined 的 key 整个丢掉，PUT 全量覆盖时旧范围仍在
    // 库里没被删——界面上看着清空了，实际没清掉。
    brandIds: values.brandIds ?? [],
    storeIds: values.storeIds ?? [],
    // 两个开关不填时必须是 false 而不是被丢掉：同一条理由，丢掉的 key 在 Go 那边
    // 解码成零值，看上去一样，但显式写出来才能让「这个字段是被编辑过的」这件事
    // 在载荷里看得见。
    isHot: values.isHot ?? false,
    isRecommended: values.isRecommended ?? false,
    sortOrder: values.sortOrder ?? 0,
  };

  // 两个模式各自的有效期字段在库里是同一行的 valid_from/valid_to/valid_days，
  // 而 PUT 是全量覆盖（templateUpdateQuery 的 SET 列表里有这三列）。antd 表单
  // 卸载字段时默认 preserve，值还留在 store 里，所以切换到另一个模式后旧值会
  // 照原样发出去 —— valid_to 一旦有值，发券处 COALESCE(valid_to, NOW()+days)
  // 就会压过 valid_days，管理员刚填的天数被静默吃掉。这里按模式显式清干净。
  if (payload.validityMode === 'fixed') {
    delete payload.validDays;
  } else {
    delete payload.validFrom;
    delete payload.validTo;
  }

  // 同一件事的第二组，而这一组连「静默吃掉」都不会：库里那条 CHECK 要求
  // periodic 时两个周期字段都非空、其余两档两个都为空（001 的 coupon_templates）。
  // 所以从 periodic 切到「仅一次」之后若还把旧值发出去，得到的是一个 500（约束
  // 失败），而不是一句「填错了」。反过来，选了 periodic 却没发这两个字段同样是
  // 500 —— 表单那边把它们标成必填正是为了不让人走到这里。
  if (payload.claimLimitMode === 'periodic') {
    payload.claimPeriodUnit = values.claimPeriodUnit;
    payload.claimPeriodQuantity = values.claimPeriodQuantity;
  } else {
    delete payload.claimPeriodUnit;
    delete payload.claimPeriodQuantity;
  }

  // 外部核销方式只对 redemptionType='external_code' 有意义，库里同样有一条 CHECK
  // （`redemption_type <> 'external_code' OR external_use_method IS NOT NULL`）。
  // 后台的核销方式下拉今天没有 external_code 这一档，所以这一支走不到；写在这里是
  // 为了它将来出现时不必再想一遍「切走之后旧值怎么办」。
  if (payload.redemptionType !== 'external_code') {
    delete payload.externalUseMethod;
  }

  return payload;
};
