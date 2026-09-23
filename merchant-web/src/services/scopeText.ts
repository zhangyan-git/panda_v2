import type { CurrentUser } from './user';

/**
 * 数据范围的中文说法。
 *
 * 商户档不显示名称：那一档的名字（「全部门店」）是界面文案，服务端刻意留空不替前端
 * 定（见 user-service 的 merchantMeResponse）。品牌档与门店档的名称是查询出来的，与
 * scopeIds 同序等长，查不到的那个目标（范围目标被删掉了）是空串——空串不参与拼接，
 * 否则会显示成「品牌「A、、B」」这种东西；一个名字都没有时退回档位本身，而不是显示
 * 一个空的引号。
 *
 * **它不能住在 `src/app.tsx` 里。** umi 把 app.tsx 的**每一个具名导出**都当成插件键注册
 * （生成物 `src/.umi/core/plugin.ts` 是 `import * as Plugin_0 from '../app.tsx'`），多一个
 * 不认识的键就在启动时抛 `register failed, invalid key`，整个应用白屏——而 tsc 与
 * `max build` 都是过的，只有真开一次页面才看得见。admin-web 的 app.ts 也只导出
 * request / layout / getInitialState 三样，同理。
 */
export function scopeText(user: CurrentUser): string {
  switch (user.scopeType) {
    case 'merchant':
      return '全部门店';
    case 'brand':
    case 'store': {
      const level = user.scopeType === 'brand' ? '品牌' : '门店';
      // 范围可以有好几个目标（后台勾了多个品牌或门店），展示时全写出来。
      const names = (user.scopeNames ?? [])
        .map((name) => name?.trim())
        .filter(Boolean)
        .join('、');
      return names ? `${level}「${names}」` : `按${level}`;
    }
    default:
      // 档位本身没认出来（比如服务端加了新档而这里没跟上）：原样显示，别装作没有范围。
      return user.scopeType || '未知';
  }
}
