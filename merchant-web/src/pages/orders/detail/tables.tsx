import { Table, Tag, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { enumMeta } from '../../../services/labels';
import { formatYuan } from '../../../services/money';
import type {
  AfterSale,
  OrderLine,
  OrderPaymentLine,
  OrderTransition,
} from '../../../services/order';
import {
  ACTOR_TYPE,
  AFTER_SALE_SCOPE,
  AFTER_SALE_STATUS,
  AGGREGATE_TYPE,
  ORDER_LINE_TYPE,
  PAYMENT_LINE_STATUS,
  paymentMethodLabel,
} from '../../../services/orderLabels';

/**
 * 订单详情里那四张只读表。
 *
 * 合成一个文件而不是一屏一个：它们都是「把详情接口已经带回来的那个数组铺开」，没有各自的
 * 取数、分页或筛选，拆开只是把同一个 import 块抄四遍。**没有分页**：这些数组是随订单详情
 * 一次性回来的（一单的行、出资、流水都是个位数到几十条），摆一个前端分页器只会让人以为
 * 后面还有没取到的数据。
 */

const dash = (value?: string | null) => (value?.trim() ? value : '—');

/** 商品明细。金额一律是分，展示按元。 */
export function LinesTable({ lines }: { lines: OrderLine[] }) {
  const columns: ColumnsType<OrderLine> = [
    { title: '行号', dataIndex: 'lineNo', width: 70 },
    {
      title: '类型',
      dataIndex: 'lineType',
      width: 100,
      render: (_, row) => {
        const meta = enumMeta(ORDER_LINE_TYPE, row.lineType);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    { title: '商品名称', dataIndex: 'itemName', ellipsis: true },
    { title: '商品编码', dataIndex: 'itemCode', width: 140, render: (_, r) => dash(r.itemCode) },
    { title: '数量', dataIndex: 'quantity', width: 80 },
    { title: '原价', dataIndex: 'originalUnitPrice', width: 100, render: (_, r) => formatYuan(r.originalUnitPrice) },
    { title: '单价', dataIndex: 'unitPrice', width: 100, render: (_, r) => formatYuan(r.unitPrice) },
    {
      title: '优惠',
      dataIndex: 'discountAmount',
      width: 100,
      render: (_, r) => formatYuan(r.discountAmount),
    },
    {
      title: '应付',
      dataIndex: 'payableAmount',
      width: 100,
      render: (_, r) => formatYuan(r.payableAmount),
    },
    // 取杯号只有饮品行有。它与列表上那个是同一个值，不是凭据。
    { title: '取杯号', dataIndex: 'pickupCode', width: 100, render: (_, r) => dash(r.pickupCode) },
  ];
  return <Table<OrderLine> rowKey="id" size="small" columns={columns} dataSource={lines} pagination={false} />;
}

/** 出资分摊：这一单的钱分别是谁出的、走到了哪一步。 */
export function PaymentLinesTable({ lines }: { lines: OrderPaymentLine[] }) {
  const columns: ColumnsType<OrderPaymentLine> = [
    { title: '行号', dataIndex: 'lineNo', width: 70 },
    // 与订单基本信息的「支付方式」是同一个值（支付方式的 code），所以文案也走同一份映射：
    // 这里给的是中文名而不是 Tag——那一列的值域是一张会长的常量表，不是库里的 CHECK。
    { title: '支付方式', dataIndex: 'lineType', width: 150, render: (_, row) => paymentMethodLabel(row.lineType) },
    { title: '金额', dataIndex: 'amount', width: 110, render: (_, r) => formatYuan(r.amount) },
    {
      title: '状态',
      dataIndex: 'status',
      width: 110,
      render: (_, row) => {
        const meta = enumMeta(PAYMENT_LINE_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    { title: '支付单号', dataIndex: 'paymentNo', width: 200, ellipsis: true, render: (_, r) => dash(r.paymentNo) },
    // 失败码只有出资失败时有值；空着的时候不显示一列空白，所以放同一格里。
    { title: '失败码', dataIndex: 'failureCode', width: 140, render: (_, r) => dash(r.failureCode) },
    { title: '成功时间', dataIndex: 'succeededAt', width: 180, render: (_, r) => dash(r.succeededAt) },
    { title: '冲正时间', dataIndex: 'reversedAt', width: 180, render: (_, r) => dash(r.reversedAt) },
  ];
  return (
    <Table<OrderPaymentLine>
      rowKey="id"
      size="small"
      columns={columns}
      dataSource={lines}
      pagination={false}
    />
  );
}

/** 状态流水：谁在什么时候把什么状态改成了什么。 */
export function TransitionsTable({ transitions }: { transitions: OrderTransition[] }) {
  const columns: ColumnsType<OrderTransition> = [
    {
      title: '对象',
      dataIndex: 'aggregateType',
      width: 110,
      render: (_, row) => {
        const meta = enumMeta(AGGREGATE_TYPE, row.aggregateType);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    { title: '对象 ID', dataIndex: 'aggregateId', width: 300, ellipsis: true },
    {
      title: '迁移',
      dataIndex: 'fromStatus',
      width: 220,
      render: (_, r) => `${dash(r.fromStatus)} → ${dash(r.toStatus)}`,
    },
    {
      title: '操作人',
      dataIndex: 'actorType',
      width: 100,
      render: (_, row) => {
        const meta = enumMeta(ACTOR_TYPE, row.actorType);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    { title: '操作人 ID', dataIndex: 'actorId', width: 300, ellipsis: true, render: (_, r) => dash(r.actorId) },
    { title: '原因', dataIndex: 'reason', ellipsis: true, render: (_, r) => dash(r.reason) },
    { title: '时间', dataIndex: 'createdAt', width: 180 },
  ];
  return (
    <Table<OrderTransition>
      rowKey="id"
      size="small"
      columns={columns}
      dataSource={transitions}
      pagination={false}
    />
  );
}

/**
 * 这一单的售后记录。
 *
 * 商户端**只读**：审核在后台（那是一次要写平台审计的人工干预）。所以这里没有通过/驳回
 * 按钮，连查售后列表的入口都没有——售后不是独立入口，它随订单一起受数据范围约束。
 */
export function AfterSalesTable({ afterSales }: { afterSales: AfterSale[] }) {
  const columns: ColumnsType<AfterSale> = [
    { title: '售后单号', dataIndex: 'afterSaleNo', width: 200, ellipsis: true },
    {
      title: '状态',
      dataIndex: 'status',
      width: 110,
      render: (_, row) => {
        const meta = enumMeta(AFTER_SALE_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '范围',
      dataIndex: 'scope',
      width: 110,
      render: (_, row) => {
        const meta = enumMeta(AFTER_SALE_SCOPE, row.scope);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    { title: '退款金额', dataIndex: 'refundAmount', width: 110, render: (_, r) => formatYuan(r.refundAmount) },
    { title: '申请原因', dataIndex: 'reason', ellipsis: true, render: (_, r) => dash(r.reason) },
    // approved 只是「审核通过」，钱由 payment-service 退——这一格空着不代表没退过，退款单号
    // 只有真退了才有。
    { title: '退款单号', dataIndex: 'refundNo', width: 200, ellipsis: true, render: (_, r) => dash(r.refundNo) },
    { title: '审核备注', dataIndex: 'reviewRemark', ellipsis: true, render: (_, r) => dash(r.reviewRemark) },
    { title: '申请时间', dataIndex: 'createdAt', width: 180 },
  ];
  if (!afterSales.length) {
    return <Typography.Text type="secondary">这一单没有售后记录。</Typography.Text>;
  }
  return (
    <Table<AfterSale> rowKey="id" size="small" columns={columns} dataSource={afterSales} pagination={false} />
  );
}
