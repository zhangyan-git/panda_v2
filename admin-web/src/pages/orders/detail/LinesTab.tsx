import { Table } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { Tag } from 'antd';
import { formatYuan } from '../../../services/money';
import type { OrderDetail, OrderLine } from '../../../services/order';
import { ORDER_LINE_TYPE } from '../../../services/orderLabels';

/**
 * 订单详情 → 订单行。一单里有几行（饮品 / 加购 / 会员套餐），每行的价与优惠分摊。
 *
 * 用 antd 的 Table 而不是 ProTable：数据已经在手上（详情接口一次给全），这里没有请求、
 * 没有分页、没有筛选，上 ProTable 只会多一层它为「自己去取数」准备的东西。
 */

const money = (fen?: number | null) => `¥${formatYuan(fen)}`;
const dash = (value?: string | null) => (value ? value : '—');

/** 快照类字段（规格、选项）是自由 JSON，压成一行给人扫；太长时由 ellipsis 收掉。 */
const compact = (value: unknown) => {
  if (value === null || value === undefined) return '—';
  if (typeof value === 'string') return value || '—';
  const text = JSON.stringify(value);
  return text && text !== '{}' && text !== '[]' ? text : '—';
};

export default function LinesTab({ order }: { order: OrderDetail }) {
  const columns: ColumnsType<OrderLine> = [
    { title: '行号', dataIndex: 'lineNo', width: 60 },
    {
      title: '类型',
      dataIndex: 'lineType',
      width: 90,
      render: (_, row) => {
        const meta = ORDER_LINE_TYPE[row.lineType] ?? { text: row.lineType, color: 'default' };
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    { title: '商品', dataIndex: 'itemName', width: 160, ellipsis: true },
    { title: '规格', dataIndex: 'specs', width: 140, ellipsis: true, render: (_, row) => compact(row.specs) },
    { title: '数量', dataIndex: 'quantity', width: 70 },
    {
      title: '原价单价',
      dataIndex: 'originalUnitPrice',
      width: 100,
      render: (_, row) => money(row.originalUnitPrice),
    },
    {
      // 「折后单价」与「原价单价」是两个数：前者是本行的成交价，后者是目录价。
      title: '折后单价',
      dataIndex: 'unitPrice',
      width: 100,
      render: (_, row) => money(row.unitPrice),
    },
    {
      // 本行自身的定价优惠（活动价一类）与券抵扣分开摆：退款时要能说清这一行的钱是
      // 被活动减掉的还是被券抵掉的，两者冲正的方式不同。
      title: '定价优惠',
      dataIndex: 'priceDiscountAmount',
      width: 100,
      render: (_, row) => money(row.priceDiscountAmount),
    },
    {
      title: '优惠合计',
      dataIndex: 'discountAmount',
      width: 100,
      render: (_, row) => money(row.discountAmount),
    },
    {
      title: '应付',
      dataIndex: 'payableAmount',
      width: 100,
      render: (_, row) => money(row.payableAmount),
    },
    {
      title: '券抵扣',
      dataIndex: 'couponDiscountAmount',
      width: 100,
      render: (_, row) => money(row.couponDiscountAmount),
    },
    {
      title: '用户券 ID',
      dataIndex: 'couponId',
      width: 150,
      ellipsis: true,
      render: (_, row) => dash(row.couponId),
    },
    {
      title: '活动 ID',
      dataIndex: 'campaignId',
      width: 150,
      ellipsis: true,
      render: (_, row) => dash(row.campaignId),
    },
    {
      // 下面这几个是设备侧的单号：出杯异常时拿它们去问厂商，是这个页面上唯一能对上号的东西。
      title: '设备订单号',
      dataIndex: 'deviceOrderNo',
      width: 160,
      ellipsis: true,
      render: (_, row) => dash(row.deviceOrderNo),
    },
    {
      title: '履约任务号',
      dataIndex: 'fulfillmentTaskNo',
      width: 160,
      ellipsis: true,
      render: (_, row) => dash(row.fulfillmentTaskNo),
    },
    {
      // 取杯号：支付成功时生成，取杯口屏幕上大字显示的也是它（那边叫取杯码，同一个值）。
      // 它曾经被当成后台不该看的凭据，那建立在一次凭空的列拆分上（见 migrations/order）。
      title: '取杯号',
      dataIndex: 'pickupCode',
      width: 120,
      render: (_, row) => dash(row.pickupCode),
    },
    {
      title: '备注',
      dataIndex: 'remark',
      width: 160,
      ellipsis: true,
      render: (_, row) => dash(row.remark),
    },
  ];

  return (
    <Table<OrderLine>
      rowKey="id"
      size="small"
      columns={columns}
      dataSource={order.lines}
      pagination={false}
      // 列多，窄屏下横向滚动比把每列压成两个字强。这里没有钉右列，所以不需要它等于列宽之和。
      scroll={{ x: 2020 }}
    />
  );
}
