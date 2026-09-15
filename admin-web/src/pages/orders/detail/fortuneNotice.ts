/**
 * 福卡页签那一句「为什么这里空着」的文案。
 *
 * 从 FortuneTab.tsx 拆出来，因为它是**纯逻辑**且带着一条容易写回去的错：三种状态三种说法，
 * 只有 paid 能叫人去点「标记完成」。理由与 pages/roles/permSelection.ts 相同——这类判断
 * 放在组件里就只能靠看，拆出来才测得住（见 fortuneNotice.test.ts）。
 */
import { enumMeta } from '../../../services/labels';
import { ORDER_STATUS } from '../../../services/orderLabels';
import type { OrderStatus } from '../../../services/order';

export interface PendingFortuneNotice {
  type: 'info' | 'warning';
  message: string;
  description: string;
}

/**
 * 说法按**这一单还走不走得到完成**分三种。
 *
 * 不能只写一句「完成之后刷新即可看到」：详情页那个「标记完成」按钮**只在 paid 上出现**
 * （见 index.tsx：后端状态机只认 paid → completed，别的状态点了必然回 409，所以按钮先被
 * 藏掉了）。对其它状态照着说，就是让客服去找一个不在那儿的按钮；已取消/已过期/已退款的单
 * 更是永远走不到完成，等多久都不会有流水，而这句话会让客服以为再等等就有了。
 *
 *   - paid：按钮就在右上角，告诉他按哪个、按完不用手动刷新。
 *   - pending_payment：还没收到钱，先别等福卡。
 *   - 其余（cancelled/expired/refunding/refunded）：不会发了，空着是对的。
 */
export function pendingFortuneNotice(status: OrderStatus): PendingFortuneNotice {
  const label = enumMeta(ORDER_STATUS, status).text;
  if (status === 'paid') {
    return {
      type: 'info',
      message: `订单尚未完成（当前状态：${label}），福卡未到账`,
      description: '福卡在订单标记完成时才发放。用右上角的「标记完成」把这一单完成，这一屏会自己刷新出流水，不用手动刷。',
    };
  }
  if (status === 'pending_payment') {
    return {
      type: 'info',
      message: `订单尚未支付（当前状态：${label}），福卡未到账`,
      description: '福卡在订单标记完成时才发放，而这一单还没收到钱。付款并完成之后这里才会有流水。',
    };
  }
  return {
    type: 'warning',
    message: `这一单不会发放福卡（当前状态：${label}）`,
    description: '福卡在订单标记完成时才发放，而这一单不会再走到完成。这里空着是对的，不是漏发了。',
  };
}
