import { ProDescriptions } from '@ant-design/pro-components';
import type { ProDescriptionsItemProps } from '@ant-design/pro-components';
import { Tag, Typography } from 'antd';
import { formatYuan } from '../../../services/money';
import type { OrderDetail } from '../../../services/order';
import { FULFILLMENT_STATUS, ORDER_SOURCE, ORDER_STATUS } from '../../../services/orderLabels';
import { paymentMethodLabel } from '../../../services/paymentMethodLabels';
import CompositionTags from '../CompositionTags';

/**
 * 订单详情 → 基本信息。列的是**订单主表上真有的**列。
 *
 * 金额一律拼 ¥：这里是逐项摊开看的（原价、优惠、应付、实付、已退），一串光秃秃的数字会
 * 让人不确定哪个是钱、哪个是张数——列表那边不拼，因为那一列的表头已经写着「实付」。
 */

const money = (fen?: number | null) => `¥${formatYuan(fen)}`;

/** 空值的显示口径与列表一致。 */
const dash = (value?: string | null) => (value ? value : '—');

export default function BasicTab({ order }: { order: OrderDetail }) {
  const columns: ProDescriptionsItemProps<OrderDetail>[] = [
    { title: '订单号', dataIndex: 'orderNo', copyable: true },
    {
      title: '订单状态',
      dataIndex: 'status',
      render: (_, row) => {
        const meta = ORDER_STATUS[row.status] ?? { text: row.status, color: 'default' };
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 与订单状态分开摆：钱收了而机器没出货（paid + failed）这个组合最需要被看见，
      // 合成一个状态就把它藏掉了。
      title: '履约状态',
      dataIndex: 'fulfillmentStatus',
      render: (_, row) => {
        const meta = FULFILLMENT_STATUS[row.fulfillmentStatus] ?? {
          text: row.fulfillmentStatus,
          color: 'default',
        };
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '来源',
      dataIndex: 'source',
      render: (_, row) => ORDER_SOURCE[row.source]?.text ?? row.source,
    },
    { title: '用户 ID', dataIndex: 'userId', copyable: true },
    {
      title: '门店',
      dataIndex: 'storeName',
      render: (_, row) => dash(row.storeName || row.storeId),
    },
    { title: '门店 ID', dataIndex: 'storeId', copyable: true, render: (_, row) => dash(row.storeId) },
    {
      title: '设备编码',
      dataIndex: 'deviceNo',
      render: (_, row) => dash(row.deviceNo || row.deviceId),
    },
    { title: '商品原价', dataIndex: 'originalAmount', render: (_, row) => money(row.originalAmount) },
    { title: '优惠合计', dataIndex: 'discountAmount', render: (_, row) => money(row.discountAmount) },
    { title: '应付金额', dataIndex: 'payableAmount', render: (_, row) => money(row.payableAmount) },
    { title: '实付金额', dataIndex: 'paidAmount', render: (_, row) => money(row.paidAmount) },
    { title: '已退金额', dataIndex: 'refundedAmount', render: (_, row) => money(row.refundedAmount) },
    {
      title: '支付方式',
      dataIndex: 'paymentMethod',
      render: (_, row) => paymentMethodLabel(row.paymentMethod),
    },
    { title: '支付单号', dataIndex: 'paymentNo', copyable: true, render: (_, row) => dash(row.paymentNo) },
    {
      // 接口没有「订单类型」这一列（它是从行里派生出来的），显示接口算好的三个行类型标记。
      // 渲染与列表的「构成」列共用 CompositionTags：同一张单在两处必须读起来一样。
      title: '订单构成',
      dataIndex: 'hasDrinkLine',
      render: (_, row) => <CompositionTags row={row} />,
    },
    {
      title: '承诺福卡',
      dataIndex: 'fortuneCardsExpected',
      // 只在这一单真承诺过时才摆：一个恒为 0 的字段会让人以为「福卡功能没配」。
      render: (_, row) =>
        row.fortuneCardsExpected > 0 ? `${row.fortuneCardsExpected} 张` : <span style={{ color: '#8c8c8c' }}>无</span>,
    },
    { title: '下单时间', dataIndex: 'createdAt', valueType: 'dateTime' },
    { title: '支付时间', dataIndex: 'paidAt', valueType: 'dateTime' },
    { title: '完成时间', dataIndex: 'finishedAt', valueType: 'dateTime' },
    { title: '取消时间', dataIndex: 'cancelledAt', valueType: 'dateTime' },
    { title: '支付截止', dataIndex: 'expiresAt', valueType: 'dateTime' },
    {
      title: '取消原因',
      dataIndex: 'cancellationReason',
      render: (_, row) => dash(row.cancellationReason),
    },
    {
      title: '会员 ID',
      dataIndex: 'membershipId',
      copyable: true,
      render: (_, row) => dash(row.membershipId),
    },
    { title: '场景令牌', dataIndex: 'sceneToken', copyable: true, render: (_, row) => dash(row.sceneToken) },
    {
      title: '备注',
      dataIndex: 'remark',
      span: 2,
      render: (_, row) => dash(row.remark),
    },
    { title: '更新时间', dataIndex: 'updatedAt', valueType: 'dateTime' },
    { title: '订单 ID', dataIndex: 'id', copyable: true },
  ];

  // 会员与福卡的快照是下单那一刻抄下来的 JSON（jsonb 解出来的对象，形状随业务变），
  // 所以不铺成一列一列，原样贴出来：它是给对账时核对用的原始凭证，不是给人扫的表格。
  const snapshotBlock = (title: string, value: unknown) => {
    if (value === null || value === undefined) return null;
    return (
      <div style={{ marginTop: 16 }}>
        <Typography.Text strong>{title}</Typography.Text>
        <pre
          style={{
            margin: '8px 0 0',
            padding: 12,
            background: '#fafafa',
            borderRadius: 4,
            maxHeight: 240,
            overflow: 'auto',
            fontSize: 12,
          }}
        >
          {JSON.stringify(value, null, 2)}
        </pre>
      </div>
    );
  };

  return (
    <>
      <ProDescriptions<OrderDetail>
        column={2}
        dataSource={order}
        title="订单信息"
        columns={columns}
      />
      {snapshotBlock('会员快照', order.membershipSnapshot)}
      {snapshotBlock('福卡快照', order.fortuneCardSnapshot)}
    </>
  );
}
