import { Table, Tag } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { formatSignedYuan } from '../../../services/money';
import type { PaymentDetail, PaymentTransaction } from '../../../services/payment';
import { TRANSACTION_DIRECTION, TRANSACTION_KIND } from '../../../services/paymentLabels';
import { paymentMethodLabel } from '../../../services/paymentMethodLabels';
import { dash, time } from './render';

/**
 * 支付单详情 → 记账流水。
 *
 * 这张表是**只增的**：一笔出资成功写一条 in，将来退款写一条 out，冲正再写一条。所以同一条
 * 出资行会有多行流水，这里**不合并**——按出资行合并之后冲正就看不见了，而「这笔钱被反做过」
 * 恰恰是对账时最要紧的一件事。
 *
 * 金额带符号（进账 +、出账 -）：光看绝对值的话，一条 out 看起来像又进了一笔钱。符号是按
 * direction 现算的，不是库里的列——库里 amount 恒为正，配合 direction 才有方向。
 */

export default function TransactionsTab({ detail }: { detail: PaymentDetail }) {
  const columns: ColumnsType<PaymentTransaction> = [
    {
      title: '类型',
      dataIndex: 'kind',
      width: 90,
      render: (_, row) => {
        const meta = TRANSACTION_KIND[row.kind] ?? { text: row.kind, color: 'default' };
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '方向',
      dataIndex: 'direction',
      width: 90,
      render: (_, row) => {
        const meta = TRANSACTION_DIRECTION[row.direction] ?? {
          text: row.direction,
          color: 'default',
        };
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '金额',
      dataIndex: 'amount',
      width: 110,
      align: 'right',
      render: (_, row) => (
        <span style={{ color: row.direction === 'out' ? '#cf1322' : '#3f8600' }}>
          {formatSignedYuan(row.direction === 'out' ? -row.amount : row.amount)}
        </span>
      ),
    },
    {
      // 与出资行那一列同一个东西、同一份文案：lineType 是这笔账对应的支付方式 code。
      title: '支付方式',
      dataIndex: 'lineType',
      width: 150,
      render: (_, row) => paymentMethodLabel(row.lineType),
    },
    { title: '出资行号', dataIndex: 'fundingLineNo', width: 80 },
    {
      // 今天全是空串：退款单还没有代码在写（这一刀也没给退款开页签）。
      title: '退款单号',
      dataIndex: 'refundNo',
      width: 190,
      ellipsis: true,
      render: (_, row) => dash(row.refundNo),
    },
    {
      title: '渠道交易号',
      dataIndex: 'providerTransactionId',
      width: 200,
      ellipsis: true,
      render: (_, row) => dash(row.providerTransactionId),
    },
    {
      title: '账户账变 ID',
      dataIndex: 'accountEntryId',
      width: 200,
      ellipsis: true,
      render: (_, row) => dash(row.accountEntryId),
    },
    {
      title: '老系统 ID',
      dataIndex: 'legacyId',
      width: 190,
      ellipsis: true,
      render: (_, row) => dash(row.legacyId),
    },
    {
      // 账发生的时间（渠道报的），与下面那行的入库时间可能差几秒——对账看的是前者。
      title: '发生时间',
      dataIndex: 'occurredAt',
      width: 170,
      render: (_, row) => time(row.occurredAt),
    },
    { title: '入库时间', dataIndex: 'createdAt', width: 170, render: (_, row) => time(row.createdAt) },
  ];

  return (
    <Table<PaymentTransaction>
      rowKey="id"
      size="small"
      columns={columns}
      dataSource={detail.transactions}
      pagination={false}
      // 1640 = 90+90+110+150+80+190+200+200+190+170+170，各列 width 之和。
      scroll={{ x: 1640 }}
      locale={{
        emptyText: (
          <div style={{ padding: '32px 0', color: '#999' }}>
            这笔支付还没有记账流水。出资成功时才会写。
          </div>
        ),
      }}
    />
  );
}
