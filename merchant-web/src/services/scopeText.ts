import type { CurrentUser } from './user';

/**
 * 数据范围的中文说法。
 *
 * 商户档不显示 scopeName：那一档的名字（「全部门店」）是界面文案，服务端刻意留空不替前端
 * 定（见 user-service 的 merchantMeResponse）。品牌档与门店档的名称是查询出来的，可能为
 * 空（范围目标被删掉了）——那时退回显示档位本身，而不是显示一个空的引号。
 *
 * **它不能住在 `src/app.tsx` 里。** umi 把 app.tsx 的**每一个具名导出**都当成插件键注册
 * （生成物 `src/.umi/core/plugin.ts` 是 `import * as Plugin_0 from '../app.tsx'`），多一个
 * 不认识的键就在启动时抛 `register failed, invalid key`，整个应用白屏——而 tsc 与
 * `max build` 都是过的，只有真开一次页面才看得见。admin-web 的 app.ts 也只导出
 * request / layout / getInitialState 三样，同理。
 */
export function scopeText(user: CurrentUser): string {
  const name = user.scopeName?.trim();
  switch (user.scopeType) {
    case 'merchant':
      return '全部门店';
    case 'brand':
      return name ? `品牌「${name}」` : '按品牌';
    case 'store':
      return name ? `门店「${name}」` : '按门店';
    default:
      // 档位本身没认出来（比如服务端加了新档而这里没跟上）：原样显示，别装作没有范围。
      return user.scopeType || '未知';
  }
}
