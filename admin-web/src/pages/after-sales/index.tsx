import { PageContainer, ProTable } from '@ant-design/pro-components';
import type { ActionType, ProColumns } from '@ant-design/pro-components';
import { history, useAccess } from '@umijs/max';
import { Button, Image, message, Space, Tag, Tooltip } from 'antd';
import { useRef, useState } from 'react';
import { toRFC3339 } from '../../services/datetime';
import { enumMeta, searchOptions } from '../../services/labels';
import { formatYuan } from '../../services/money';
import {
  listAfterSales,
  startAfterSaleRefund,
  type AfterSale,
  type AfterSaleQuery,
} from '../../services/order';
import { AFTER_SALE_SCOPE, AFTER_SALE_STATUS } from '../../services/orderLabels';
import { requestErrorMessage } from '../../services/requestError';
import ReviewModal from './ReviewModal';

/**
 * 退款申请列表。这一页管的是退款这件事从头到尾：审「要不要退」，以及退到哪一步了。
 *
 * 两者在这张表上是同一行不同列：审核结果看「状态」（通过 / 驳回），退款进度看「退款单号」
 * 与「退款时间」，退不成的原因看「失败原因」。退款单本身在 payment-service，这里显示的
 * 是它回写过来的投影。
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
  // 正在发起退款的那一张（售后单号）。按行记而不是一个布尔：转圈要转在被点的那一行上。
  const [refunding, setRefunding] = useState<string>();

  // 发起退款（重试出口）。分发逻辑见下面「操作」列那段注释：只有 approved 的单会走到这里。
  const onStartRefund = async (row: AfterSale) => {
    setRefunding(row.afterSaleNo);
    try {
      await startAfterSaleRefund(row.afterSaleNo);
      // 说「已发起」而不是「已退款」：这一次调用只把退款单建起来，钱到没到要看支付域回来的
      // 那个结果——它随后会把这一行改成「退款中」或「已退款」。
      message.success('已发起退款，结果稍后回写到这一行');
      actionRef.current?.reload();
    } catch (error) {
      // 失败就停在「已通过」，没别的地方去。把后端的原话透出来——它区分得出「支付服务这次
      // 没答上来」（可以过会儿再点）与「这单根本没有支付单」（点了也没用）。
      message.error(requestErrorMessage(error, '发起退款失败，请稍后重试'));
    } finally {
      setRefunding(undefined);
    }
  };

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
    {
      // 支付域那张退款单的号。退款没发起（还停在「已通过」）时是空的——那一栏空着本身
      // 就是线索：这一笔还卡在「同意退、退款没上路」，可以去点「发起退款」。
      title: '退款单号',
      dataIndex: 'refundNo',
      search: false,
      width: 190,
      ellipsis: true,
      render: (_, row) => dash(row.refundNo),
    },
    {
      // 失败原因：渠道回的原文，配一个码。文案是给人读的（「原交易不存在」），码是给查日志
      // 的人对上的，两个都摆出来——只给码客服读不懂，只给文案开发者对不上流水。
      title: '失败原因',
      dataIndex: 'failureMessage',
      search: false,
      width: 220,
      ellipsis: true,
      render: (_, row) => {
        if (row.status !== 'failed') return '—';
        const text = row.failureMessage || row.failureCode;
        if (!text) return '—';
        return (
          <Tooltip title={row.failureCode ? `失败码：${row.failureCode}` : undefined}>
            <span>{text}</span>
          </Tooltip>
        );
      },
    },
    { title: '退款时间', dataIndex: 'refundedAt', valueType: 'dateTime', search: false, width: 170 },
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
        // 没有审核权限就什么都不能点——两个动作都在同一枚权限码下（见 routes/admin.go）。
        if (!access.canReviewAfterSales) return '—';
        // 待审核：审它（通过 = 同意并当场发起退款）。
        if (row.status === 'pending') {
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
        }
        // 已通过：审核落了库、但那次发起退款没成（支付服务抖了、进程重启）。这不是死胡同，
        // 这一行是唯一能把它推下去的地方。文案「发起退款」而不是「重试退款」：多数情况下
        // 退款单**压根没建成**，说「重试」会让人以为上一次可能已经退过了。
        if (row.status === 'approved') {
          return [
            <Button
              key="refund"
              type="link"
              size="small"
              loading={refunding === row.afterSaleNo}
              onClick={() => onStartRefund(row)}
            >
              发起退款
            </Button>,
          ];
        }
        // refunding / refunded / rejected / failed / cancelled：没有任何可点的动作。
        // 尤其是 failed——退款失败之后不是在这张单上重来，而是用户重新申请（新的一张单）。
        return '—';
      },
    },
  ];

  return (
    <PageContainer title="退款申请">
      <ProTable<AfterSale>
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        // 2880 = 上面 2300 + 退款单号 190 + 失败原因 220 + 退款时间 170，
        // 与各列 width 之和一致。钉右列必须显式给宽度、这一项也必须等于各列之和，否则
        // 右侧固定列会跟着横向滚动错位（见 [[protable-fixed-right-column-width-invariant]]）。
        scroll={{ x: 2880 }}
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
