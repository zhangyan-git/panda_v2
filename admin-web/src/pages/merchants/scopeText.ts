import type { MerchantUser } from '../../services/merchant';

/**
 * 数据范围的中文说法，账号列表那一列用的。
 *
 * 名称与 scopeIds 同序等长，查不到的那个目标是空串（范围目标被删掉了，或者那一次解析
 * 没查到）。空串**不参与拼接**：写成「品牌：A、、B」会在界面上看着像坏数据，而事实只是
 * 其中一个名字没解析出来。查不到名字的目标直接不显示。
 *
 * 商户档没有目标，那一档的名字（「商户全部」）是界面文案，服务端这一格刻意留空。
 *
 * **它不能就地写在 index.tsx 里**：那样只能靠渲染整个页面来测，而全仓的惯例是页面里的
 * 纯函数拆成同目录的 .ts + .test.ts（见 deviceCard、couponForm 等同名文件）。
 */
export type ScopeTextInput = Pick<MerchantUser, 'scopeNames'> & {
  /**
   * 刻意收成 string 而不是 MerchantUser 那个三值联合：这条函数的 default 分支
   * 处理的就是「服务端加了新档、这里还没跟上」。把入参钉成联合类型，那个分支
   * 就成了永远走不到、也测不到的代码——而它恰恰是唯一能挡住「装作没有范围」的一支。
   */
  scopeType: string;
};

export function scopeText(row: ScopeTextInput): string {
  switch (row.scopeType) {
    case 'merchant':
      return '商户全部';
    case 'brand':
    case 'store': {
      const level = row.scopeType === 'brand' ? '品牌' : '门店';
      const names = (row.scopeNames ?? []).map((name) => name?.trim()).filter(Boolean);
      return names.length ? `${level}：${names.join('、')}` : level;
    }
    default:
      // 档位本身没认出来（服务端加了新档而这里没跟上）：原样显示，别装作没有范围。
      return row.scopeType || '未知';
  }
}
