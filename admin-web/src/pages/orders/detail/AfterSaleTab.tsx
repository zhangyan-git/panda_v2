import { useAccess } from '@umijs/max';
import { Button, Image, Space, Table, Tag, Tooltip, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { useState } from 'react';
import { formatDateTime } from '../../../services/datetime';
import { formatYuan } from '../../../services/money';
import type { AfterSale, OrderDetail } from '../../../services/order';
import { AFTER_SALE_SCOPE, AFTER_SALE_STATUS } from '../../../services/orderLabels';
import ReviewModal from '../../after-sales/ReviewModal';

/**
 * 订单详情 → 售后记录：这一单提交过的退款申请，新的在前。
 *
 * 客服的入口就是这里——用户打电话来问退款，客服拿着单号进来就能看到进度，不必再去退款申请
 * 列表里按订单号搜一遍。审核按钮也放在这一屏，理由同上：看到进度和决定要不要通过是同一件事。
 *
 * 审核弹窗与退款申请列表共用（同一个 ReviewModal）：**福卡规则必须在两个入口上都拦一次**，
 * 各写一份的话迟早会有一边漏掉那句话。
 */

const dash = (value?: string | null) => (value ? value : '—');
const time = (value?: string | null) => (value ? formatDateTime(value) : '—');
const money = (fen?: number | null) => `¥${formatYuan(fen)}`;

export default function AfterSaleTab({
  order,
  onReviewed,
}: {
  order: OrderDetail;
  /** 审完通知外面重新取一次订单：售后状态变了，这一屏要跟着变。 */
  onReviewed: () => void;
}) {
  const access = useAccess();
  const [reviewing, setReviewing] = useState<{ sale: AfterSale; action: 'approve' | 'reject' }>();

  const columns: ColumnsType<AfterSale> = [
    {
      // antd 的 Table 没有 ProTable 的 copyable 列属性，用 Typography 自己给一个复制按钮：
      // 单号是客服要报给开发/对账的那串字，能一键复制比手抄强。
      title: '售后单号',
      dataIndex: 'afterSaleNo',
      width: 190,
      render: (_, row) => <Typography.Text copyable>{row.afterSaleNo}</Typography.Text>,
    },
    {
      title: '范围',
      dataIndex: 'scope',
      width: 100,
      render: (_, row) => {
        const meta = AFTER_SALE_SCOPE[row.scope] ?? { text: row.scope, color: 'default' };
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 按行退时才有一行可指。摆的是行号 + 商品名：售后那一侧存的是 orderLineId，
      // 光看 uuid 认不出退的是哪一杯。
      title: '退的行',
      dataIndex: 'orderLineId',
      width: 200,
      ellipsis: true,
      render: (_, row) =>
        row.orderLine
          ? `第 ${row.orderLine.lineNo} 行 ${row.orderLine.itemName} ×${row.orderLine.quantity}`
          : '—',
    },
    { title: '退款金额', dataIndex: 'refundAmount', width: 100, render: (_, row) => money(row.refundAmount) },
    {
      title: '状态',
      dataIndex: 'status',
      width: 90,
      render: (_, row) => {
        const meta = AFTER_SALE_STATUS[row.status] ?? { text: row.status, color: 'default' };
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    { title: '申请理由', dataIndex: 'reason', width: 200, ellipsis: true, render: (_, row) => dash(row.reason) },
    {
      title: '凭证',
      dataIndex: 'images',
      width: 100,
      render: (_, row) => {
        const images = row.images ?? [];
        if (images.length === 0) return '—';
        // 图片只在点开时加载（Image 预览），列表里只摆缩略图：凭证常是好几张手机截图，
        // 全部铺开会把这一行撑得很高。
        return (
          <Image.PreviewGroup>
            <Space size={4}>
              {images.map((url) => (
                <Image key={url} src={url} width={32} height={32} style={{ objectFit: 'cover' }} />
              ))}
            </Space>
          </Image.PreviewGroup>
        );
      },
    },
    { title: '申请时间', dataIndex: 'createdAt', width: 170, render: (_, row) => time(row.createdAt) },
    {
      title: '审核人 ID',
      dataIndex: 'reviewedBy',
      width: 180,
      ellipsis: true,
      render: (_, row) => dash(row.reviewedBy),
    },
    { title: '审核时间', dataIndex: 'reviewedAt', width: 170, render: (_, row) => time(row.reviewedAt) },
    {
      title: '审核备注',
      dataIndex: 'reviewRemark',
      width: 200,
      ellipsis: true,
      render: (_, row) => dash(row.reviewRemark),
    },
    {
      title: '退款单号',
      dataIndex: 'refundNo',
      width: 190,
      ellipsis: true,
      render: (_, row) => dash(row.refundNo),
    },
    { title: '退款时间', dataIndex: 'refundedAt', width: 170, render: (_, row) => time(row.refundedAt) },
    {
      title: '操作',
      key: 'action',
      width: 160,
      fixed: 'right',
      render: (_, row) => {
        // 两档判断：有审核权限，且这一张还等着人来审（其余状态再点就是 409）。
        if (!access.canReviewAfterSales || row.status !== 'pending') return '—';
        return (
          <Space size={0}>
            <Button
              type="link"
              size="small"
              onClick={() => setReviewing({ sale: row, action: 'approve' })}
            >
              通过申请
            </Button>
            <Button
              type="link"
              size="small"
              danger
              onClick={() => setReviewing({ sale: row, action: 'reject' })}
            >
              驳回申请
            </Button>
          </Space>
        );
      },
    },
  ];

  return (
    <>
      {order.fortuneCardsExpected > 0 && (
        // 审核前要看的那个前提就摆在这一屏上：这一单承诺过福卡，福卡规则才适用。放在这里
        // 而不是只写进弹窗里，是为了让人在点按钮之前就知道有这么一条规矩。
        <Tooltip title="订单赠送的福卡若已参与抽奖，整笔不可退">
          <Tag color="gold" style={{ marginBottom: 12 }}>
            本单承诺赠送 {order.fortuneCardsExpected} 张福卡，审核通过前需确认未参与抽奖
          </Tag>
        </Tooltip>
      )}
      <Table<AfterSale>
        rowKey="id"
        size="small"
        columns={columns}
        dataSource={order.afterSales}
        pagination={false}
        // 钉了右列（操作），所以这里的 x 必须覆盖各列宽度之和，否则固定列会算错位置。
        // 2220 = 190+100+200+100+90+200+100+170+180+170+200+190+170+160。
        scroll={{ x: 2220 }}
      />
      <ReviewModal
        open={!!reviewing}
        onOpenChange={(open) => {
          if (!open) setReviewing(undefined);
        }}
        afterSale={reviewing?.sale}
        action={reviewing?.action ?? 'approve'}
        onReviewed={() => {
          setReviewing(undefined);
          onReviewed();
        }}
      />
    </>
  );
}
