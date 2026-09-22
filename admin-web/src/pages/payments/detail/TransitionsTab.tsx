import { Table, Tag } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { enumMeta } from '../../../services/labels';
import type { PaymentDetail, PaymentTransition } from '../../../services/payment';
import { ACTOR_TYPE, PAYMENT_STATUS } from '../../../services/paymentLabels';
import { dash, RawBlock, time } from './render';

/**
 * 支付单详情 → 状态流转。
 *
 * `reason` 是**原样显示的**，不做码表映射。它是中英混着的诊断串——`payment created`、
 * `provider reported payment failed: 用户取消`、`咖啡豆余额不足，需要 3400 分`——写它的人
 * 当时就在代码里混着写，硬翻成一张中文码表只会把「当时到底发生了什么」这条唯一的一手信息
 * 换成二手猜测。这一列给得比其他列宽，就是要让人读得完整。
 *
 * 只显示 aggregate_type = 'payment' 的那一层（后端就只查这一层）：出资行/退款单/协议自己的
 * 状态迁移有它们各自的时间线，混进来会让这张表多出不是支付单自己的节点。
 */

export default function TransitionsTab({ detail }: { detail: PaymentDetail }) {
  const columns: ColumnsType<PaymentTransition> = [
    {
      title: '状态变更',
      dataIndex: 'toStatus',
      width: 220,
      // 前后两个状态是同一套词表（payments.status），所以翻得动；认不出来的码原样显示，
      // 那是「迁移加了新状态而文案表没跟上」的信号。
      render: (_, row) =>
        `${enumMeta(PAYMENT_STATUS, row.fromStatus).text} → ${enumMeta(PAYMENT_STATUS, row.toStatus).text}`,
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
      // system 迁移没有操作者，是 null。
      title: '操作者 ID',
      dataIndex: 'actorId',
      width: 200,
      ellipsis: true,
      render: (_, row) => dash(row.actorId),
    },
    {
      title: '原因',
      dataIndex: 'reason',
      width: 360,
      render: (_, row) => (
        <span style={{ wordBreak: 'break-all' }}>{dash(row.reason)}</span>
      ),
    },
    {
      // 把这一次流转与同一请求下的渠道调用、回调通知串起来的那根线。
      title: '请求 ID',
      dataIndex: 'requestId',
      width: 200,
      ellipsis: true,
      render: (_, row) => dash(row.requestId),
    },
    {
      title: '附加数据',
      dataIndex: 'metadata',
      width: 240,
      render: (_, row) => <RawBlock value={row.metadata} />,
    },
    { title: '时间', dataIndex: 'createdAt', width: 170, render: (_, row) => time(row.createdAt) },
  ];

  return (
    <Table<PaymentTransition>
      rowKey="id"
      size="small"
      columns={columns}
      dataSource={detail.transitions}
      pagination={false}
      // 1490 = 220+100+200+360+200+240+170，各列 width 之和。
      scroll={{ x: 1490 }}
      locale={{
        emptyText: (
          <div style={{ padding: '32px 0', color: '#999' }}>
            这笔支付还没有状态流转记录。
          </div>
        ),
      }}
    />
  );
}
