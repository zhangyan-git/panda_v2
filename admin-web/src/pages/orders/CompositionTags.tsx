import { Space, Tag, Tooltip } from 'antd';
import { enumMeta } from '../../services/labels';
import { orderComposition, ORDER_LINE_TYPE } from '../../services/orderLabels';
import type { OrderDetail, OrderSummary } from '../../services/order';

/**
 * 一单含哪几类行，渲染成一排标签。列表的「构成」列和详情的「订单构成」都用它。
 *
 * 两边共用而不是各写一遍：同一个东西在两处必须叫同一个名字——列表写「饮品 / 加购 / 会员套餐」，
 * 详情原先自己写成「含加购行 / 仅饮品」，同一张单在两处读起来像两种东西。
 *
 * 数据来源是接口算好的三个标记（`hasDrinkLine` 等），列表根本没取明细，所以两边用同一份入参形状。
 */
export default function CompositionTags({ row }: { row: OrderSummary | OrderDetail }) {
  const tags = orderComposition(row).map((lineType) => {
    const meta = enumMeta(ORDER_LINE_TYPE, lineType);
    const tag = <Tag color={meta.color}>{meta.text}</Tag>;
    // 「加购」这个词在界面上别处没出现过，而菜单叫「幸运杯套订单」：点明它俩是一回事，
    // 免得有人以为杯套和加购是两种东西。
    return lineType === 'addon' ? (
      <Tooltip key={lineType} title="加购 = 幸运杯套一类的活动商品">
        {tag}
      </Tooltip>
    ) : (
      <span key={lineType}>{tag}</span>
    );
  });
  // 三个标记都为假：历史数据里空单是可能的（迁移前的老单没有行）。留白分不清是「没有」
  // 还是「没取到」，所以给一个明确的字。
  if (tags.length === 0) return <span style={{ color: '#8c8c8c' }}>无订单行</span>;
  return <Space size={4}>{tags}</Space>;
}
