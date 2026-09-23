import { Table, Tag } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import type { PaymentDetail, PaymentFunding } from '../../../services/payment';
import { FUNDING_STATUS } from '../../../services/paymentLabels';
import { paymentMethodLabel } from '../../../services/paymentMethodLabels';
import { dash, money, time } from './render';

/**
 * 支付单详情 → 出资行：这一笔钱是按什么拆的（整笔微信 / 一部分咖啡豆 + 一部分微信 …）。
 *
 * 没有「退款」这一列：退款单在 payment_refunds，这一刀没做也不显示（那三组表零行）。
 * 这里只有 `reversedAt`——它记的是**这笔出资自己**有没有被反做，与「用户申请退款」是两件事。
 */

export default function FundingsTab({ detail }: { detail: PaymentDetail }) {
  const columns: ColumnsType<PaymentFunding> = [
    { title: '行号', dataIndex: 'lineNo', width: 60 },
    {
      // 这一列从前叫「出资类型」，是另一套词表；今天 lineType 就是支付方式的 code
      // （与支付单上的 methodCode 同一个值，见 migrations/payment），所以标题跟着改成同一个说法，
      // 中文名也与订单页共用 paymentMethodLabels 那一份。
      title: '支付方式',
      dataIndex: 'lineType',
      width: 150,
      render: (_, row) => paymentMethodLabel(row.lineType),
    },
    { title: '金额', dataIndex: 'amount', width: 100, align: 'right', render: (_, row) => money(row.amount) },
    {
      title: '状态',
      dataIndex: 'status',
      width: 90,
      render: (_, row) => {
        const meta = FUNDING_STATUS[row.status] ?? { text: row.status, color: 'default' };
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '渠道交易号',
      dataIndex: 'providerTransactionId',
      width: 200,
      ellipsis: true,
      render: (_, row) => dash(row.providerTransactionId),
    },
    {
      // 渠道给的原文，不翻译：报障给渠道、查渠道文档都靠它。
      title: '失败码',
      dataIndex: 'failureCode',
      width: 140,
      ellipsis: true,
      render: (_, row) => dash(row.failureCode),
    },
    {
      // 咖啡豆出资扣的是账户余额，冲正靠这笔账变。对账时要拿它去 account-service 查。
      title: '账户账变 ID',
      dataIndex: 'accountEntryId',
      width: 200,
      ellipsis: true,
      render: (_, row) => dash(row.accountEntryId),
    },
    { title: '成功时间', dataIndex: 'succeededAt', width: 170, render: (_, row) => time(row.succeededAt) },
    { title: '冲正时间', dataIndex: 'reversedAt', width: 170, render: (_, row) => time(row.reversedAt) },
    { title: '创建时间', dataIndex: 'createdAt', width: 170, render: (_, row) => time(row.createdAt) },
  ];

  return (
    <Table<PaymentFunding>
      rowKey="id"
      size="small"
      columns={columns}
      dataSource={detail.fundings}
      pagination={false}
      // 1450 = 60+150+100+90+200+140+200+170+170+170，各列 width 之和。
      scroll={{ x: 1450 }}
      locale={{
        emptyText: (
          // 空与「还没开始出资」是一回事：支付单刚建出来、还没走到出资那一步时就是这个样子。
          <div style={{ padding: '32px 0', color: '#999' }}>
            这笔支付还没有出资行。支付单刚创建、渠道还没回调时就是这样。
          </div>
        ),
      }}
    />
  );
}
