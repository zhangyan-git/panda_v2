import { PageContainer, ProTable } from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { history, useAccess } from '@umijs/max';
import { Button, Image, message, Space, Tag, Tooltip } from 'antd';
import { useRef, useState } from 'react';
import { toRFC3339 } from '../../services/datetime';
import { enumMeta, searchOptions } from '../../services/labels';
import { formatYuan } from '../../services/money';
import { listAfterSales, type AfterSale, type AfterSaleQuery } from '../../services/order';
import { AFTER_SALE_SCOPE, AFTER_SALE_STATUS } from '../../services/orderLabels';
import { requestErrorMessage } from '../../services/requestError';
import ReviewModal from './ReviewModal';

/**
 * 退款申请列表。这一页审的是用户提交的售后申请——退款单本身在 payment-service（还没建），
 * 所以这里审的是「要不要退」，不是「钱退了没有」。
 *
 * **没有详情页**：列表返回的就是完整售后单（含凭证图片与福卡快照），点开只是为了看同一份
 * 数据，所以凭证就在行内展开看，不为它单开一屏。订单详情里的「售后记录」tab 是另一个入口，
 * 两个入口共用同一个审核弹窗（福卡规则要在两边都拦一次）。
 */

const dash = (value?: string | null) => (value ? value : '—');

/** 精确匹配的筛选参数：trim 之后空的丢掉——后端全是等值比较，多一个空格就是「查不到」。 */
const exact = (value: unknown): string | undefined => {
  const trimmed = typeof value === 'string' ? value.trim() : '';
  return trimmed || undefined;
};

export default function AfterSalesPage() {
  const access = useAccess();
  const actionRef = useRef<ActionType>();
  const [reviewing, setReviewing] = useState<{ sale: AfterSale; action: 'approve' | 'reject' }>();

  const columns: ProColumns<AfterSale>[] = [
    {
      // 仅搜索用的时间范围。独立的 dataIndex，不与下面那列 createdAt 共用（同名两列会在搜索
      // 表单里争同一个键，范围数组覆盖掉字符串之后表格里的时间列就空了）。
      title: '申请时间',
      dataIndex: 'createdRange',
      valueType: 'dateTimeRange',
      hideInTable: true,
      search: {
        transform: (value: unknown) => {
          const range = (value ?? []) as unknown[];
          return { createdFrom: toRFC3339(range[0]), createdTo: toRFC3339(range[1]) };
        },
      },
    },
    {
      title: '售后单号',
      dataIndex: 'afterSaleNo',
      copyable: true,
      ellipsis: true,
      width: 190,
      fieldProps: { placeholder: '完整售后单号' },
    },
    {
      // 订单号在这一页是可点的入口：查一笔退款多半要从「这是哪一单」开始。
      title: '订单号',
      dataIndex: 'orderNo',
      copyable: true,
      ellipsis: true,
      width: 190,
      fieldProps: { placeholder: '完整订单号' },
      render: (_, row) => (
        <a onClick={() => history.push(`/orders/${row.orderId}`)}>{row.orderNo}</a>
      ),
    },
    {
      title: '用户 ID',
      dataIndex: 'userId',
      copyable: true,
      ellipsis: true,
      width: 150,
      fieldProps: { placeholder: '完整用户 ID' },
    },
    {
      title: '范围',
      dataIndex: 'scope',
      valueType: 'select',
      valueEnum: searchOptions(AFTER_SALE_SCOPE),
      width: 100,
      render: (_, row) => {
        const meta = enumMeta(AFTER_SALE_SCOPE, row.scope);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      // 按行退时才有一行可指。摆行号 + 商品名：库里存的是 orderLineId，光看 uuid 认不出退的
      // 是哪一杯。
      title: '退的行',
      dataIndex: 'orderLineId',
      search: false,
      width: 200,
      ellipsis: true,
      render: (_, row) =>
        row.orderLine
          ? `第 ${row.orderLine.lineNo} 行 ${row.orderLine.itemName} ×${row.orderLine.quantity}`
          : '—',
    },
    {
      title: '退款金额',
      dataIndex: 'refundAmount',
      search: false,
      width: 100,
      render: (_, row) => formatYuan(row.refundAmount),
    },
    {
      title: '状态',
      dataIndex: 'status',
      valueType: 'select',
      valueEnum: searchOptions(AFTER_SALE_STATUS),
      width: 90,
      render: (_, row) => {
        const meta = enumMeta(AFTER_SALE_STATUS, row.status);
        return <Tag color={meta.color}>{meta.text}</Tag>;
      },
    },
    {
      title: '申请理由',
      dataIndex: 'reason',
      search: false,
      width: 200,
      ellipsis: true,
      render: (_, row) => dash(row.reason),
    },
    {
      title: '凭证',
      dataIndex: 'images',
      search: false,
      width: 100,
      render: (_, row) => {
        const images = row.images ?? [];
        if (images.length === 0) return '—';
        // 行内只摆缩略图，点开才加载大图：凭证常是好几张手机截图，全铺开会把行撑得很高。
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
    {
      title: '承诺福卡',
      dataIndex: 'fortuneCardsExpected',
      search: false,
      width: 100,
      render: (_, row) =>
        row.fortuneCardsExpected > 0 ? (
          <Tooltip title="订单赠送的福卡若已参与抽奖，整笔不可退">
            <Tag color="gold">{row.fortuneCardsExpected} 张</Tag>
          </Tooltip>
        ) : (
          '—'
        ),
    },
    { title: '申请时间', dataIndex: 'createdAt', valueType: 'dateTime', search: false, width: 170 },
    {
      title: '审核人 ID',
      dataIndex: 'reviewedBy',
      search: false,
      width: 180,
      ellipsis: true,
      render: (_, row) => dash(row.reviewedBy),
    },
    { title: '审核时间', dataIndex: 'reviewedAt', valueType: 'dateTime', search: false, width: 170 },
    {
      title: '审核备注',
      dataIndex: 'reviewRemark',
      search: false,
      width: 200,
      ellipsis: true,
      render: (_, row) => dash(row.reviewRemark),
    },
    {
      title: '操作',
      valueType: 'option',
      // 钉右列必须显式给宽度，scroll.x 也要等于各列宽度之和（见下面的 2240）。
      width: 160,
      fixed: 'right',
      render: (_, row) => {
        // 两档判断：有审核权限，且这一张还等着人来审。其余状态点了只会拿到 409。
        if (!access.canReviewAfterSales || row.status !== 'pending') return '—';
        return [
          <Button
            key="approve"
            type="link"
            size="small"
            onClick={() => setReviewing({ sale: row, action: 'approve' })}
          >
            通过申请
          </Button>,
          <Button
            key="reject"
            type="link"
            size="small"
            danger
            onClick={() => setReviewing({ sale: row, action: 'reject' })}
          >
            驳回申请
          </Button>,
        ];
      },
    },
  ];

  return (
    <PageContainer title="退款申请">
      <ProTable<AfterSale>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        // 2300 = 190+190+150+100+200+100+90+200+100+100+170+180+170+200+160，
        // 与各列 width 之和一致。原先这里写的是 2240（少算了 60），实测表格仍按列宽
        // 之和渲染成 2300、固定列位置也对，但留着就是一处对不上的算术。
        scroll={{ x: 2300 }}
        search={{ labelWidth: 'auto' }}
        request={async (params) => {
          const query: AfterSaleQuery = {
            page: params.current,
            pageSize: params.pageSize,
            afterSaleNo: exact(params.afterSaleNo),
            orderNo: exact(params.orderNo),
            userId: exact(params.userId),
            status: exact(params.status) as AfterSaleQuery['status'],
            scope: exact(params.scope) as AfterSaleQuery['scope'],
            createdFrom: toRFC3339(params.createdFrom),
            createdTo: toRFC3339(params.createdTo),
          };
          try {
            const result = await listAfterSales(query);
            return { data: result.items, total: result.total, success: true };
          } catch (error) {
            // 状态/范围写错会被后端挡成 400（它宁可报错也不静默回空列表）。这里把话透出来，
            // 否则表格只是空白，看着像「这个状态下没有申请」。
            message.error(requestErrorMessage(error, '加载退款申请失败'));
            return { data: [], total: 0, success: false };
          }
        }}
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
          actionRef.current?.reload();
        }}
      />
    </PageContainer>
  );
}
