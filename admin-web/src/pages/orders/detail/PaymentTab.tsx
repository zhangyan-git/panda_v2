import { Table, Tag } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { formatDateTime } from '../../../services/datetime';
import { formatYuan } from '../../../services/money';
import type { OrderDetail, OrderPaymentLine } from '../../../services/order';
import { PAYMENT_LINE_STATUS } from '../../../services/orderLabels';
import { paymentMethodLabel } from '../../../services/paymentMethodLabels';

/**
 * 订单详情 → 出资分摊：这一单的钱是哪几笔凑出来的（支付宝 / 咖啡豆 …）。
 *
 * 老系统这一屏是「支付流水」，列的是渠道返回的一笔笔流水。本服务里没有那张表——渠道流水与
 * 退款单都属于 payment-service（还没建），这里有的是**出资分摊**：一笔支付按来源拆成的行。
 * 两者不是一个东西，所以这一屏也不叫「支付流水」。
 *
 * 没有渠道流水号（provider_transaction_id）：后端有意不映射它（对账凭据，要看就去
 * payment-service 查），所以这里既不给这一列，也不该去别处找。
 */

const money = (fen?: number | null) => `¥${formatYuan(fen)}`;
const dash = (value?: string | null) => (value ? value : '—');
// 这里是 antd 的 Table，没有 ProTable 那套 valueType: 'dateTime'，所以自己格式化。
// 用 services/datetime.ts 的那一份，与页面上其它地方同一个口径。
const time = (value?: string | null) => (value ? formatDateTime(value) : '—');

export default function PaymentTab({ order }: { order: OrderDetail }) {
  const columns: ColumnsType<OrderPaymentLine> = [
    { title: '行号', dataIndex: 'lineNo', width: 60 },
    {
      // lineType 是支付方式的 code，与订单表上的 paymentMethod 同一个值——文案走订单页与
      // 支付页共用的那一份，认不出来的原样回显（从前是手工 `PAYMENT_LINE_TYPE[x] ?? x`，
      // 那套词表已经退场，回显也就成了唯一的兜底）。
      title: '支付方式',
      dataIndex: 'lineType',
      width: 150,
      render: (_, row) => paymentMethodLabel(row.lineType),
    },
    { title: '金额', dataIndex: 'amount', width: 100, render: (_, row) => money(row.amount) },
    {
      title: '状态',
      dataIndex: 'status',
      width: 90,
      render: (_, row) => {
        const meta = PAYMENT_LINE_STATUS[row.status] ?? { text: row.status, color: 'default' };
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '支付单号',
      dataIndex: 'paymentNo',
      width: 190,
      ellipsis: true,
      render: (_, row) => dash(row.paymentNo),
    },
    {
      // 失败码是渠道给的原文，不做翻译：查渠道文档、报障给渠道都靠它。
      title: '失败码',
      dataIndex: 'failureCode',
      width: 140,
      ellipsis: true,
      render: (_, row) => dash(row.failureCode),
    },
    {
      // 咖啡豆出资扣的是账户余额，冲正靠这笔账变。对账时要拿它去 account-service 查。
      title: '账变 ID',
      dataIndex: 'accountEntryId',
      width: 200,
      ellipsis: true,
      render: (_, row) => dash(row.accountEntryId),
    },
    {
      title: '成功时间',
      dataIndex: 'succeededAt',
      width: 170,
      render: (_, row) => time(row.succeededAt),
    },
    {
      title: '冲正时间',
      dataIndex: 'reversedAt',
      width: 170,
      render: (_, row) => time(row.reversedAt),
    },
    {
      title: '创建时间',
      dataIndex: 'createdAt',
      width: 170,
      render: (_, row) => time(row.createdAt),
    },
  ];

  return (
    <Table<OrderPaymentLine>
      rowKey="id"
      size="small"
      columns={columns}
      dataSource={order.paymentLines}
      pagination={false}
      // 1440 = 60+150+100+90+190+140+200+170+170+170，各列 width 之和。
      scroll={{ x: 1440 }}
    />
  );
}
