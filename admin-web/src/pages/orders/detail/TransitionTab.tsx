import { Table, Tag } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { formatDateTime } from '../../../services/datetime';
import type { OrderDetail, OrderTransition } from '../../../services/order';
import { ACTOR_TYPE, AGGREGATE_TYPE } from '../../../services/orderLabels';

/**
 * 订单详情 → 状态流水。
 *
 * 一屏回答「这一单经历了什么」：订单自己、它的行、出资行、售后单的状态变化都落在同一张流水
 * 表里（聚合类型那一列就是在区分它们），所以这一屏不只是订单的状态史——排查「退款卡在哪」
 * 时，售后那几条就在下面几行。
 *
 * 时间升序（老的在上）是后端给的顺序，也是读故事的顺序，这里不再排。
 */

const dash = (value?: string | null) => (value ? value : '—');
const time = (value?: string | null) => (value ? formatDateTime(value) : '—');

export default function TransitionTab({ order }: { order: OrderDetail }) {
  const columns: ColumnsType<OrderTransition> = [
    {
      title: '时间',
      dataIndex: 'createdAt',
      width: 170,
      render: (_, row) => time(row.createdAt),
    },
    {
      title: '聚合',
      dataIndex: 'aggregateType',
      width: 90,
      render: (_, row) => {
        const meta = AGGREGATE_TYPE[row.aggregateType] ?? {
          text: row.aggregateType,
          color: 'default',
        };
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 空串是「从无到有」（比如刚创建的第一条），显示成「—」而不是留白：留白看着像这一格
      // 没数据，而这里恰恰是在说「之前没有状态」。
      title: '原状态',
      dataIndex: 'fromStatus',
      width: 130,
      render: (_, row) => dash(row.fromStatus),
    },
    { title: '新状态', dataIndex: 'toStatus', width: 130 },
    {
      title: '原因',
      dataIndex: 'reason',
      width: 260,
      ellipsis: true,
      render: (_, row) => dash(row.reason),
    },
    {
      title: '操作者',
      dataIndex: 'actorType',
      width: 100,
      render: (_, row) => {
        const meta = ACTOR_TYPE[row.actorType] ?? { text: row.actorType, color: 'default' };
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 操作者 id 保留原文（用户 id / 管理员 id）：它是唯一能拿去别处查的线索。
      title: '操作者 ID',
      dataIndex: 'actorId',
      width: 200,
      ellipsis: true,
      render: (_, row) => dash(row.actorId),
    },
    {
      title: '聚合 ID',
      dataIndex: 'aggregateId',
      width: 200,
      ellipsis: true,
    },
  ];

  return (
    <Table<OrderTransition>
      rowKey="id"
      size="small"
      columns={columns}
      dataSource={order.transitions}
      pagination={false}
      scroll={{ x: 1280 }}
    />
  );
}
